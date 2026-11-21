package index

import (
	"math"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wilfex81/DB_Engine/internal/storage"
)

// collect drains an iterator, checking every RID on the way.
func collect(tb testing.TB, it *Iterator) []int64 {
	tb.Helper()
	var out []int64
	for it.Next() {
		require.Equal(tb, ridFor(it.Key()), it.RID(), "key %d", it.Key())
		out = append(out, it.Key())
	}
	require.NoError(tb, it.Err())
	return out
}

func evens(n int64) []int64 {
	var out []int64
	for k := int64(0); k < n; k += 2 {
		out = append(out, k)
	}
	return out
}

func TestScanReturnsKeysInOrder(t *testing.T) {
	const n = 500
	for _, sz := range sizeCases {
		for _, ord := range orders {
			t.Run(sz.name+"/"+ord.name, func(t *testing.T) {
				e := newTreeEnv(t, 8, WithNodeSizes(sz.leaf, sz.internal))
				for _, k := range ord.keys(n) {
					mustInsert(t, e.tr, k)
				}
				assert.Equal(t, ascending(n), collect(t, e.tr.Scan()))
				assertNoPins(t, e.pool)
			})
		}
	}
}

func TestEmptyTreeScan(t *testing.T) {
	e := newTreeEnv(t, 4)
	it := e.tr.Scan()
	assert.False(t, it.Next())
	assert.NoError(t, it.Err())
	assert.False(t, it.Next(), "stays finished")
}

func TestRangeBounds(t *testing.T) {
	e := newTreeEnv(t, 8, WithNodeSizes(3, 3))
	for _, k := range evens(100) { // 0, 2, ..., 98
		mustInsert(t, e.tr, k)
	}

	tests := []struct {
		name   string
		lo, hi int64
		want   []int64
	}{
		{"both bounds are keys", 10, 20, []int64{10, 12, 14, 16, 18, 20}},
		{"bounds between keys", 11, 19, []int64{12, 14, 16, 18}},
		{"starts below the first key", -5, 3, []int64{0, 2}},
		{"ends past the last key", 98, 1000, []int64{98}},
		{"starts past the last key", 99, 1000, nil},
		{"one key", 50, 50, []int64{50}},
		{"one missing key", 51, 51, nil},
		{"lo above hi", 20, 10, nil},
		{"the largest possible key", math.MaxInt64, math.MaxInt64, nil},
		{"the smallest possible bound", math.MinInt64, 4, []int64{0, 2, 4}},
		{"everything", math.MinInt64, math.MaxInt64, evens(100)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, collect(t, e.tr.Range(tc.lo, tc.hi)))
		})
	}

	assert.Equal(t, []int64{90, 92, 94, 96, 98}, collect(t, e.tr.Seek(90)))
	assert.Equal(t, evens(100), collect(t, e.tr.Scan()))
	assertNoPins(t, e.pool)
}

func TestScanSkipsEmptyLeaves(t *testing.T) {
	e := newTreeEnv(t, 8, WithNodeSizes(3, 3))
	for _, k := range ascending(100) {
		mustInsert(t, e.tr, k)
	}
	for k := int64(20); k < 80; k++ { // empties many leaves in the middle
		require.NoError(t, e.tr.Delete(k))
	}

	var want []int64
	want = append(want, ascending(20)...)
	for k := int64(80); k < 100; k++ {
		want = append(want, k)
	}
	assert.Equal(t, want, collect(t, e.tr.Scan()))
	assert.Equal(t, []int64{18, 19, 80, 81}, collect(t, e.tr.Range(18, 81)))
}

func TestScanLeavesNothingPinned(t *testing.T) {
	e := newTreeEnv(t, 2, WithNodeSizes(3, 3))
	for _, k := range ascending(200) {
		mustInsert(t, e.tr, k)
	}
	it := e.tr.Scan()
	require.True(t, it.Next())
	assertNoPins(t, e.pool) // abandoned after one key, and nothing is held

	assert.Equal(t, ascending(200), collect(t, e.tr.Scan()), "a full scan works with two frames")
}

// The tree changes between two calls of Next: splits move keys to a new leaf,
// deletes remove keys that were ahead. The keys must still come out ascending
// and only once, a deleted key must not come out, and a key that was never
// touched must not be missed.
func TestScanWhileTheTreeChanges(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	e := newTreeEnv(t, 4, WithNodeSizes(3, 3))
	present := map[int64]bool{}
	for k := int64(0); k < 3000; k += 10 {
		mustInsert(t, e.tr, k)
		present[k] = true
	}
	removed := map[int64]bool{}

	it := e.tr.Scan()
	seen := map[int64]bool{}
	prev := int64(math.MinInt64)
	for it.Next() {
		k := it.Key()
		require.Greater(t, k, prev, "keys must come out strictly ascending")
		require.True(t, present[k], "key %d was returned but is not in the tree", k)
		prev = k
		seen[k] = true

		if k%10 == 0 && rng.Intn(3) == 0 {
			mustInsert(t, e.tr, k+5) // just ahead: may be seen
			present[k+5] = true
			mustInsert(t, e.tr, k-3) // behind: must not be seen
			present[k-3] = true
			if present[k+10] { // ahead: must not be seen
				require.NoError(t, e.tr.Delete(k+10))
				present[k+10] = false
				removed[k+10] = true
			}
		}
	}
	require.NoError(t, it.Err())

	for k := int64(0); k < 3000; k += 10 {
		if !removed[k] {
			assert.True(t, seen[k], "key %d was never returned", k)
		}
	}
	n, err := e.tr.Verify()
	require.NoError(t, err)
	count := 0
	for _, ok := range present {
		if ok {
			count++
		}
	}
	assert.Equal(t, count, n)
	assertNoPins(t, e.pool)
}

func TestScanStopsOnALoopingLeafChain(t *testing.T) {
	e := newTreeEnv(t, 8, WithNodeSizes(3, 3))
	for _, k := range ascending(30) {
		mustInsert(t, e.tr, k)
	}
	_, leaf, err := e.tr.descend(math.MinInt64)
	require.NoError(t, err)
	rewrite(t, e.pool, leaf, func(p *storage.Page) { p.SetNext(leaf) }) // the first leaf links to itself

	it := e.tr.Scan()
	for it.Next() {
	}
	assert.ErrorIs(t, it.Err(), ErrCorrupt)
	assertNoPins(t, e.pool)
}
