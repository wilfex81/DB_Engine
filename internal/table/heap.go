package table

import (
	"errors"
	"fmt"
	"sync"

	"github.com/wilfex81/DB_Engine/internal/buffer"
	"github.com/wilfex81/DB_Engine/internal/storage"
	"github.com/wilfex81/DB_Engine/internal/types"
)

var ErrCorrupt = errors.New("table: corrupt heap")

// Stores the rows of one table. Its pages form a linked list, and the
// first page is the identity of the table: the catalog will store it.
//
// A Heap is safe for concurrent use. Writes are exclusive and reads share,
// and this lock also protects the page bytes, which the buffer pool does not.
type Heap struct {
	mu     sync.RWMutex
	pool   *buffer.Pool
	schema *types.Schema
	first  storage.PageID
	last   storage.PageID
}

// Creates starts a new, empty heap with one page.
func Create(pool *buffer.Pool, schema *types.Schema) (*Heap, error) {
	id, _, err := pool.NewPage()
	if err != nil {
		return nil, err
	}
	// the file already holds this same empty page, so it is not dirty
	if err := pool.Unpin(id, false); err != nil {
		return nil, err
	}
	return &Heap{pool: pool, schema: schema, first: id, last: id}, nil
}

// attaches to an existing heap and finds its tail by walking the chain
func Open(pool *buffer.Pool, schema *types.Schema, first storage.PageID) (*Heap, error) {
	h := &Heap{pool: pool, schema: schema, first: first, last: first}
	id := first
	for hops := 0; ; hops++ {
		if hops > pool.NumPages() {
			return nil, fmt.Errorf("%w: the chain from page %d never ends", ErrCorrupt, first)
		}
		var next storage.PageID
		var ok bool
		err := h.view(id, func(p *storage.Page) error {
			next, ok = p.Next()
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("table: open heap at page %d: %w", first, err)
		}
		h.last = id
		if !ok {
			return h, nil
		}
		id = next
	}
}

// this is the page n0. that identifies this heap
func (h *Heap) FirstPage() storage.PageID { return h.first }

// schema of the rows
func (h *Heap) Schema() *types.Schema { return h.schema }

// stores a row and returns its RID
func (h *Heap) Insert(vals []types.Value) (RID, error) {
	data, err := h.schema.Encode(vals)
	if err != nil {
		return RID{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.insert(data)
}

// Returns a row at rid
func (h *Heap) Get(rid RID) ([]types.Value, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var vals []types.Value
	err := h.view(rid.Page, func(p *storage.Page) error {
		b, err := p.Get(int(rid.Slot))
		if err != nil {
			return err
		}
		vals, err = h.schema.Decode(b)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("table: get %s: %w", rid, err)
	}
	return vals, nil

}

// removes the row at rid
func (h *Heap) Delete(rid RID) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	err := h.modify(rid.Page, func(p *storage.Page) error {
		return p.Delete(int(rid.Slot))
	})
	if err != nil {
		return fmt.Errorf("table: delete %s: %w", rid, err)
	}
	return nil
}

// Update replaces the row at rid and returns its RID. Normally that is the
// same RID. If the new row no longer fits in the page, the row moves and the
// RID changes, so anything that stored the old RID (an index) must use the
// returned one. On error the old row is unchanged.
func (h *Heap) Update(rid RID, vals []types.Value) (RID, error) {
	data, err := h.schema.Encode(vals)
	if err != nil {
		return RID{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	err = h.modify(rid.Page, func(p *storage.Page) error { return p.Update(int(rid.Slot), data) })
	if err == nil {
		return rid, nil
	}
	if !errors.Is(err, storage.ErrNoSpace) {
		return RID{}, fmt.Errorf("table: update %s: %w", rid, err)
	}

	moved, err := h.insert(data) // the old row is still in place if this fails
	if err != nil {
		return RID{}, fmt.Errorf("table: move %s: %w", rid, err)
	}
	err = h.modify(rid.Page, func(p *storage.Page) error { return p.Delete(int(rid.Slot)) })
	if err != nil {
		return RID{}, fmt.Errorf("table: row %s moved to %s but the old copy was not deleted: %w", rid, moved, err)
	}
	return moved, nil
}

// insert must be called with the write lock held.
func (h *Heap) insert(data []byte) (RID, error) {
	var slot int
	err := h.modify(h.last, func(p *storage.Page) (err error) {
		slot, err = p.Insert(data)
		return err
	})
	if err == nil {
		return RID{h.last, uint16(slot)}, nil
	}
	if !errors.Is(err, storage.ErrNoSpace) {
		return RID{}, err
	}

	// The tail is full: add a page, fill it, and only then link it in.
	// A failure in between leaves an unlinked page, which does no harm.
	id, p, err := h.pool.NewPage()
	if err != nil {
		return RID{}, err
	}
	slot, err = p.Insert(data) // an empty page always has room for a valid tuple
	if err != nil {
		_ = h.pool.Unpin(id, false)
		return RID{}, err
	}
	if err := h.pool.Unpin(id, true); err != nil {
		return RID{}, err
	}
	err = h.modify(h.last, func(p *storage.Page) error {
		p.SetNext(id)
		return nil
	})
	if err != nil {
		return RID{}, err
	}
	h.last = id
	return RID{id, uint16(slot)}, nil
}

func (h *Heap) view(id storage.PageID, fn func(*storage.Page) error) error {
	return h.with(id, false, fn)
}

// modify marks the page dirty when fn succeeds. A failed page operation
// leaves the page unchanged, so it is not marked.
func (h *Heap) modify(id storage.PageID, fn func(*storage.Page) error) error {
	return h.with(id, true, fn)
}

func (h *Heap) with(id storage.PageID, write bool, fn func(*storage.Page) error) error {
	p, err := h.pool.Fetch(id)
	if err != nil {
		return err
	}
	ferr := fn(p)
	uerr := h.pool.Unpin(id, write && ferr == nil)
	if ferr != nil {
		return ferr
	}
	return uerr
}
