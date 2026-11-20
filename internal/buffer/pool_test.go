package buffer

import (
	"encoding/binary"
	"math/rand"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wilfex81/DB_Engine/internal/storage"
)

func openDisk(tb testing.TB, path string) *storage.DiskManager {
	tb.Helper()
	dm, err := storage.OpenDiskManager(path)
	require.NoError(tb, err)
	tb.Cleanup(func() { _ = dm.Close() })
	return dm
}

func newPool(tb testing.TB, frames int) (*Pool, *storage.DiskManager, string) {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "test.db")
	dm := openDisk(tb, path)
	return NewPool(dm, frames), dm, path
}

// addPage creates a page holding one tuple and unpins it as dirty.
func addPage(tb testing.TB, bp *Pool, data []byte) storage.PageID {
	tb.Helper()
	id, p, err := bp.NewPage()
	require.NoError(tb, err)
	_, err = p.Insert(data)
	require.NoError(tb, err)
	require.NoError(tb, bp.Unpin(id, true))
	return id
}

// read returns a copy of the tuple in slot 0 of a page.
func read(tb testing.TB, bp *Pool, id storage.PageID) []byte {
	tb.Helper()
	p, err := bp.Fetch(id)
	require.NoError(tb, err)
	got, err := p.Get(0)
	require.NoError(tb, err)
	out := append([]byte(nil), got...)
	require.NoError(tb, bp.Unpin(id, false))
	return out
}

func resident(bp *Pool) map[storage.PageID]FrameInfo {
	m := map[storage.PageID]FrameInfo{}
	for _, fi := range bp.Frames() {
		if fi.Used {
			m[fi.Page] = fi
		}
	}
	return m
}

func enc(v uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, v)
	return b
}

func TestNewPoolRejectsBadArguments(t *testing.T) {
	_, dm, _ := newPool(t, 1)
	assert.Panics(t, func() { NewPool(nil, 4) })
	assert.Panics(t, func() { NewPool(dm, 0) })
}

func TestNewPageAndFetch(t *testing.T) {
	bp, _, _ := newPool(t, 3)
	a := addPage(t, bp, []byte("zero"))
	b := addPage(t, bp, []byte("one"))
	assert.Equal(t, storage.PageID(0), a)
	assert.Equal(t, storage.PageID(1), b)
	assert.Equal(t, []byte("zero"), read(t, bp, a))
	assert.Equal(t, []byte("one"), read(t, bp, b))
	assert.Equal(t, 2, bp.NumPages())
}

func TestFetchSharesTheFrame(t *testing.T) {
	bp, _, _ := newPool(t, 2)
	id := addPage(t, bp, []byte("x"))

	a, err := bp.Fetch(id)
	require.NoError(t, err)
	b, err := bp.Fetch(id)
	require.NoError(t, err)
	assert.Same(t, a, b)
	assert.Equal(t, 2, resident(bp)[id].Pins)

	require.NoError(t, bp.Unpin(id, false))
	require.NoError(t, bp.Unpin(id, false))
	assert.ErrorIs(t, bp.Unpin(id, false), ErrNotPinned)
}

func TestUnpinErrors(t *testing.T) {
	tests := []struct {
		name string
		do   func(bp *Pool) error
		want error
	}{
		{"never loaded", func(bp *Pool) error { return bp.Unpin(99, false) }, ErrNotResident},
		{"pin count already zero", func(bp *Pool) error {
			id := addPage(t, bp, []byte("x"))
			return bp.Unpin(id, false)
		}, ErrNotPinned},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bp, _, _ := newPool(t, 2)
			assert.ErrorIs(t, tc.do(bp), tc.want)
		})
	}
}

func TestEvictionWritesDirtyPagesBack(t *testing.T) {
	bp, dm, _ := newPool(t, 2)
	var ids []storage.PageID
	for i := 0; i < 5; i++ {
		ids = append(ids, addPage(t, bp, []byte{byte(i + 1)}))
	}

	// two frames, five pages: page 0 was evicted, so it must be on disk now
	var p storage.Page
	require.NoError(t, dm.ReadPage(ids[0], &p))
	got, err := p.Get(0)
	require.NoError(t, err)
	assert.Equal(t, []byte{1}, got)

	for i, id := range ids {
		assert.Equal(t, []byte{byte(i + 1)}, read(t, bp, id), "page %d", id)
	}
}

// The dirty flag is what decides whether a page is written. A caller that
// changes a page and reports it clean loses the change. This test shows that
// on purpose.
func TestCleanPagesAreNotWrittenOnEviction(t *testing.T) {
	bp, _, _ := newPool(t, 1)
	id := addPage(t, bp, []byte("original"))
	require.NoError(t, bp.Flush(id))

	p, err := bp.Fetch(id)
	require.NoError(t, err)
	require.NoError(t, p.Update(0, []byte("changed")))
	require.NoError(t, bp.Unpin(id, false))

	addPage(t, bp, []byte("other")) // takes the only frame
	assert.Equal(t, []byte("original"), read(t, bp, id))
}

func TestDirtyFlagIsSticky(t *testing.T) {
	bp, _, _ := newPool(t, 2)
	id := addPage(t, bp, []byte("x"))
	require.NoError(t, bp.Flush(id))
	assert.False(t, resident(bp)[id].Dirty)

	_, err := bp.Fetch(id)
	require.NoError(t, err)
	_, err = bp.Fetch(id)
	require.NoError(t, err)
	require.NoError(t, bp.Unpin(id, true))
	require.NoError(t, bp.Unpin(id, false))
	assert.True(t, resident(bp)[id].Dirty)
}

func TestWhenEveryFrameIsPinned(t *testing.T) {
	bp, dm, _ := newPool(t, 2)
	id0, _, err := bp.NewPage()
	require.NoError(t, err)
	id1, _, err := bp.NewPage()
	require.NoError(t, err)

	_, _, err = bp.NewPage()
	assert.ErrorIs(t, err, ErrNoFrames)
	_, err = bp.Fetch(5)
	assert.ErrorIs(t, err, ErrNoFrames)
	assert.Equal(t, 2, dm.NumPages(), "a failed NewPage must not grow the file")

	require.NoError(t, bp.Unpin(id0, true))
	id2, _, err := bp.NewPage()
	require.NoError(t, err)
	assert.Equal(t, storage.PageID(2), id2)
	assert.NotContains(t, resident(bp), id0)
	assert.Contains(t, resident(bp), id1)
}

func TestFetchOfAMissingPageGivesTheFrameBack(t *testing.T) {
	bp, _, _ := newPool(t, 2)
	_, err := bp.Fetch(99)
	assert.ErrorIs(t, err, storage.ErrPageNotFound)
	for _, fi := range bp.Frames() {
		assert.False(t, fi.Used)
	}

	_, _, err = bp.NewPage() // both frames still work
	require.NoError(t, err)
	_, _, err = bp.NewPage()
	require.NoError(t, err)
}

func TestEvictsTheLeastRecentlyUnpinned(t *testing.T) {
	bp, _, _ := newPool(t, 3)
	a := addPage(t, bp, []byte("a"))
	b := addPage(t, bp, []byte("b"))
	c := addPage(t, bp, []byte("c"))

	_, err := bp.Fetch(a) // a becomes the newest
	require.NoError(t, err)
	require.NoError(t, bp.Unpin(a, false))

	addPage(t, bp, []byte("d")) // must evict b
	r := resident(bp)
	assert.Contains(t, r, a)
	assert.Contains(t, r, c)
	assert.NotContains(t, r, b)
}

func TestPinnedPageIsNeverEvicted(t *testing.T) {
	bp, _, _ := newPool(t, 2)
	id, p, err := bp.NewPage()
	require.NoError(t, err)
	_, err = p.Insert([]byte("keep"))
	require.NoError(t, err)

	for i := 0; i < 10; i++ { // all of these go through the one other frame
		addPage(t, bp, []byte{byte(i)})
	}
	assert.Contains(t, resident(bp), id)
	got, err := p.Get(0)
	require.NoError(t, err)
	assert.Equal(t, []byte("keep"), got)
	require.NoError(t, bp.Unpin(id, true))
}

func TestFlush(t *testing.T) {
	bp, dm, _ := newPool(t, 3)
	a := addPage(t, bp, []byte("a"))
	b := addPage(t, bp, []byte("b"))

	require.NoError(t, bp.Flush(a))
	assert.False(t, resident(bp)[a].Dirty)
	assert.True(t, resident(bp)[b].Dirty)
	assert.ErrorIs(t, bp.Flush(99), ErrNotResident)

	var p storage.Page
	require.NoError(t, dm.ReadPage(a, &p))
	got, err := p.Get(0)
	require.NoError(t, err)
	assert.Equal(t, []byte("a"), got)

	require.NoError(t, bp.FlushAll())
	for _, fi := range bp.Frames() {
		assert.False(t, fi.Dirty)
	}
	require.NoError(t, dm.ReadPage(b, &p))
	got, err = p.Get(0)
	require.NoError(t, err)
	assert.Equal(t, []byte("b"), got)
}

func TestDataSurvivesReopen(t *testing.T) {
	bp, dm, path := newPool(t, 2)
	var ids []storage.PageID
	for i := 0; i < 6; i++ {
		ids = append(ids, addPage(t, bp, []byte{byte(i + 10)}))
	}
	require.NoError(t, bp.FlushAll())
	require.NoError(t, dm.Close())

	bp2 := NewPool(openDisk(t, path), 2)
	for i, id := range ids {
		assert.Equal(t, []byte{byte(i + 10)}, read(t, bp2, id), "page %d", id)
	}
}

// Random fetches and updates through a pool much smaller than the data,
// checked against a plain slice after every step.
func TestRandomOperationsAgainstAModel(t *testing.T) {
	const pages, frames, steps = 20, 4, 10000
	rng := rand.New(rand.NewSource(1))
	bp, dm, path := newPool(t, frames)

	model := make([]uint64, pages)
	for i := range model {
		model[i] = uint64(i)
		addPage(t, bp, enc(model[i]))
	}

	for step := 0; step < steps; step++ {
		i := rng.Intn(pages)
		id := storage.PageID(i)
		p, err := bp.Fetch(id)
		require.NoError(t, err, "step %d", step)
		got, err := p.Get(0)
		require.NoError(t, err, "step %d", step)
		require.Equal(t, enc(model[i]), got, "step %d page %d", step, i)

		dirty := rng.Intn(2) == 0
		if dirty {
			model[i] = rng.Uint64()
			require.NoError(t, p.Update(0, enc(model[i])), "step %d", step)
		}
		require.NoError(t, bp.Unpin(id, dirty), "step %d", step)

		for _, fi := range bp.Frames() {
			require.Zero(t, fi.Pins, "step %d: frame %d still pinned", step, fi.Frame)
		}
	}

	require.NoError(t, bp.FlushAll())
	require.NoError(t, dm.Close())
	bp2 := NewPool(openDisk(t, path), frames)
	for i := range model {
		require.Equal(t, enc(model[i]), read(t, bp2, storage.PageID(i)), "page %d after reopen", i)
	}
}

// run with -race: eight workers share eight frames, and each worker owns two
// pages, so the frames are contended but no page is shared
func TestConcurrentUse(t *testing.T) {
	const workers, ops = 8, 500
	bp, _, _ := newPool(t, workers)
	for i := 0; i < workers*2; i++ {
		addPage(t, bp, enc(0))
	}

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(w)))
			var last [2]uint64
			for i := 0; i < ops; i++ {
				k := rng.Intn(2)
				id := storage.PageID(2*w + k)
				p, err := bp.Fetch(id)
				if !assert.NoError(t, err) {
					return
				}
				got, err := p.Get(0)
				if assert.NoError(t, err) {
					assert.Equal(t, enc(last[k]), got)
				}
				dirty := rng.Intn(2) == 0
				if dirty {
					last[k] = rng.Uint64()
					assert.NoError(t, p.Update(0, enc(last[k])))
				}
				assert.NoError(t, bp.Unpin(id, dirty))
			}
		}(w)
	}
	wg.Wait()

	for _, fi := range bp.Frames() {
		assert.Zero(t, fi.Pins)
	}
}
