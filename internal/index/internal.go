package index

import (
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/wilfex81/DB_Engine/internal/storage"
)

// internal is a view of a page of kind KindInternal: n sorted keys and n+1
// children. Child i holds the keys below key i, and the last child holds the
// rest. A key equal to a separator belongs to the child on its right.
type internal struct{ p *storage.Page }

// initInternal turns p into a node with no keys and one child.
func initInternal(p *storage.Page, firstChild storage.PageID) internal {
	*p = storage.Page{}
	p.SetKind(storage.KindInternal)
	n := internal{p}
	n.setChild(0, firstChild)
	return n
}

// asInternal views a page as an internal node after the quick checks.
func asInternal(p *storage.Page) (internal, error) {
	if p.Kind() != storage.KindInternal {
		return internal{}, fmt.Errorf("%w: expected an internal node, found page kind %d", ErrCorrupt, p.Kind())
	}
	if n := count(p); n > InternalCapacity {
		return internal{}, fmt.Errorf("%w: node claims %d keys, the limit is %d", ErrCorrupt, n, InternalCapacity)
	}
	return internal{p}, nil
}

func (n internal) count() int      { return count(n.p) }
func (n internal) key(i int) int64 { return key(n.p, i) }

func (n internal) child(i int) storage.PageID {
	return storage.PageID(binary.LittleEndian.Uint32(n.p[childrenAt+i*childSize:]))
}

func (n internal) setChild(i int, id storage.PageID) {
	binary.LittleEndian.PutUint32(n.p[childrenAt+i*childSize:], uint32(id))
}

// route returns which child to follow for key k: the number of keys <= k.
func (n internal) route(k int64) int {
	return sort.Search(n.count(), func(i int) bool { return n.key(i) > k })
}

// insertAt adds separator k at position i, with right as the child on its
// right (child i+1). It is what a split of child i produces. A full node or a
// bad position is a bug and panics.
func (n internal) insertAt(i int, k int64, right storage.PageID) {
	c := n.count()
	if c >= InternalCapacity || i < 0 || i > c {
		panic(fmt.Sprintf("index: internal insertAt(%d) with %d of %d keys", i, c, InternalCapacity))
	}
	shiftUp(n.p, keysAt, keySize, i, c)
	shiftUp(n.p, childrenAt, childSize, i+1, c+1)
	setKey(n.p, i, k)
	n.setChild(i+1, right)
	setCount(n.p, c+1)
}

// check is the full check: keys must be strictly increasing.
func (n internal) check() error {
	for i := 1; i < n.count(); i++ {
		if n.key(i-1) >= n.key(i) {
			return fmt.Errorf("%w: node keys %d and %d are not in order", ErrCorrupt, i-1, i)
		}
	}
	return nil
}