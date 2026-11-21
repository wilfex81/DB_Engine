package index

import (
	"math/rand"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeleteRemovesKeys(t *testing.T) {
	const n = 1000
	for _, sz := range sizeCases {
		t.Run(sz.name, func(t *testing.T) {
			e := newTreeEnv(t, 8, WithNodeSizes(sz.leaf, sz.internal))
			for _, k := range shuffled(n) {
				mustInsert(t, e.tr, k)
			}
			for _, k := range shuffled(n) {
				if k%2 == 0 {
					require.NoError(t, e.tr.Delete(k), "delete %d", k)
				}
			}

			got, err := e.tr.Verify()
			require.NoError(t, err)
			assert.Equal(t, n/2, got)
			for k := int64(0); k < n; k++ {
				if k%2 == 0 {
					_, ok, err := e.tr.Search(k)
					require.NoError(t, err)
					assert.False(t, ok, "key %d should be gone", k)
				} else {
					mustFind(t, e.tr, k)
				}
			}
			assertNoPins(t, e.pool)
		})
	}
}

func TestDeleteErrors(t *testing.T) {
	e := newTreeEnv(t, 4, WithNodeSizes(2, 2))
	assert.ErrorIs(t, e.tr.Delete(1), ErrKeyNotFound, "empty tree")

	for k := int64(1); k <= 10; k++ {
		mustInsert(t, e.tr, k)
	}
	assert.ErrorIs(t, e.tr.Delete(99), ErrKeyNotFound)
	require.NoError(t, e.tr.Delete(5))
	assert.ErrorIs(t, e.tr.Delete(5), ErrKeyNotFound, "deleted twice")

	n, err := e.tr.Verify()
	require.NoError(t, err)
	assert.Equal(t, 9, n)
	assertNoPins(t, e.pool)
}

// Nodes are never merged, so an emptied tree keeps its shape. Putting the same
// keys back must then fit in the nodes that are already there.
func TestEmptiedTreeIsReusedWithoutNewPages(t *testing.T) {
	e := newTreeEnv(t, 8, WithNodeSizes(3, 3))
	for _, k := range shuffled(500) {
		mustInsert(t, e.tr, k)
	}
	pages := e.dm.NumPages()
	height, err := e.tr.Height()
	require.NoError(t, err)

	for _, k := range descending(500) {
		require.NoError(t, e.tr.Delete(k))
	}
	n, err := e.tr.Verify()
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	h, err := e.tr.Height()
	require.NoError(t, err)
	assert.Equal(t, height, h, "no merging, so the shape stays")
	assert.False(t, e.tr.Scan().Next())

	for _, k := range ascending(500) { // another order
		mustInsert(t, e.tr, k)
	}
	assert.Equal(t, pages, e.dm.NumPages(), "every key goes back to the leaf it came from")
	n, err = e.tr.Verify()
	require.NoError(t, err)
	assert.Equal(t, 500, n)
}

// Random inserts, deletes and searches checked against a map. At every
// checkpoint a full scan must equal the sorted keys of the map.
func TestDeleteAgainstAModel(t *testing.T) {
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
				k := rng.Int63n(300)
				switch op := rng.Intn(10); {
				case op < 4:
					err := e.tr.Insert(k, ridFor(k))
					if model[k] {
						require.ErrorIs(t, err, ErrDuplicateKey, "step %d key %d", step, k)
					} else {
						require.NoError(t, err, "step %d key %d", step, k)
						model[k] = true
					}
				case op < 7:
					err := e.tr.Delete(k)
					if model[k] {
						require.NoError(t, err, "step %d key %d", step, k)
						delete(model, k)
					} else {
						require.ErrorIs(t, err, ErrKeyNotFound, "step %d key %d", step, k)
					}
				default:
					got, ok, err := e.tr.Search(k)
					require.NoError(t, err, "step %d", step)
					require.Equal(t, model[k], ok, "step %d key %d", step, k)
					if ok {
						require.Equal(t, ridFor(k), got)
					}
				}

				if step%500 == 499 {
					n, err := e.tr.Verify()
					require.NoError(t, err, "step %d", step)
					require.Equal(t, len(model), n, "step %d", step)

					want := make([]int64, 0, len(model))
					for key := range model {
						want = append(want, key)
					}
					sort.Slice(want, func(a, b int) bool { return want[a] < want[b] })
					require.Equal(t, want, collect(t, e.tr.Scan()), "step %d", step)
					assertNoPins(t, e.pool)
				}
			}
		})
	}
}

// run with -race: writers insert and then delete the odd keys of their own
// range, while readers scan
func TestConcurrentWritesAndScans(t *testing.T) {
	const writers, each, readers = 4, 400, 2
	e := newTreeEnv(t, 8, WithNodeSizes(4, 4))

	stop := make(chan struct{})
	var rwg sync.WaitGroup
	for r := 0; r < readers; r++ {
		rwg.Add(1)
		go func() {
			defer rwg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				prev := int64(-1)
				it := e.tr.Scan()
				for it.Next() {
					assert.Greater(t, it.Key(), prev)
					assert.Equal(t, ridFor(it.Key()), it.RID())
					prev = it.Key()
				}
				assert.NoError(t, it.Err())
			}
		}()
	}

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(w)))
			for _, i := range rng.Perm(each) {
				k := int64(w*each + i)
				assert.NoError(t, e.tr.Insert(k, ridFor(k)))
			}
			for i := 1; i < each; i += 2 {
				assert.NoError(t, e.tr.Delete(int64(w*each+i)))
			}
		}(w)
	}
	wg.Wait()
	close(stop)
	rwg.Wait()

	n, err := e.tr.Verify()
	require.NoError(t, err)
	assert.Equal(t, writers*each/2, n)
	assertNoPins(t, e.pool)
}
