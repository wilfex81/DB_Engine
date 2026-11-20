package index

import (
	"math"
	"math/rand"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wilfex81/DB_Engine/internal/storage"
	"github.com/wilfex81/DB_Engine/internal/table"
)

func newLeaf() leaf                             { return initLeaf(new(storage.Page)) }
func newInternal(first storage.PageID) internal { return initInternal(new(storage.Page), first) }
func rid(p storage.PageID, s uint16) table.RID  { return table.RID{Page: p, Slot: s} }

// put inserts in key order, the way the tree will.
func put(tb testing.TB, l leaf, k int64, r table.RID) {
	tb.Helper()
	i, found := l.search(k)
	require.False(tb, found, "key %d is already there", k)
	l.insertAt(i, k, r)
}

func leafKeys(l leaf) []int64 {
	out := make([]int64, l.count())
	for i := range out {
		out[i] = l.key(i)
	}
	return out
}

func TestCapacities(t *testing.T) {
	assert.Equal(t, 291, LeafCapacity)
	assert.Equal(t, 339, InternalCapacity)
}

func TestLeafSearch(t *testing.T) {
	l := newLeaf()
	for _, k := range []int64{10, 20, 30} {
		put(t, l, k, rid(1, 1))
	}
	tests := []struct {
		key       int64
		wantPos   int
		wantFound bool
	}{
		{math.MinInt64, 0, false},
		{5, 0, false},
		{10, 0, true},
		{15, 1, false},
		{20, 1, true},
		{30, 2, true},
		{35, 3, false},
		{math.MaxInt64, 3, false},
	}
	for _, tc := range tests {
		pos, found := l.search(tc.key)
		assert.Equal(t, tc.wantPos, pos, "key %d", tc.key)
		assert.Equal(t, tc.wantFound, found, "key %d", tc.key)
	}
}

func TestLeafFillsToCapacityInAnyOrder(t *testing.T) {
	l := newLeaf()
	for _, k := range rand.New(rand.NewSource(1)).Perm(LeafCapacity) {
		put(t, l, int64(k), rid(storage.PageID(k), uint16(k)))
	}
	require.Equal(t, LeafCapacity, l.count())
	require.NoError(t, l.check())
	for i := 0; i < LeafCapacity; i++ {
		assert.Equal(t, int64(i), l.key(i))
		assert.Equal(t, rid(storage.PageID(i), uint16(i)), l.rid(i))
	}
	assert.Panics(t, func() { l.insertAt(0, -1, rid(0, 0)) }, "a full leaf must not take more")
}

func TestLeafKeepsExtremeValues(t *testing.T) {
	l := newLeaf()
	big := rid(storage.PageID(math.MaxUint32), math.MaxUint16)
	for _, k := range []int64{math.MaxInt64, 0, math.MinInt64, -1} {
		put(t, l, k, big)
	}
	assert.Equal(t, []int64{math.MinInt64, -1, 0, math.MaxInt64}, leafKeys(l))
	for i := 0; i < l.count(); i++ {
		assert.Equal(t, big, l.rid(i))
	}
	assert.NoError(t, l.check())
}

func TestLeafRemove(t *testing.T) {
	l := newLeaf()
	for _, k := range []int64{1, 2, 3, 4, 5} {
		put(t, l, k, rid(storage.PageID(k), 0))
	}
	l.removeAt(2)
	assert.Equal(t, []int64{1, 2, 4, 5}, leafKeys(l))
	assert.Equal(t, rid(4, 0), l.rid(2))
	l.removeAt(0)
	l.removeAt(2)
	assert.Equal(t, []int64{2, 4}, leafKeys(l))

	assert.Panics(t, func() { l.removeAt(2) })
	assert.Panics(t, func() { l.removeAt(-1) })

	l.removeAt(0)
	l.removeAt(0)
	assert.True(t, *l.p == *newLeaf().p, "an emptied leaf has the bytes of a new one")
}

func TestLeafNextLink(t *testing.T) {
	l := newLeaf()
	_, ok := l.next()
	assert.False(t, ok)
	l.setNext(7)
	id, ok := l.next()
	assert.True(t, ok)
	assert.Equal(t, storage.PageID(7), id)
}

// These bytes end up in database files, like the tuple and page formats.
func TestLeafFormatIsStable(t *testing.T) {
	l := newLeaf()
	l.insertAt(0, 1, rid(2, 3))
	l.insertAt(1, 258, rid(0x01020304, 0x0506))

	p := l.p
	assert.Equal(t, []byte{2, 0}, p[0:2], "key count")
	assert.Equal(t, byte(storage.KindLeaf), p[8], "page kind")
	assert.Equal(t, []byte{1, 0, 0, 0, 0, 0, 0, 0, 2, 1, 0, 0, 0, 0, 0, 0}, p[16:32], "keys")
	assert.Equal(t, []byte{2, 0, 0, 0, 3, 0, 4, 3, 2, 1, 6, 5}, p[2344:2356], "RIDs")
	assert.NoError(t, p.Validate())
}

func TestAsLeafRejectsBadPages(t *testing.T) {
	tooMany := newLeaf()
	setCount(tooMany.p, LeafCapacity+1)

	tests := []struct {
		name string
		page *storage.Page
	}{
		{"an internal node", newInternal(1).p},
		{"a slotted page", func() *storage.Page { p := new(storage.Page); p.Init(); return p }()},
		{"too many entries", tooMany.p},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := asLeaf(tc.page)
			assert.ErrorIs(t, err, ErrCorrupt)
		})
	}
	_, err := asLeaf(newLeaf().p)
	assert.NoError(t, err)
}

func TestLeafCheckRejectsDisorder(t *testing.T) {
	tests := []struct {
		name   string
		damage func(l leaf)
	}{
		{"duplicate keys", func(l leaf) { setKey(l.p, 1, l.key(0)) }},
		{"descending keys", func(l leaf) { setKey(l.p, 1, -5) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := newLeaf()
			for _, k := range []int64{1, 2, 3} {
				put(t, l, k, rid(1, 1))
			}
			require.NoError(t, l.check())
			tc.damage(l)
			assert.ErrorIs(t, l.check(), ErrCorrupt)
		})
	}
}

// Random toggling of keys in a leaf, checked against a map after every step.
func TestLeafAgainstAModel(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	l := newLeaf()
	model := map[int64]table.RID{}

	for step := 0; step < 5000; step++ {
		k := rng.Int63n(400)
		pos, found := l.search(k)
		switch {
		case found:
			l.removeAt(pos)
			delete(model, k)
		case l.count() < LeafCapacity:
			r := rid(storage.PageID(rng.Intn(1000)), uint16(rng.Intn(1000)))
			l.insertAt(pos, k, r)
			model[k] = r
		}

		want := make([]int64, 0, len(model))
		for key := range model {
			want = append(want, key)
		}
		sort.Slice(want, func(a, b int) bool { return want[a] < want[b] })
		require.Equal(t, want, leafKeys(l), "step %d", step)
		for i, key := range want {
			require.Equal(t, model[key], l.rid(i), "step %d key %d", step, key)
		}
		require.NoError(t, l.check(), "step %d", step)
	}
}

func TestInternalRoute(t *testing.T) {
	n := newInternal(100)
	n.insertAt(0, 10, 101)
	n.insertAt(1, 20, 102)
	n.insertAt(2, 30, 103)
	require.NoError(t, n.check())

	tests := []struct {
		key  int64
		want int
	}{
		{math.MinInt64, 0},
		{9, 0},
		{10, 1}, // a key equal to a separator goes right
		{19, 1},
		{20, 2},
		{29, 2},
		{30, 3},
		{math.MaxInt64, 3},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, n.route(tc.key), "key %d", tc.key)
	}
	for i, want := range []storage.PageID{100, 101, 102, 103} {
		assert.Equal(t, want, n.child(i))
	}
}

func TestInternalInsertInTheMiddle(t *testing.T) {
	n := newInternal(100)
	n.insertAt(0, 10, 101)
	n.insertAt(1, 20, 102)
	n.insertAt(2, 30, 103)

	// child 1 (101) split: it keeps keys 10 to 14, and 200 takes 15 to 19
	n.insertAt(1, 15, 200)

	require.Equal(t, 4, n.count())
	for i, want := range []int64{10, 15, 20, 30} {
		assert.Equal(t, want, n.key(i))
	}
	for i, want := range []storage.PageID{100, 101, 200, 102, 103} {
		assert.Equal(t, want, n.child(i), "child %d", i)
	}
	assert.NoError(t, n.check())
}

func TestInternalFillsToCapacity(t *testing.T) {
	n := newInternal(0)
	for i := 0; i < InternalCapacity; i++ {
		n.insertAt(i, int64(10*i), storage.PageID(i+1))
	}
	require.Equal(t, InternalCapacity, n.count())
	require.NoError(t, n.check())
	assert.Equal(t, storage.PageID(InternalCapacity), n.child(InternalCapacity), "the last child is intact")
	assert.Panics(t, func() { n.insertAt(0, -1, 9) }, "a full node must not take more")
}

func TestInternalRouteMatchesALinearScan(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	n := newInternal(0)
	for i := 0; i < InternalCapacity; i++ {
		n.insertAt(i, int64(10*i), storage.PageID(i+1))
	}
	for i := 0; i < 5000; i++ {
		k := rng.Int63n(10*InternalCapacity+20) - 10
		want := 0
		for j := 0; j < n.count(); j++ {
			if n.key(j) <= k {
				want++
			}
		}
		require.Equal(t, want, n.route(k), "key %d", k)
	}
}

func TestInternalFormatIsStable(t *testing.T) {
	n := newInternal(7)
	n.insertAt(0, 5, 8)

	p := n.p
	assert.Equal(t, []byte{1, 0}, p[0:2], "key count")
	assert.Equal(t, byte(storage.KindInternal), p[8], "page kind")
	assert.Equal(t, []byte{5, 0, 0, 0, 0, 0, 0, 0}, p[16:24], "keys")
	assert.Equal(t, []byte{7, 0, 0, 0, 8, 0, 0, 0}, p[2728:2736], "children")
	assert.NoError(t, p.Validate())
}

func TestAsInternalRejectsBadPages(t *testing.T) {
	tooMany := newInternal(1)
	setCount(tooMany.p, InternalCapacity+1)

	tests := []struct {
		name string
		page *storage.Page
	}{
		{"a leaf", newLeaf().p},
		{"too many keys", tooMany.p},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := asInternal(tc.page)
			assert.ErrorIs(t, err, ErrCorrupt)
		})
	}
}

func TestInternalCheckRejectsDisorder(t *testing.T) {
	n := newInternal(1)
	n.insertAt(0, 10, 2)
	n.insertAt(1, 20, 3)
	require.NoError(t, n.check())

	setKey(n.p, 1, 10)
	assert.ErrorIs(t, n.check(), ErrCorrupt)
	setKey(n.p, 1, 5)
	assert.ErrorIs(t, n.check(), ErrCorrupt)
}
