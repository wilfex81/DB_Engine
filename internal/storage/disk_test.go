package storage

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openTemp(tb testing.TB) (*DiskManager, string) {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "test.db")
	dm, err := OpenDiskManager(path)
	require.NoError(tb, err)
	tb.Cleanup(func() { _ = dm.Close() })
	return dm, path
}

func mustAlloc(tb testing.TB, dm *DiskManager) PageID {
	tb.Helper()
	id, err := dm.AllocatePage()
	require.NoError(tb, err)
	return id
}

func TestNewFileIsEmpty(t *testing.T) {
	dm, path := openTemp(t)
	assert.Equal(t, 0, dm.NumPages())
	st, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, int64(0), st.Size())
}

func TestAllocateGivesEmptyValidPages(t *testing.T) {
	dm, path := openTemp(t)
	for want := PageID(0); want < 3; want++ {
		assert.Equal(t, want, mustAlloc(t, dm))
	}
	assert.Equal(t, 3, dm.NumPages())

	var p Page
	for id := PageID(0); id < 3; id++ {
		require.NoError(t, dm.ReadPage(id, &p))
		assert.Equal(t, 0, p.SlotCount())
	}
	st, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, int64(3*PageSize), st.Size())
}

func TestWriteThenRead(t *testing.T) {
	dm, _ := openTemp(t)
	a, b := mustAlloc(t, dm), mustAlloc(t, dm)

	pa, pb := newPage(t), newPage(t)
	mustInsert(t, pa, []byte("page a"))
	mustInsert(t, pb, []byte("page b"))
	require.NoError(t, dm.WritePage(a, pa))
	require.NoError(t, dm.WritePage(b, pb))

	var got Page
	require.NoError(t, dm.ReadPage(b, &got))
	assert.Equal(t, []byte("page b"), mustGet(t, &got, 0))
	require.NoError(t, dm.ReadPage(a, &got))
	assert.Equal(t, []byte("page a"), mustGet(t, &got, 0))
}

func TestDataSurvivesReopen(t *testing.T) {
	dm, path := openTemp(t)
	id := mustAlloc(t, dm)
	p := newPage(t)
	mustInsert(t, p, []byte("still here"))
	require.NoError(t, dm.WritePage(id, p))
	require.NoError(t, dm.Sync())
	require.NoError(t, dm.Close())

	dm2, err := OpenDiskManager(path)
	require.NoError(t, err)
	defer dm2.Close()
	assert.Equal(t, 1, dm2.NumPages())

	var got Page
	require.NoError(t, dm2.ReadPage(id, &got))
	assert.Equal(t, []byte("still here"), mustGet(t, &got, 0))
}

func TestPageNumberOutOfRange(t *testing.T) {
	dm, _ := openTemp(t)
	mustAlloc(t, dm)
	var p Page
	assert.ErrorIs(t, dm.ReadPage(1, &p), ErrPageNotFound)
	assert.ErrorIs(t, dm.WritePage(1, newPage(t)), ErrPageNotFound)
	assert.ErrorIs(t, dm.ReadPage(1<<31, &p), ErrPageNotFound)
}

func TestWritePageRefusesAnInvalidPage(t *testing.T) {
	dm, _ := openTemp(t)
	id := mustAlloc(t, dm)

	var bad Page // never initialised
	assert.ErrorIs(t, dm.WritePage(id, &bad), ErrCorrupt)

	var got Page
	require.NoError(t, dm.ReadPage(id, &got)) // the file was not touched
	assert.Equal(t, 0, got.SlotCount())
}

func TestReadDetectsDamagedPage(t *testing.T) {
	dm, path := openTemp(t)
	mustAlloc(t, dm)
	mustAlloc(t, dm)
	require.NoError(t, dm.Close())

	// damage page 1 behind the manager's back
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	_, err = f.WriteAt(make([]byte, PageSize), PageSize)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	dm2, err := OpenDiskManager(path)
	require.NoError(t, err)
	defer dm2.Close()
	var p Page
	assert.NoError(t, dm2.ReadPage(0, &p), "the other page is fine")
	assert.ErrorIs(t, dm2.ReadPage(1, &p), ErrCorrupt)
}

func TestOpenRejectsCutFile(t *testing.T) {
	dm, path := openTemp(t)
	mustAlloc(t, dm)
	require.NoError(t, dm.Close())

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	require.NoError(t, err)
	_, err = f.Write(make([]byte, 10)) // a torn write at the end
	require.NoError(t, err)
	require.NoError(t, f.Close())

	_, err = OpenDiskManager(path)
	assert.ErrorIs(t, err, ErrCorrupt)
}

func TestUseAfterClose(t *testing.T) {
	dm, _ := openTemp(t)
	id := mustAlloc(t, dm)
	require.NoError(t, dm.Close())

	var p Page
	assert.ErrorIs(t, dm.ReadPage(id, &p), ErrClosed)
	assert.ErrorIs(t, dm.WritePage(id, newPage(t)), ErrClosed)
	_, err := dm.AllocatePage()
	assert.ErrorIs(t, err, ErrClosed)
	assert.ErrorIs(t, dm.Sync(), ErrClosed)
	assert.ErrorIs(t, dm.Close(), ErrClosed)
}

// The file is just the pages one after another. These bytes are the file format.
func TestFileFormatIsStable(t *testing.T) {
	dm, path := openTemp(t)
	mustAlloc(t, dm)
	id := mustAlloc(t, dm)
	p := newPage(t)
	mustInsert(t, p, []byte("hi"))
	require.NoError(t, dm.WritePage(id, p))
	require.NoError(t, dm.Sync())

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Len(t, raw, 2*PageSize)
	assert.Equal(t, newPage(t).Bytes(), raw[:PageSize])
	assert.Equal(t, p.Bytes(), raw[PageSize:])
}

// run with -race: allocation, reads and writes at the same time
func TestConcurrentUse(t *testing.T) {
	dm, _ := openTemp(t)
	const workers, each = 8, 50

	ids := make(chan PageID, workers*each)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				id, err := dm.AllocatePage()
				if !assert.NoError(t, err) {
					return
				}
				p := newPage(t)
				_, err = p.Insert([]byte{byte(w), byte(i)})
				assert.NoError(t, err)
				assert.NoError(t, dm.WritePage(id, p)) // each worker owns its page
				var got Page
				assert.NoError(t, dm.ReadPage(id, &got))
				ids <- id
			}
		}(w)
	}
	wg.Wait()
	close(ids)

	seen := map[PageID]bool{}
	for id := range ids {
		assert.False(t, seen[id], "page %d handed out twice", id)
		seen[id] = true
	}
	assert.Len(t, seen, workers*each)
	assert.Equal(t, workers*each, dm.NumPages())
}