package index

import (
	"encoding/binary"
	"fmt"

	"github.com/wilfex81/DB_Engine/internal/storage"
	"github.com/wilfex81/DB_Engine/internal/table"
)

// leaf is a view of a page of kind KindLeaf: sorted keys, and the RID of the
// row for each key. Leaves are linked through the page's Next link, in key
// order, for range scans.
type leaf struct {
	p *storage.Page
}

// initLeaf turns p into an empty leaf
func initLeaf(p *storage.Page) leaf {
	*p = storage.Page{}
	p.SetKind(storage.KindLeaf)
	return leaf{p}
}

// asLeaf views a page as a leaf after the quick checks. Use check for a full one
func asLeaf(p *storage.Page) (leaf, error) {
	if p.Kind() != storage.KindLeaf {
		return leaf{}, fmt.Errorf("%w: expected a leaf, found page kind %d", ErrCorrupt, p.Kind())
	}
	if n := count(p); n > LeafCapacity {
		return leaf{}, fmt.Errorf("%w: leaf claims %d entries, the limit is %d", ErrCorrupt, n, LeafCapacity)
	}
	return leaf{p}, nil
}

func (l leaf) count() int      { return count(l.p) }
func (l leaf) key(i int) int64 { return key(l.p, i) }
func (l leaf) rid(i int) table.RID {
	at := leafRIDsAt + i*ridSize
	return table.RID{
		Page: storage.PageID(binary.LittleEndian.Uint32(l.p[at:])),
		Slot: binary.LittleEndian.Uint16(l.p[at+4:]),
	}
}

func (l leaf) setRID(i int, r table.RID) {
	at := leafRIDsAt + i*ridSize
	binary.LittleEndian.PutUint32(l.p[at:], uint32(r.Page))
	binary.LittleEndian.PutUint16(l.p[at+4:], r.Slot)
}

func (l leaf) next() (storage.PageID, bool) { return l.p.Next() }
func (l leaf) setNext(id storage.PageID)    { l.p.SetNext(id) }

// search returns the first position whose key is >= k, and whether that key is k.
func (l leaf) search(k int64) (int, bool) {
	n := l.count()
	i := sort.Search(n, func(i int) bool { return l.key(i) >= k })
	return i, i < n && l.key(i) == k
}

// insertAt puts an entry at position i and moves the later ones up. The caller
// keeps the keys sorted and makes sure there is room, so a full leaf or a bad
// position is a bug and panics.
func (l leaf) insertAt(i int, k int64, r table.RID) {
	n := l.count()
	if n >= LeafCapacity || i < 0 || i > n {
		panic(fmt.Sprintf("index: leaf insertAt(%d) with %d of %d entries", i, n, LeafCapacity))
	}
	shiftUp(l.p, keysAt, keySize, i, n)
	shiftUp(l.p, leafRIDsAt, ridSize, i, n)
	setKey(l.p, i, k)
	l.setRID(i, r)
	setCount(l.p, n+1)
}

// removeAt deletes the entry at position i and moves the later ones down.
func (l leaf) removeAt(i int) {
	n := l.count()
	if i < 0 || i >= n {
		panic(fmt.Sprintf("index: leaf removeAt(%d) with %d entries", i, n))
	}
	shiftDown(l.p, keysAt, keySize, i, n)
	shiftDown(l.p, leafRIDsAt, ridSize, i, n)
	setCount(l.p, n-1)
}

// check is the full check: keys must be strictly increasing.
func (l leaf) check() error {
	for i := 1; i < l.count(); i++ {
		if l.key(i-1) >= l.key(i) {
			return fmt.Errorf("%w: leaf keys %d and %d are not in order", ErrCorrupt, i-1, i)
		}
	}
	return nil
}