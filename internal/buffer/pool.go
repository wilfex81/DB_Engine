package buffer

import (
	"errors"
	"fmt"
	"sync"

	"github.com/wilfex81/DB_Engine/internal/storage"
)

var (
	ErrNoFrames    = errors.New("buffer: every frame is pinned")
	ErrNotResident = errors.New("buffer: page is not in the pool")
	ErrNotPinned   = errors.New("buffer: page is not pinned")
)

type frame struct {
	page  storage.Page
	id    storage.PageID
	used  bool // holds a page
	pins  int
	dirty bool
}

// FrameInfo describes one frame, for tests and the inspector.
type FrameInfo struct {
	Frame FrameID
	Used  bool
	Page  storage.PageID // meaningful only when Used
	Pins  int
	Dirty bool
}

// Pool keeps a fixed number of pages in memory over a DiskManager.
// It is safe for concurrent use. The pool protects its frames, not the bytes
// inside a page: two goroutines changing the same page must be ordered by
// the caller.
type Pool struct {
	mu     sync.Mutex
	dm     *storage.DiskManager
	frames []frame
	table  map[storage.PageID]FrameID
	free   []FrameID // frames that hold no page, used as a stack
	repl   Replacer
}

// NewPool makes a pool of the given number of frames. The pool does not own
// the disk manager, so the caller closes it, after FlushAll.
func NewPool(dm *storage.DiskManager, frames int) *Pool {
	if dm == nil || frames <= 0 {
		panic(fmt.Sprintf("buffer: bad pool arguments (disk manager nil: %v, frames: %d)", dm == nil, frames))
	}
	p := &Pool{
		dm:     dm,
		frames: make([]frame, frames),
		table:  make(map[storage.PageID]FrameID, frames),
		free:   make([]FrameID, 0, frames),
		repl:   NewLRUReplacer(frames),
	}
	for i := frames - 1; i >= 0; i-- { // so frame 0 is handed out first
		p.free = append(p.free, FrameID(i))
	}
	return p
}

// Fetch returns page id, pinned. Every Fetch needs one Unpin. The returned
// pointer is valid until that Unpin, and not after: the frame may then hold
// another page.
func (p *Pool) Fetch(id storage.PageID) (*storage.Page, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if fid, ok := p.table[id]; ok {
		f := &p.frames[fid]
		f.pins++
		p.repl.Pin(fid)
		return &f.page, nil
	}

	fid, err := p.takeFrame()
	if err != nil {
		return nil, err
	}
	f := &p.frames[fid]
	if err := p.dm.ReadPage(id, &f.page); err != nil {
		p.release(fid)
		return nil, err
	}
	p.install(fid, id)
	return &f.page, nil
}

// NewPage adds an empty page to the file and returns it pinned.
func (p *Pool) NewPage() (storage.PageID, *storage.Page, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	fid, err := p.takeFrame() // first, so a full pool does not grow the file
	if err != nil {
		return 0, nil, err
	}
	id, err := p.dm.AllocatePage()
	if err != nil {
		p.release(fid)
		return 0, nil, err
	}
	f := &p.frames[fid]
	f.page.Init() // the file already holds this same empty page
	p.install(fid, id)
	return id, &f.page, nil
}

// Unpin gives back one pin. Pass dirty as true if you changed the page. The
// flag only ever turns on here: a later Unpin(false) does not undo it.
func (p *Pool) Unpin(id storage.PageID, dirty bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	fid, ok := p.table[id]
	if !ok {
		return fmt.Errorf("%w: %d", ErrNotResident, id)
	}
	f := &p.frames[fid]
	if f.pins == 0 {
		return fmt.Errorf("%w: %d", ErrNotPinned, id)
	}
	if dirty {
		f.dirty = true
	}
	f.pins--
	if f.pins == 0 {
		p.repl.Unpin(fid)
	}
	return nil
}

// Flush writes page id to the file if it is dirty. It does not sync.
func (p *Pool) Flush(id storage.PageID) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	fid, ok := p.table[id]
	if !ok {
		return fmt.Errorf("%w: %d", ErrNotResident, id)
	}
	return p.flush(fid)
}

// FlushAll writes every dirty page and then syncs the file.
func (p *Pool) FlushAll() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	for i := range p.frames {
		if p.frames[i].used {
			if err := p.flush(FrameID(i)); err != nil {
				return err
			}
		}
	}
	return p.dm.Sync()
}

// Frames reports the state of every frame, in frame order.
func (p *Pool) Frames() []FrameInfo {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]FrameInfo, len(p.frames))
	for i := range p.frames {
		f := &p.frames[i]
		out[i] = FrameInfo{Frame: FrameID(i), Used: f.used, Pins: f.pins, Dirty: f.dirty}
		if f.used {
			out[i].Page = f.id
		}
	}
	return out
}

// takeFrame returns an empty frame: a free one, or else an evicted one. An
// evicted dirty page is written first. All of these must be called with the
// lock held.
func (p *Pool) takeFrame() (FrameID, error) {
	if n := len(p.free); n > 0 {
		fid := p.free[n-1]
		p.free = p.free[:n-1]
		return fid, nil
	}

	fid, ok := p.repl.Victim()
	if !ok {
		return 0, ErrNoFrames
	}
	f := &p.frames[fid]
	if err := p.flush(fid); err != nil {
		p.repl.Unpin(fid) // still evictable, and its page stays in memory
		return 0, fmt.Errorf("buffer: evict page %d: %w", f.id, err)
	}
	delete(p.table, f.id)
	f.used, f.pins, f.dirty = false, 0, false
	return fid, nil
}

// release puts an empty frame back on the free stack.
func (p *Pool) release(fid FrameID) {
	f := &p.frames[fid]
	f.used, f.pins, f.dirty = false, 0, false
	p.free = append(p.free, fid)
}

// install makes a frame hold page id, pinned once. The frame is in neither
// the free stack nor the replacer at this point.
func (p *Pool) install(fid FrameID, id storage.PageID) {
	f := &p.frames[fid]
	f.id, f.used, f.pins, f.dirty = id, true, 1, false
	p.table[id] = fid
}

func (p *Pool) flush(fid FrameID) error {
	f := &p.frames[fid]
	if !f.dirty {
		return nil
	}
	if err := p.dm.WritePage(f.id, &f.page); err != nil {
		return err
	}
	f.dirty = false
	return nil
}

// NumPages is the number of pages in the file.
func (p *Pool) NumPages() int { return p.dm.NumPages() }
