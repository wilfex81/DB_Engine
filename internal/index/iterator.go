package index

import (
	"fmt"
	"math"

	"github.com/wilfex81/DB_Engine/internal/storage"
	"github.com/wilfex81/DB_Engine/internal/table"
)

// Iterator walks keys in ascending order
//
//		it := tr.Range(10, 20)
//		for it.Next() {
//			use(it.Key(), it.RID())
//		}
//		if err := it.Err(); err != nil { ... }
//
//	 It pins no page between calls, so it needs no Close. It is not a snapshot:
//
// keys inserted or deleted during the scan may or may not be seen, but a key
// is never returned twise, and the keys always come out in ascending order.
type Iterator struct {
	t       *Tree
	lo, hi  int64
	started bool // false until the first Next has found the first leaf
	leaf    storage.PageID
	last    int64 // the last key returned
	hasLast bool
	done    bool
	k       int64
	r       table.RID
	err     error
}

// Range returns the key from lo to hi, both included.
func (t *Tree) Range(lo, hi int64) *Iterator { return &Iterator{t: t, lo: lo, hi: hi} }

// Seek returns every key from lo upwards
func (t *Tree) Seek(lo int64) *Iterator { return t.Range(lo, math.MaxInt64) }

// Scan returns every key.
func (t *Tree) Scan() *Iterator { return t.Range(math.MinInt64, math.MaxInt64) }

// Next moves to the next key. It returns false at the end or an error, and
// Err tells which.
func (it *Iterator) Next() bool {
	if it.done {
		return false
	}
	t := it.t
	t.mu.RLock()
	defer t.mu.RUnlock()

	if !it.started {
		if it.lo > it.hi {
			it.done = true
			return false
		}
		_, leaf, err := t.descend(it.lo)
		if err != nil {
			return it.fail(err)
		}
		it.leaf, it.started = leaf, true
	}

	// Look for the first key after the last one returned, in this leaf, and
	// in the next leaves if this one has none. Empty leaves are skipped. The
	// hop limit stops a damaged chain that loops
	for hops := 0; ; hops++ {
		if hops > t.pool.NumPages() {
			return it.fail(fmt.Errorf("%w: the leaf chain never ends", ErrCorrupt))
		}
		var found, hasNext bool
		var next storage.PageID
		err := t.view(it.leaf, func(p *storage.Page) error {
			l, err := asLeaf(p)
			if err != nil {
				return err
			}
			var pos int
			if it.hasLast {
				i, ok := l.search(it.last)
				pos = i
				if ok {
					pos++
				}
			} else {
				pos, _ = l.search(it.lo)
			}
			if pos < l.count() {
				it.k, it.r, found = l.key(pos), l.rid(pos), true
				return nil
			}
			next, hasNext = l.next()
			return nil
		})
		if err != nil {
			return it.fail(err)
		}
		if found {
			if it.k > it.hi {
				it.done = true
				return false
			}
			it.last, it.hasLast = it.k, true
			return true
		}
		if !hasNext {
			it.done = true
			return false
		}
		it.leaf = next
	}
}

func (it *Iterator) fail(err error) bool {
	it.err, it.done = err, true
	return false
}

// Key is the current key. Call it only after Next returned true.
func (it *Iterator) Key() int64 { return it.k }

// RID is the RID of the current key. Call it only after Next returned true.
func (it *Iterator) RID() table.RID { return it.r }

// Err is the error that stopped the scan, or nil
func (it *Iterator) Err() error { return it.err }
