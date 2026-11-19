package buffer

import (
	"container/list"
	"fmt"
	"sync"
)

type FrameID int

type Replacer interface {
	// Removes and returns the frame to evict, or false if none is evictable
	Victim() (FrameID, bool)
	// makes frame not evictable: Pinning a frame that is not tracked is fine
	Pin(FrameID)
	//  Unpinning a frame already tracked changes nothing
	Unpin(FrameID)
	// no. of evictable frames
	Size() int
}

var _ Replacer = (*LRUReplacer)(nil)

// evicts a frame that has been evictable the longest
// it is safe for concurrent use
type LRUReplacer struct {
	mu    sync.Mutex
	order *list.List
	elems []*list.Element
}

// makes a replacer for frames numbered 0 to frames-1.
func NewLRUReplacer(frames int) *LRUReplacer {
	if frames <= 0 {
		panic(fmt.Sprintf("buffer: replacer needs at least one frame, got %d", frames))
	}
	return &LRUReplacer{order: list.New(), elems: make([]*list.Element, frames)}
}

func (r *LRUReplacer) Victim() (FrameID, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.order.Back()
	if e == nil {
		return 0, false
	}
	id := e.Value.(FrameID)
	r.order.Remove(e)
	r.elems[id] = nil
	return id, true
}

func (r *LRUReplacer) Pin(id FrameID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.check(id)
	if e := r.elems[id]; e != nil {
		r.order.Remove(e)
		r.elems[id] = nil
	}
}

func (r *LRUReplacer) Unpin(id FrameID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.check(id)
	if r.elems[id] != nil {
		return // already evicted, keeps its place in line
	}
	r.elems[id] = r.order.PushFront(id)
}

func (r *LRUReplacer) Size() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.order.Len()
}

// lists the evictable frames, the next victim first.
// It is meant for tets and for the inspector.
func (r *LRUReplacer) Evictable() []FrameID {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]FrameID, 0, r.order.Len())
	for e := r.order.Back(); e != nil; e = e.Prev() {
		out = append(out, e.Value.(FrameID))
	}
	return out
}

// check must be called with the lock held
// A frame number that does not exist
// is a bug in the caller, so we panic.
func (r *LRUReplacer) check(id FrameID) {
	if id < 0 || int(id) >= len(r.elems) {
		panic(fmt.Sprintf("buffer: frame %d is out of range [0 to %d)", id, len(r.elems)-1))
	}
}
