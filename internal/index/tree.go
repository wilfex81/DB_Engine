package index

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/wilfex81/DB_Engine/internal/buffer"
	"github.com/wilfex81/DB_Engine/internal/storage"
	"github.com/wilfex81/DB_Engine/internal/table"
)

// returned by Insert when the key is already in the tree
var ErrDuplicateKey = errors.New("index: duplicate key")

// an internal signal that a leaf is full. It is never returned
var errSplit = errors.New("index: leaf is full")

// stops a damaged tree from looping forever
const maxDepth = 64

// a slotted page with one 8 byte tuple: a magic and the root
var metaMagic = [4]byte{'P', 'G', 'I', 'X'}

const metaSize = 8

func encodeMeta(root storage.PageID) []byte {
	b := make([]byte, metaSize)
	copy(b, metaMagic[:])
	binary.LittleEndian.PutUint32(b[4:], uint32(root))
	return b
}

func decodeMeta(b []byte) (storage.PageID, error) {
	if len(b) != metaSize || [4]byte(b[:4]) != metaMagic {
		return 0, fmt.Errorf("%w: not a tree meta page", ErrCorrupt)
	}
	return storage.PageID(binary.LittleEndian.Uint32(b[4:])), nil
}

// Tree is a B+ tree from unique int64 keys to RIDs. It needs a buffer pool of
// at least 2 frames. It is safe for concurrent use: Insert is exclusive, and
// Search, Height, and Verify share.
type Tree struct {
	mu          sync.RWMutex
	pool        *buffer.Pool
	meta        storage.PageID
	root        storage.PageID
	maxLeaf     int
	maxInternal int
}

// Option changes how a tree is built
type Option func(*Tree)

// WithNodeSizes makes nodes split ealier than a page forces them to. It is
// for tests: with small nodes, a few dozen keys already make a deep tree. The
// sizes are not sorted in the file. A leaf needs 2 LeafCapacity entries, an
// internal node 2 to InternalCapacity keys.
func WithNodeSizes(leaf, internal int) Option {
	if leaf < 2 || leaf > LeafCapacity || internal < 2 || internal > InternalCapacity {
		panic(fmt.Sprintf("index: bad node sizes %d and %d", leaf, internal))
	}
	return func(t *Tree) { t.maxLeaf, t.maxInternal = leaf, internal }
}

func newTree(pool *buffer.Pool, meta, root storage.PageID, opts []Option) *Tree {
	t := &Tree{pool: pool, meta: meta, root: root, maxLeaf: LeafCapacity, maxInternal: InternalCapacity}
	for _, o := range opts {
		o(t)
	}
	return t
}

// Create starts a new, empty tree: one empty leaf as root, and a meta page.
func Create(pool *buffer.Pool, opts ...Option) (*Tree, error) {
	rootID, p, err := pool.NewPage()
	if err != nil {
		return nil, err
	}
	initLeaf(p)
	if err := pool.Unpin(rootID, true); err != nil {
		return nil, err
	}

	metaID, mp, err := pool.NewPage()
	if err != nil {
		return nil, err
	}
	if _, err := mp.Insert(encodeMeta(rootID)); err != nil {
		_ = pool.Unpin(metaID, false)
		return nil, err
	}
	if err := pool.Unpin(metaID, true); err != nil {
		return nil, err
	}
	return newTree(pool, metaID, rootID, opts), nil
}

// Open attaches to an existing tree by its meta page
func Open(pool *buffer.Pool, metaID storage.PageID, opts ...Option) (*Tree, error) {
	var root storage.PageID
	err := withPage(pool, metaID, false, func(p *storage.Page) error {
		b, err := p.Get(0)
		if err != nil {
			return err
		}
		root, err = decodeMeta(b)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("index: open tree at page %d: %w", metaID, err)
	}
	err = withPage(pool, root, false, func(p *storage.Page) error {
		if k := p.Kind(); k != storage.KindLeaf && k != storage.KindInternal {
			return fmt.Errorf("%w: root page %d si not a node", ErrCorrupt, root)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("index: open tree at page %d: %w", metaID, err)
	}
	return newTree(pool, metaID, root, opts), nil
}

// MetaPage is th epage number that identifies this tree. The catalog stores it.
func (t *Tree) MetaPage() storage.PageID { return t.meta }

// Height is the number of levels, so a tree that is single leaf has height 1.
func (t *Tree) Height() (int, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	path, _, err := t.descend(math.MinInt64)
	if err != nil {
		return 0, err
	}
	return len(path) + 1, nil
}

// Search returns the RID stored for the key k.
func (t *Tree) Search(k int64) (table.RID, bool, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	_, leafID, err := t.descend(k)
	if err != nil {
		return table.RID{}, false, err
	}

	var r table.RID
	var found bool
	err = t.view(leafID, func(p *storage.Page) error {
		l, err := asLeaf(p)
		if err != nil {
			return err
		}
		if i, ok := l.search(k); ok {
			r, found = l.rid(i), true
		}
		return nil
	})
	return r, found, err
}

// step is one internal node on the way down: which node, which child was
// taken, and whether the node was already full
type step struct {
	id   storage.PageID
	idx  int
	full bool
}

// descend walks from the root to the leaf that holds, or would hold, key k.
// Nodes have no parent pointers, so the path comes back with it.
func (t *Tree) descend(k int64) ([]step, storage.PageID, error) {
	var path []step
	id := t.root
	for depth := 0; depth <= maxDepth; depth++ {
		var next storage.PageID
		var isLeaf bool
		err := t.view(id, func(p *storage.Page) error {
			if p.Kind() == storage.KindLeaf {
				isLeaf = true
				return nil
			}
			n, err := asInternal(p)
			if err != nil {
				return err
			}
			i := n.route(k)
			path = append(path, step{id: id, idx: i, full: n.count() >= t.maxInternal})
			next = n.child(i)
			return nil
		})
		if err != nil {
			return nil, 0, err
		}
		if isLeaf {
			return path, id, nil
		}
		id = next
	}
	return nil, 0, fmt.Errorf("%w: the tree is deeper than %d levels", ErrCorrupt, maxDepth)
}

// Insert adds key k with its RID. A key that is already there is an error
func (t *Tree) Insert(k int64, r table.RID) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	path, leafID, err := t.descend(k)
	if err != nil {
		return err
	}

	// the easy case: the leaf has room
	err = t.modify(leafID, func(p *storage.Page) error {
		l, err := asLeaf(p)
		if err != nil {
			return err
		}
		pos, found := l.search(k)
		switch {
		case found:
			return ErrDuplicateKey
		case l.count() >= t.maxLeaf:
			return errSplit
		}
		l.insertAt(pos, k, r)
		return nil
	})
	if !errors.Is(err, errSplit) {
		return err
	}

	//The leaf splits, and so does every full node above it, until a node
	// with room is found. If all of them are full, the root splits too. Count
	// the pages that needs and allocate them all before changing anything, so
	// that running out of frames or disk cannot leave a half done split.
	need := 1 // the new right leaf
	allFull := true
	for i := len(path) - 1; i >= 0; i-- {
		if !path[i].full {
			allFull = false
			break
		}
		need++ // a new rright sibling for each full node
	}
	if allFull {
		need++ // a new root.
	}
	fresh := make([]storage.PageID, 0, need)
	for len(fresh) < need {
		id, _, err := t.pool.NewPage()
		if err != nil {
			return err
		}
		if err := t.pool.Unpin(id, false); err != nil {
			return err
		}
		fresh = append(fresh, id)
	}
	sep, err := t.splitLeaf(leafID, fresh[0], k, r)
	if err != nil {
		return err
	}
	right := fresh[0]
	used := 1
	for i := len(path) - 1; i >= 0; i-- {
		s := path[i]
		if !s.full {
			return t.modify(s.id, func(p *storage.Page) error {
				n, err := asInternal(p)
				if err != nil {
					return err
				}
				n.insertAt(s.idx, sep, right)
				return nil
			})
		}
		sep, err = t.splitInternal(s.id, fresh[used], s.idx, sep, right)
		if err != nil {
			return err
		}
		right = fresh[used]
		used++
	}
	return t.newRoot(fresh[used], t.root, sep, right)
}

// splitLeaf splits a full leaf while adding (k, r). The lower half stays, the
// upper half moves to righID, and the first key of the new leaf is returned
// as the separator. The key is copied up, so it stays in the leaf too.
func (t *Tree) splitLeaf(leafID, rightID storage.PageID, k int64, r table.RID) (int64, error) {
	var sep int64
	err := t.modify(leafID, func(p *storage.Page) error {
		l, err := asLeaf(p)
		if err != nil {
			return err
		}
		n := l.count()
		pos, _ := l.search(k)

		keys := make([]int64, 0, n+1)
		rids := make([]table.RID, 0, n+1)
		for i := 0; i < n; i++ {
			if i == pos {
				keys, rids = append(keys, k), append(rids, r)
			}
			keys, rids = append(keys, l.key(i)), append(rids, l.rid(i))
		}
		if pos == n {
			keys, rids = append(keys, k), append(rids, r)
		}
		oldNext, hasNext := l.next()
		leftN := (len(keys) + 1) / 2
		sep = keys[leftN]

		// the new leaf is written first, so a failure leaves the old one intact
		err = t.modify(rightID, func(rp *storage.Page) error {
			rl := initLeaf(rp)
			for i := leftN; i < len(keys); i++ {
				rl.insertAt(i-leftN, keys[i], rids[i])
			}
			if hasNext {
				rl.setNext(oldNext)
			}
			return nil
		})
		if err != nil {
			return err
		}

		ll := initLeaf(p)
		for i := 0; i < leftN; i++ {
			ll.insertAt(i, keys[i], rids[i])
		}
		ll.setNext(rightID)
		return nil
	})
	return sep, err
}

// splitInternal splits a full internal node while adding a separator with
// child to its right at position idx. The middle key moves up and is returned.
func (t *Tree) splitInternal(nodeID, rightID storage.PageID, idx int, sep int64, child storage.PageID) (int64, error) {
	var up int64
	err := t.modify(nodeID, func(p *storage.Page) error {
		n, err := asInternal(p)
		if err != nil {
			return err
		}
		c := n.count()

		keys := make([]int64, 0, c+1)
		children := make([]storage.PageID, 0, c+2)
		for i := 0; i < c; i++ {
			if i == idx {
				keys = append(keys, sep)
			}
			keys = append(keys, n.key(i))
		}
		if idx == c {
			keys = append(keys, sep)
		}
		for i := 0; i <= c; i++ {
			children = append(children, n.child(i))
			if i == idx {
				children = append(children, child)
			}
		}

		m := len(keys)
		mid := m / 2
		up = keys[mid]

		err = t.modify(rightID, func(rp *storage.Page) error {
			rn := initInternal(rp, children[mid+1])
			for i := mid + 1; i < m; i++ {
				rn.insertAt(i-mid-1, keys[i], children[i+1])
			}
			return nil
		})
		if err != nil {
			return err
		}

		ln := initInternal(p, children[0])
		for i := 0; i < mid; i++ {
			ln.insertAt(i, keys[i], children[i+1])
		}
		return nil
	})
	return up, err
}

// newRoot makes page id the new root, with the old root on the left and right
// on the right of the separator, and records it in the meta page.
func (t *Tree) newRoot(id, left storage.PageID, sep int64, right storage.PageID) error {
	err := t.modify(id, func(p *storage.Page) error {
		n := initInternal(p, left)
		n.insertAt(0, sep, right)
		return nil
	})
	if err != nil {
		return err
	}
	err = t.modify(t.meta, func(p *storage.Page) error { return p.Update(0, encodeMeta(id)) })
	if err != nil {
		return err
	}
	t.root = id
	return nil
}

// Verify checks the whole tree and returns the number of entries. It checks
// that keys are sorted inside every node, that every key lies inside the range
// its parents promises, that all leaves are at the same depth, that no page is reachable twice, and that the leaf
// chain visits every leaf in order.
func (t *Tree) Verify() (int, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	w := &walker{t: t, seen: map[storage.PageID]bool{}, leafDepth: -1}
	if err := w.walk(t.root, 0, 0, 0, false, false); err != nil {
		return 0, err
	}
	for i, lf := range w.leaves {
		last := i == len(w.leaves)-1
		switch {
		case !last && (!lf.hasNext || lf.next != w.leaves[i+1].id):
			return 0, fmt.Errorf("%w: the leaf chain is broken after page %d", ErrCorrupt, lf.id)
		case last && lf.hasNext:
			return 0, fmt.Errorf("%w: the last leaf, page %d, links onward", ErrCorrupt, lf.id)
		}
	}
	return w.entries, nil
}

type leafInfo struct {
	id      storage.PageID
	next    storage.PageID
	hasNext bool
}

type walker struct {
	t         *Tree
	seen      map[storage.PageID]bool
	leaves    []leafInfo // in key order
	leafDepth int
	entries   int
}

// walk checks the subtree at id, whose keys must be >= lo (if hasLo) and < hi (if hasHi).
func (w *walker) walk(id storage.PageID, depth int, lo, hi int64, hasLo, hasHi bool) error {
	if depth > maxDepth {
		return fmt.Errorf("%w: the tree is deeper than %d levels", ErrCorrupt, maxDepth)
	}
	if w.seen[id] {
		return fmt.Errorf("%w: page %d is reachable twice", ErrCorrupt, id)
	}
	w.seen[id] = true

	var keys []int64
	var children []storage.PageID
	var isLeaf bool
	err := w.t.view(id, func(p *storage.Page) error {
		if p.Kind() == storage.KindLeaf {
			l, err := asLeaf(p)
			if err != nil {
				return err
			}
			if err := l.check(); err != nil {
				return fmt.Errorf("page %d: %w", id, err)
			}
			for i := 0; i < l.count(); i++ {
				if k := l.key(i); (hasLo && k < lo) || (hasHi && k >= hi) {
					return fmt.Errorf("%w: page %d holds key %d outside its range", ErrCorrupt, id, k)
				}
			}
			next, hasNext := l.next()
			w.leaves = append(w.leaves, leafInfo{id, next, hasNext})
			w.entries += l.count()
			isLeaf = true
			return nil
		}

		n, err := asInternal(p)
		if err != nil {
			return err
		}
		if err := n.check(); err != nil {
			return fmt.Errorf("page %d: %w", id, err)
		}
		if n.count() == 0 {
			return fmt.Errorf("%w: internal node %d has no keys", ErrCorrupt, id)
		}
		for i := 0; i < n.count(); i++ {
			k := n.key(i)
			if (hasLo && k <= lo) || (hasHi && k >= hi) {
				return fmt.Errorf("%w: page %d holds separator %d outside its range", ErrCorrupt, id, k)
			}
			keys = append(keys, k)
		}
		for i := 0; i <= n.count(); i++ {
			children = append(children, n.child(i))
		}
		return nil
	})
	if err != nil {
		return err
	}

	if isLeaf {
		switch {
		case w.leafDepth < 0:
			w.leafDepth = depth
		case w.leafDepth != depth:
			return fmt.Errorf("%w: leaves are at different depths", ErrCorrupt)
		}
		return nil
	}
	for i, c := range children {
		clo, chi, cHasLo, cHasHi := lo, hi, hasLo, hasHi
		if i > 0 {
			clo, cHasLo = keys[i-1], true
		}
		if i < len(keys) {
			chi, cHasHi = keys[i], true
		}
		if err := w.walk(c, depth+1, clo, chi, cHasLo, cHasHi); err != nil {
			return err
		}
	}
	return nil
}

func (t *Tree) view(id storage.PageID, fn func(*storage.Page) error) error {
	return withPage(t.pool, id, false, fn)
}

// modify marks the page dirty when fn succeeds.
func (t *Tree) modify(id storage.PageID, fn func(*storage.Page) error) error {
	return withPage(t.pool, id, true, fn)
}

func withPage(pool *buffer.Pool, id storage.PageID, write bool, fn func(*storage.Page) error) error {
	p, err := pool.Fetch(id)
	if err != nil {
		return err
	}
	ferr := fn(p)
	uerr := pool.Unpin(id, write && ferr == nil)
	if ferr != nil {
		return ferr
	}
	return uerr
}
