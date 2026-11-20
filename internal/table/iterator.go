package table

import (
	"errors"
	"fmt"

	"github.com/wilfex81/DB_Engine/internal/storage"
	"github.com/wilfex81/DB_Engine/internal/types"
)

// Iterator walks every row of a heap in page order, then slot order.
//
//	it := h.Scan()
//	for it.Next() {
//		use(it.RID(), it.Values())
//	}
//	if err := it.Err(); err != nil { ... }
//
// It pins no page between calls, so it needs no Close, and abandoning it
// leaks nothing. It is not a snapshot: rows added or removed during the scan
// may or may not be seen.
type Iterator struct {
	h    *Heap
	page storage.PageID
	slot int
	hops int
	done bool
	rid  RID
	vals []types.Value
	err  error
}

// Scan starts an iterator at the first row.
func (h *Heap) Scan() *Iterator { return &Iterator{h: h, page: h.first} }

// Next moves to the next row. It returns false at the end or on an error,
// and Err tells which.
func (it *Iterator) Next() bool {
	if it.done {
		return false
	}
	it.h.mu.RLock()
	defer it.h.mu.RUnlock()

	for {
		var found, hasNext bool
		var next storage.PageID
		err := it.h.view(it.page, func(p *storage.Page) error {
			for it.slot < p.SlotCount() {
				s := it.slot
				it.slot++
				b, err := p.Get(s)
				if errors.Is(err, storage.ErrSlotEmpty) {
					continue
				}
				if err != nil {
					return err
				}
				vals, err := it.h.schema.Decode(b)
				if err != nil {
					return fmt.Errorf("page %d slot %d: %w", it.page, s, err)
				}
				it.rid, it.vals, found = RID{it.page, uint16(s)}, vals, true
				return nil
			}
			next, hasNext = p.Next()
			return nil
		})
		if err != nil {
			return it.fail(err)
		}
		if found {
			return true
		}
		if !hasNext {
			it.done = true
			return false
		}
		if it.hops++; it.hops > it.h.pool.NumPages() {
			return it.fail(fmt.Errorf("%w: the chain from page %d never ends", ErrCorrupt, it.h.first))
		}
		it.page, it.slot = next, 0
	}
}

func (it *Iterator) fail(err error) bool {
	it.err, it.done = err, true
	return false
}

// RID is the address of the current row. Call it only after Next returned true.
func (it *Iterator) RID() RID { return it.rid }

// Values is the current row. Call it only after Next returned true.
func (it *Iterator) Values() []types.Value { return it.vals }

// Err is the error that stopped the scan, or nil.
func (it *Iterator) Err() error { return it.err }
