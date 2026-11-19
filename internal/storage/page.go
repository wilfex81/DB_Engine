// Holds the on-disk building blocks: the slotted page and
// later, the disk manager
package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

const (
	PageSize   = 4096
	headerSize = 16 // slot count (2), free end (2), 12 bytes reserved (the WAL will use them)
	slotSize   = 4  // tuple offset (2), tuple length (2)

	// MaxTupleSize is the largest tuple one page can hold: a page with one slot.
	MaxTupleSize = PageSize - headerSize - slotSize
)

var (
	ErrNoSpace       = errors.New("storage: not enough room in the page")
	ErrTupleTooLarge = errors.New("storage: tuple is larger than a page can hold")
	ErrEmptyTuple    = errors.New("storage: tuple has no bytes")
	ErrInvalidSlot   = errors.New("storage: slot numbers out of range")
	ErrSlotEmpty     = errors.New("storage: slot holds no tuples")
	ErrCorrupt       = errors.New("storage: corrupt storage")
)

// Page is one 4 KiB block. It knows nothing about schemas: it stores byte
// strings and hands out slot numbers. A zero Page is not usable until Init
// is called, and a page read from disk must pass Validate before any other use.
//
// Header:  [0:2] slot count, [2:4] free end, [4:16] reserved.
// Slot i:  at 16+4*i, offset then length. A deleted slot has offset 0 and
// length 0. Offset 0 is inside the header, so no tuple can start there.
type Page [PageSize]byte

// Init makes p an empty page.
func (p *Page) Init() {
	*p = Page{}
	p.putU16(2, PageSize)
}

// returns the whole page, for the disk manager to read into and write from
func (p *Page) Bytes() []byte { return p[:] }

// no. of slots, deleted ones included
func (p *Page) SlotCount() int { return p.u16(0) }

// size of the largest tuple Insert would accept right now
func (p *Page) FreeSpace() int {
	free := p.totalFree()
	if p.firstDeleted() < 0 {
		free -= slotSize
	}
	if free < 0 {
		return 0
	}
	return free
}

// Insert stores a copy of data and returns its slot number. A deleted slot is
// reused when there is one. If the free space is split into pieces, the page
// is compacted first. On error the page is unchanged.
func (p *Page) Insert(data []byte) (int, error) {
	if err := checkLen(len(data)); err != nil {
		return 0, err
	}
	slot := p.firstDeleted()
	need := len(data)
	if slot < 0 {
		need += slotSize
	}
	if p.totalFree() < need {
		return 0, ErrNoSpace
	}
	if p.contiguous() < need {
		p.Compact()
	}
	if slot < 0 {
		slot = p.SlotCount()
		p.putU16(0, slot+1)
	}
	p.place(slot, data)
	return slot, nil
}

// Get returns the tuple in a slot. The slice points into the page and is only
// valid until the next change to the page, so decode it right away. Its
// capacity is limited, so an append on it cannot overwrite a neighbour.
func (p *Page) Get(slot int) ([]byte, error) {
	off, n, err := p.live(slot)
	if err != nil {
		return nil, err
	}
	return p[off : off+n : off+n], nil
}

// The space freed here comes back when the next page is compacted
func (p *Page) Delete(slot int) error {
	off, n, err := p.live(slot)
	if err != nil {
		return err
	}
	zero(p[off : off+n])
	p.setSlot(slot, 0, 0)
	return nil
}

// Update replaces the tuple in a slot and keeps the slot number, so a RID
// stays valid. On error the page is unchanged. data must not be a slice
// returned by Get on this same page.
func (p *Page) Update(slot int, data []byte) error {
	if err := checkLen(len(data)); err != nil {
		return err
	}
	off, n, err := p.live(slot)
	if err != nil {
		return err
	}

	if len(data) <= n { // fits where it is
		copy(p[off:], data)
		zero(p[off+len(data) : off+n])
		p.setSlot(slot, off, len(data))
		return nil
	}

	if p.totalFree()+n < len(data) { // the old copy is freed, so it counts
		return ErrNoSpace
	}
	zero(p[off : off+n])
	p.setSlot(slot, 0, 0)
	if p.contiguous() < len(data) {
		p.Compact()
	}
	p.place(slot, data)
	return nil
}

// Compact packs all tuples against the end of the page, so the free space
// becomes one block. Slot numbers do not change.
func (p *Page) Compact() {
	var tmp [PageSize]byte
	end := PageSize
	for i := 0; i < p.SlotCount(); i++ {
		off, n := p.slot(i)
		if off == 0 {
			continue
		}
		end -= n
		copy(tmp[end:], p[off:off+n])
		p.setSlot(i, end, n)
	}
	copy(p[end:], tmp[end:])
	zero(p[p.dirEnd():end])
	p.putU16(2, end)
}

// Validate checks that a page is well formed. It trusts nothing, because the
// bytes may come from a damaged disk. After it passes, no operation on the
// page can read outside it.
func (p *Page) Validate() error {
	n := p.SlotCount()
	dirEnd := headerSize + n*slotSize
	fe := p.freeEnd()
	if fe > PageSize || dirEnd > fe {
		return fmt.Errorf("%w: %d slots and free end %d do not fit", ErrCorrupt, n, fe)
	}

	type span struct{ start, end, slot int }
	spans := make([]span, 0, n)
	for i := 0; i < n; i++ {
		off, l := p.slot(i)
		if off == 0 {
			if l != 0 {
				return fmt.Errorf("%w: deleted slot %d has a length", ErrCorrupt, i)
			}
			continue
		}
		if l == 0 {
			return fmt.Errorf("%w: slot %d holds a tuple of length 0", ErrCorrupt, i)
		}
		if off < fe || off+l > PageSize {
			return fmt.Errorf("%w: slot %d points outside the tuple area", ErrCorrupt, i)
		}
		spans = append(spans, span{off, off + l, i})
	}
	sort.Slice(spans, func(a, b int) bool { return spans[a].start < spans[b].start })
	for i := 1; i < len(spans); i++ {
		if spans[i].start < spans[i-1].end {
			return fmt.Errorf("%w: slots %d and %d overlap", ErrCorrupt, spans[i-1].slot, spans[i].slot)
		}
	}
	return nil
}

func checkLen(n int) error {
	switch {
	case n == 0:
		return ErrEmptyTuple
	case n > MaxTupleSize:
		return fmt.Errorf("%w (%d bytes, the limit is %d)", ErrTupleTooLarge, n, MaxTupleSize)
	}
	return nil
}

// returns where the tuple in a slot is.
func (p *Page) live(slot int) (off, n int, err error) {
	if slot < 0 || slot >= p.SlotCount() {
		return 0, 0, fmt.Errorf("%w: %d", ErrInvalidSlot, slot)
	}
	off, n = p.slot(slot)
	if off == 0 {
		return 0, 0, fmt.Errorf("%w: %d", ErrSlotEmpty, slot)
	}
	return off, n, nil
}

// writes data just below the free end and points the slot at it.
// The caller has already made sure there is room
func (p *Page) place(slot int, data []byte) {
	end := p.freeEnd() - len(data)
	copy(p[end:], data)
	p.setSlot(slot, end, len(data))
	p.putU16(2, end)
}

func (p *Page) firstDeleted() int {
	for i := 0; i < p.SlotCount(); i++ {
		if off, _ := p.slot(i); off == 0 {
			return i
		}
	}
	return -1
}

func (p *Page) liveBytes() int {
	total := 0
	for i := 0; i < p.SlotCount(); i++ {
		if off, n := p.slot(i); off != 0 {
			total += n
		}
	}
	return total
}

func (p *Page) freeEnd() int    { return p.u16(2) }
func (p *Page) dirEnd() int     { return headerSize + p.SlotCount()*slotSize }
func (p *Page) contiguous() int { return p.freeEnd() - p.dirEnd() }
func (p *Page) totalFree() int  { return PageSize - p.dirEnd() - p.liveBytes() }

func (p *Page) slot(i int) (off, n int) {
	base := headerSize + i*slotSize
	return p.u16(base), p.u16(base + 2)
}

func (p *Page) setSlot(i, off, n int) {
	base := headerSize + i*slotSize
	p.putU16(base, off)
	p.putU16(base+2, n)
}

func (p *Page) u16(at int) int   { return int(binary.LittleEndian.Uint16(p[at:])) }
func (p *Page) putU16(at, v int) { binary.LittleEndian.PutUint16(p[at:], uint16(v)) }

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
