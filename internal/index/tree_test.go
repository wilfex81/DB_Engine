package index

import (
	"math"
	"math/rand"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wilfex81/DB_Engine/internal/buffer"
	"github.com/wilfex81/DB_Engine/internal/storage"
	"github.com/wilfex81/DB_Engine/internal/table"
)

type treeEnv struct {
	tr   *Tree
	pool *buffer.Pool
	dm   *storage.DiskManager
	path string
}

func newTreeEnv(tb testing.TB, frames int, opts ...Option) *treeEnv {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "idx.db")
	dm, err := storage.OpenDiskManager(path)
	require.NoError(tb, err)
	tb.Cleanup(func() { _ = dm.Close() })
	pool := buffer.NewPool(dm, frames)
	tr, err := Create(pool, opts...)
	require.NoError(tb, err)
	return &treeEnv{tr: tr, pool: pool, dm: dm, path: path}
}

// ridFor gives every key its own RID, so a lookup can be checked.
func ridFor(k int64) table.RID { return rid(storage.PageID(k), uint16(k&0x3ff)) }

func mustInsert(tb testing.TB, tr *Tree, k int64) {
	tb.Helper()
	require.NoError(tb, tr.Insert(k, ridFor(k)), "insert %d", k)
}

func mustFind(tb testing.TB, tr *Tree, k int64) {
	tb.Helper()
	got, ok, err := tr.Search(k)
	require.NoError(tb, err)
	require.True(tb, ok, "key %d not found", k)
	require.Equal(tb, ridFor(k), got, "key %d", k)
}

func assertNoPins(tb testing.TB, pool *buffer.Pool) {
	tb.Helper()
	for _, fi := range pool.Frames() {
		assert.Zero(tb, fi.Pins, "frame %d is still pinned", fi.Frame)
	}
}

// rewrite changes a page behind the tree's back, to test damage detection.
func rewrite(tb testing.TB, pool *buffer.Pool, id storage.PageID, fn func(p *storage.Page)) {
	tb.Helper()
	p, err := pool.Fetch(id)
	require.NoError(tb, err)
	fn(p)
	require.NoError(tb, pool.Unpin(id, true))
}

func ascending(n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = int64(i)
	}
	return out
}

func descending(n int) []int64 {
	out := ascending(n)
	sort.Slice(out, func(a, b int) bool { return out[a] > out[b] })
	return out
}

func shuffled(n int) []int64 {
	out := ascending(n)
	rand.New(rand.NewSource(1)).Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

var sizeCases = []struct {
	name           string
	leaf, internal int
}{
	{"leaf 2 internal 2", 2, 2},
	{"leaf 3 internal 3", 3, 3},
	{"leaf 4 internal 5", 4, 5},
	{"full pages", LeafCapacity, InternalCapacity},
}

var orders = []struct {
	name string
	keys func(n int) []int64
}{
	{"ascending", ascending},
	{"descending", descending},
	{"shuffled", shuffled},
}

func TestEmptyTree(t *testing.T) {
	e := newTreeEnv(t, 4)
	_, ok, err := e.tr.Search(1)
	require.NoError(t, err)
	assert.False(t, ok)

	n, err := e.tr.Verify()
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	h, err := e.tr.Height()
	require.NoError(t, err)
	assert.Equal(t, 1, h)
}

func TestInsertAndSearchInEveryOrder(t *testing.T) {
	const n = 1000
	for _, sz := range sizeCases {
		for _, ord := range orders {
			t.Run(sz.name+"/"+ord.name, func(t *testing.T) {
				e := newTreeEnv(t, 8, WithNodeSizes(sz.leaf, sz.internal))
				keys := ord.keys(n)
				for _, k := range keys {
					mustInsert(t, e.tr, k)
				}

				got, err := e.tr.Verify()
				require.NoError(t, err)
				assert.Equal(t, n, got)
				for _, k := range keys {
					mustFind(t, e.tr, k)
				}
				for _, k := range []int64{-1, n, n + 100, math.MinInt64, math.MaxInt64} {
					_, ok, err := e.tr.Search(k)
					require.NoError(t, err)
					assert.False(t, ok, "key %d", k)
				}
				assertNoPins(t, e.pool)
			})
		}
	}
}

func TestHeightGrowsWithSplits(t *testing.T) {
	e := newTreeEnv(t, 4, WithNodeSizes(2, 2))
	height := func() int {
		h, err := e.tr.Height()
		require.NoError(t, err)
		return h
	}

	assert.Equal(t, 1, height())
	mustInsert(t, e.tr, 1)
	mustInsert(t, e.tr, 2)
	assert.Equal(t, 1, height(), "two keys fit in one leaf")
	mustInsert(t, e.tr, 3)
	assert.Equal(t, 2, height(), "the root leaf split, so a new root was made")
	for k := int64(4); k <= 100; k++ {
		mustInsert(t, e.tr, k)
	}
	assert.GreaterOrEqual(t, height(), 4)
}

// With full size nodes, the root only splits after about 50 thousand
// sequential keys, so this one is slow.
func TestFullSizeNodesGrowToThreeLevels(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	const n = 60000
	e := newTreeEnv(t, 64)
	for k := int64(0); k < n; k++ {
		mustInsert(t, e.tr, k)
	}
	h, err := e.tr.Height()
	require.NoError(t, err)
	assert.Equal(t, 3, h)

	got, err := e.tr.Verify()
	require.NoError(t, err)
	assert.Equal(t, n, got)
	for _, k := range []int64{0, 1, 29999, 59999} {
		mustFind(t, e.tr, k)
	}
}

func TestDuplicateKeyIsRejectedBeforeAnythingIsAllocated(t *testing.T) {
	e := newTreeEnv(t, 4, WithNodeSizes(2, 2))
	mustInsert(t, e.tr, 1)
	mustInsert(t, e.tr, 2) // the root leaf is now full
	pages := e.dm.NumPages()

	err := e.tr.Insert(2, rid(9, 9))
	assert.ErrorIs(t, err, ErrDuplicateKey)
	assert.Equal(t, pages, e.dm.NumPages(), "a rejected key must not split anything")

	mustFind(t, e.tr, 2) // still the first RID
	n, err := e.tr.Verify()
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assertNoPins(t, e.pool)
}

func TestExtremeKeys(t *testing.T) {
	e := newTreeEnv(t, 4)
	keys := []int64{math.MaxInt64, 0, math.MinInt64, -1, 1}
	for _, k := range keys {
		mustInsert(t, e.tr, k)
	}
	for _, k := range keys {
		mustFind(t, e.tr, k)
	}
	n, err := e.tr.Verify()
	require.NoError(t, err)
	assert.Equal(t, len(keys), n)
}

func TestPersistsAcrossReopen(t *testing.T) {
	e := newTreeEnv(t, 4, WithNodeSizes(3, 3))
	keys := shuffled(1000)
	for _, k := range keys {
		mustInsert(t, e.tr, k)
	}
	meta := e.tr.MetaPage()
	require.NoError(t, e.pool.FlushAll())
	require.NoError(t, e.dm.Close())

	dm2, err := storage.OpenDiskManager(e.path)
	require.NoError(t, err)
	defer dm2.Close()
	tr2, err := Open(buffer.NewPool(dm2, 4), meta, WithNodeSizes(3, 3))
	require.NoError(t, err)

	n, err := tr2.Verify()
	require.NoError(t, err)
	assert.Equal(t, 1000, n)
	for _, k := range keys {
		mustFind(t, tr2, k)
	}

	for k := int64(1000); k < 1200; k++ { // and it keeps working
		mustInsert(t, tr2, k)
	}
	n, err = tr2.Verify()
	require.NoError(t, err)
	assert.Equal(t, 1200, n)
}

func TestOpenRejectsBadMeta(t *testing.T) {
	tests := []struct {
		name string
		page func(t *testing.T, e *treeEnv) storage.PageID // the page to open
		want error
	}{
		{"page past the file", func(*testing.T, *treeEnv) storage.PageID { return 999 }, storage.ErrPageNotFound},
		{"a node page", func(_ *testing.T, e *treeEnv) storage.PageID { return e.tr.root }, storage.ErrWrongKind},
		{"wrong magic", func(t *testing.T, e *treeEnv) storage.PageID {
			rewrite(t, e.pool, e.tr.meta, func(p *storage.Page) {
				require.NoError(t, p.Update(0, []byte{1, 2, 3, 4, 5, 6, 7, 8}))
			})
			return e.tr.meta
		}, ErrCorrupt},
		{"root is not a node", func(t *testing.T, e *treeEnv) storage.PageID {
			rewrite(t, e.pool, e.tr.meta, func(p *storage.Page) {
				require.NoError(t, p.Update(0, encodeMeta(e.tr.meta)))
			})
			return e.tr.meta
		}, ErrCorrupt},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newTreeEnv(t, 4)
			_, err := Open(e.pool, tc.page(t, e))
			assert.ErrorIs(t, err, tc.want)
			assertNoPins(t, e.pool)
		})
	}
}

func TestVerifyDetectsDamage(t *testing.T) {
	leftmostLeaf := func(t *testing.T, e *treeEnv) storage.PageID {
		_, id, err := e.tr.descend(math.MinInt64)
		require.NoError(t, err)
		return id
	}
	tests := []struct {
		name   string
		damage func(t *testing.T, e *treeEnv)
	}{
		{"leaf keys out of order", func(t *testing.T, e *treeEnv) {
			rewrite(t, e.pool, leftmostLeaf(t, e), func(p *storage.Page) { setKey(p, 0, key(p, 1)) })
		}},
		{"leaf key outside its range", func(t *testing.T, e *treeEnv) {
			rewrite(t, e.pool, leftmostLeaf(t, e), func(p *storage.Page) { setKey(p, count(p)-1, math.MaxInt64) })
		}},
		{"leaf chain broken", func(t *testing.T, e *treeEnv) {
			rewrite(t, e.pool, leftmostLeaf(t, e), func(p *storage.Page) { p.ClearNext() })
		}},
		{"child reachable twice", func(t *testing.T, e *treeEnv) {
			rewrite(t, e.pool, e.tr.root, func(p *storage.Page) { n := internal{p}; n.setChild(1, n.child(0)) })
		}},
		{"child is not a node", func(t *testing.T, e *treeEnv) {
			rewrite(t, e.pool, e.tr.root, func(p *storage.Page) { internal{p}.setChild(0, e.tr.meta) })
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newTreeEnv(t, 8, WithNodeSizes(3, 3))
			for _, k := range ascending(60) {
				mustInsert(t, e.tr, k)
			}
			n, err := e.tr.Verify()
			require.NoError(t, err)
			require.Equal(t, 60, n)

			tc.damage(t, e)
			_, err = e.tr.Verify()
			assert.ErrorIs(t, err, ErrCorrupt)
			assertNoPins(t, e.pool)
		})
	}
}

func TestWorksWithTwoFrames(t *testing.T) {
	e := newTreeEnv(t, 2, WithNodeSizes(3, 3))
	keys := shuffled(300)
	for _, k := range keys {
		mustInsert(t, e.tr, k)
	}
	n, err := e.tr.Verify()
	require.NoError(t, err)
	assert.Equal(t, 300, n)
	for _, k := range keys {
		mustFind(t, e.tr, k)
	}
	assertNoPins(t, e.pool)
}

// Random inserts and searches, including repeated keys, checked against a map.
func TestTreeAgainstAModel(t *testing.T) {
	const steps = 6000
	for _, sz := range []struct {
		name           string
		leaf, internal int
	}{{"3 and 3", 3, 3}, {"2 and 5", 2, 5}} {
		t.Run(sz.name, func(t *testing.T) {
			rng := rand.New(rand.NewSource(1))
			e := newTreeEnv(t, 4, WithNodeSizes(sz.leaf, sz.internal))
			model := map[int64]bool{}

			for step := 0; step < steps; step++ {
				k := rng.Int63n(1500) - 200
				if rng.Intn(10) < 7 {
					err := e.tr.Insert(k, ridFor(k))
					if model[k] {
						require.ErrorIs(t, err, ErrDuplicateKey, "step %d key %d", step, k)
					} else {
						require.NoError(t, err, "step %d key %d", step, k)
						model[k] = true
					}
				} else {
					got, ok, err := e.tr.Search(k)
					require.NoError(t, err, "step %d", step)
					require.Equal(t, model[k], ok, "step %d key %d", step, k)
					if ok {
						require.Equal(t, ridFor(k), got, "step %d key %d", step, k)
					}
				}

				if step%500 == 499 {
					n, err := e.tr.Verify()
					require.NoError(t, err, "step %d", step)
					require.Equal(t, len(model), n, "step %d", step)
					assertNoPins(t, e.pool)
				}
			}
			for k := range model {
				mustFind(t, e.tr, k)
			}
		})
	}
}

// run with -race: writers insert disjoint keys while readers search
func TestConcurrentInsertsAndSearches(t *testing.T) {
	const writers, each, readers = 4, 500, 3
	e := newTreeEnv(t, 8, WithNodeSizes(4, 4))

	stop := make(chan struct{})
	var rwg sync.WaitGroup
	for r := 0; r < readers; r++ {
		rwg.Add(1)
		go func(seed int64) {
			defer rwg.Done()
			rng := rand.New(rand.NewSource(seed))
			for {
				select {
				case <-stop:
					return
				default:
				}
				k := rng.Int63n(writers * each)
				got, ok, err := e.tr.Search(k)
				if !assert.NoError(t, err) {
					return
				}
				if ok {
					assert.Equal(t, ridFor(k), got)
				}
			}
		}(int64(r))
	}

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(100 + w)))
			for _, i := range rng.Perm(each) {
				k := int64(w*each + i)
				assert.NoError(t, e.tr.Insert(k, ridFor(k)))
			}
		}(w)
	}
	wg.Wait()
	close(stop)
	rwg.Wait()

	n, err := e.tr.Verify()
	require.NoError(t, err)
	assert.Equal(t, writers*each, n)
	for k := int64(0); k < writers*each; k++ {
		mustFind(t, e.tr, k)
	}
	assertNoPins(t, e.pool)
}
