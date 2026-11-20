package table

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wilfex81/DB_Engine/internal/buffer"
	"github.com/wilfex81/DB_Engine/internal/storage"
	"github.com/wilfex81/DB_Engine/internal/types"
)

func testSchema(tb testing.TB) *types.Schema {
	tb.Helper()
	s, err := types.NewSchema([]types.Column{
		{Name: "id", Type: types.Int, NotNull: true},
		{Name: "name", Type: types.Text},
	})
	require.NoError(tb, err)
	return s
}

func row(id int64, name string) []types.Value {
	return []types.Value{types.NewInt(id), types.NewText(name)}
}

type env struct {
	h    *Heap
	pool *buffer.Pool
	dm   *storage.DiskManager
	path string
}

func newEnv(tb testing.TB, frames int) *env {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "test.db")
	dm, err := storage.OpenDiskManager(path)
	require.NoError(tb, err)
	tb.Cleanup(func() { _ = dm.Close() })
	pool := buffer.NewPool(dm, frames)
	h, err := Create(pool, testSchema(tb))
	require.NoError(tb, err)
	return &env{h: h, pool: pool, dm: dm, path: path}
}

func mustInsert(tb testing.TB, h *Heap, vals []types.Value) RID {
	tb.Helper()
	rid, err := h.Insert(vals)
	require.NoError(tb, err)
	return rid
}

// growTo inserts rows of about 111 bytes until the heap has the given number of pages.
func growTo(tb testing.TB, h *Heap, pages int) []RID {
	tb.Helper()
	name := strings.Repeat("g", 100)
	seen := map[storage.PageID]bool{}
	var rids []RID
	for i := int64(0); len(seen) < pages; i++ {
		rid := mustInsert(tb, h, row(i, name))
		rids = append(rids, rid)
		seen[rid.Page] = true
	}
	return rids
}

type entry struct {
	rid  RID
	vals []types.Value
}

func scanAll(tb testing.TB, h *Heap) []entry {
	tb.Helper()
	var out []entry
	it := h.Scan()
	for it.Next() {
		out = append(out, entry{it.RID(), it.Values()})
	}
	require.NoError(tb, it.Err())
	return out
}

func scanMap(tb testing.TB, h *Heap) map[RID][]types.Value {
	tb.Helper()
	m := map[RID][]types.Value{}
	for _, e := range scanAll(tb, h) {
		m[e.rid] = e.vals
	}
	return m
}

func assertNoPins(tb testing.TB, pool *buffer.Pool) {
	tb.Helper()
	for _, fi := range pool.Frames() {
		assert.Zero(tb, fi.Pins, "frame %d is still pinned", fi.Frame)
	}
}

func TestRIDString(t *testing.T) {
	assert.Equal(t, "(3,7)", RID{Page: 3, Slot: 7}.String())
}

func TestInsertAndGet(t *testing.T) {
	e := newEnv(t, 4)
	want := [][]types.Value{row(1, "Wilfex"), row(2, ""), {types.NewInt(3), types.Null(types.Text)}}
	var rids []RID
	for _, v := range want {
		rids = append(rids, mustInsert(t, e.h, v))
	}
	assert.Equal(t, RID{e.h.FirstPage(), 0}, rids[0])
	assert.Equal(t, RID{e.h.FirstPage(), 2}, rids[2])
	for i, rid := range rids {
		got, err := e.h.Get(rid)
		require.NoError(t, err)
		assert.Equal(t, want[i], got)
	}
}

func TestInsertSpillsToNewPages(t *testing.T) {
	e := newEnv(t, 4)
	const n = 300
	name := strings.Repeat("n", 100)
	rids := make([]RID, n)
	pages := map[storage.PageID]bool{}
	for i := range rids {
		rids[i] = mustInsert(t, e.h, row(int64(i), name))
		pages[rids[i].Page] = true
	}
	assert.Greater(t, len(pages), 1)

	for i, rid := range rids {
		got, err := e.h.Get(rid)
		require.NoError(t, err)
		assert.Equal(t, row(int64(i), name), got)
	}
	got := scanAll(t, e.h)
	require.Len(t, got, n)
	for i, en := range got {
		assert.Equal(t, rids[i], en.rid)
		assert.Equal(t, row(int64(i), name), en.vals)
	}
	assertNoPins(t, e.pool)
}

func TestInsertErrors(t *testing.T) {
	tests := []struct {
		name string
		vals []types.Value
		want string
	}{
		{"too few values", row(1, "a")[:1], "columns"},
		{"wrong type", []types.Value{types.NewText("x"), types.NewText("y")}, "expects INT"},
		{"NULL in a NOT NULL column", []types.Value{types.Null(types.Int), types.NewText("y")}, "cannot be NULL"},
		{"row larger than a page", row(1, strings.Repeat("a", 5000)), "larger than a page"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, 2)
			_, err := e.h.Insert(tc.vals)
			assert.ErrorContains(t, err, tc.want)
			assert.Empty(t, scanAll(t, e.h))
			assertNoPins(t, e.pool)
		})
	}
}

func TestScanOfAnEmptyHeap(t *testing.T) {
	e := newEnv(t, 2)
	it := e.h.Scan()
	assert.False(t, it.Next())
	assert.NoError(t, it.Err())
	assert.False(t, it.Next(), "stays finished")
}

func TestDeleteAndScan(t *testing.T) {
	e := newEnv(t, 4)
	var rids []RID
	for i := 0; i < 10; i++ {
		rids = append(rids, mustInsert(t, e.h, row(int64(i), "x")))
	}
	for i := 0; i < 10; i += 2 {
		require.NoError(t, e.h.Delete(rids[i]))
		_, err := e.h.Get(rids[i])
		assert.ErrorIs(t, err, storage.ErrSlotEmpty)
	}

	got := scanAll(t, e.h)
	require.Len(t, got, 5)
	for i, en := range got {
		assert.Equal(t, int64(2*i+1), en.vals[0].Int())
	}
}

func TestDeleteAndGetErrors(t *testing.T) {
	tests := []struct {
		name string
		rid  func(first storage.PageID, live RID) RID
		want error
	}{
		{"slot past the end", func(f storage.PageID, _ RID) RID { return RID{f, 50} }, storage.ErrInvalidSlot},
		{"page past the file", func(storage.PageID, RID) RID { return RID{99, 0} }, storage.ErrPageNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, 2)
			live := mustInsert(t, e.h, row(1, "a"))
			rid := tc.rid(e.h.FirstPage(), live)
			assert.ErrorIs(t, e.h.Delete(rid), tc.want)
			_, err := e.h.Get(rid)
			assert.ErrorIs(t, err, tc.want)
			assertNoPins(t, e.pool)
		})
	}

	t.Run("deleted twice", func(t *testing.T) {
		e := newEnv(t, 2)
		rid := mustInsert(t, e.h, row(1, "a"))
		require.NoError(t, e.h.Delete(rid))
		assert.ErrorIs(t, e.h.Delete(rid), storage.ErrSlotEmpty)
	})
}

func TestUpdateInPlace(t *testing.T) {
	e := newEnv(t, 2)
	rid := mustInsert(t, e.h, row(1, "a rather long name"))
	got, err := e.h.Update(rid, row(1, "x"))
	require.NoError(t, err)
	assert.Equal(t, rid, got, "the RID stays when the row still fits")

	vals, err := e.h.Get(rid)
	require.NoError(t, err)
	assert.Equal(t, row(1, "x"), vals)
}

func TestUpdateMovesWhenThePageIsFull(t *testing.T) {
	e := newEnv(t, 4)
	rids := growTo(t, e.h, 2) // the first page is now too full to grow a row by 400 bytes
	old := rids[0]

	newRID, err := e.h.Update(old, row(0, strings.Repeat("big", 170)))
	require.NoError(t, err)
	assert.NotEqual(t, old, newRID)

	_, err = e.h.Get(old)
	assert.ErrorIs(t, err, storage.ErrSlotEmpty)
	got, err := e.h.Get(newRID)
	require.NoError(t, err)
	assert.Equal(t, row(0, strings.Repeat("big", 170)), got)
	assert.Len(t, scanAll(t, e.h), len(rids), "a move must not add or lose a row")
	assertNoPins(t, e.pool)
}

func TestUpdateErrors(t *testing.T) {
	e := newEnv(t, 2)
	rid := mustInsert(t, e.h, row(1, "keep"))
	gone := mustInsert(t, e.h, row(2, "gone"))
	require.NoError(t, e.h.Delete(gone))

	_, err := e.h.Update(gone, row(2, "x"))
	assert.ErrorIs(t, err, storage.ErrSlotEmpty)
	_, err = e.h.Update(rid, row(1, strings.Repeat("a", 5000)))
	assert.ErrorIs(t, err, storage.ErrTupleTooLarge)
	_, err = e.h.Update(rid, []types.Value{types.NewInt(1)})
	assert.Error(t, err)

	got, err := e.h.Get(rid)
	require.NoError(t, err)
	assert.Equal(t, row(1, "keep"), got, "a failed update changes nothing")
}

func TestScanLeavesNothingPinned(t *testing.T) {
	e := newEnv(t, 4)
	growTo(t, e.h, 3)

	it := e.h.Scan()
	require.True(t, it.Next())
	assertNoPins(t, e.pool) // abandoned half way, and nothing is held
}

func TestWorksWithOneFrame(t *testing.T) {
	e := newEnv(t, 1)
	rids := growTo(t, e.h, 3)
	assert.Len(t, scanAll(t, e.h), len(rids))
	_, err := e.h.Get(rids[0])
	assert.NoError(t, err)
	assertNoPins(t, e.pool)
}

func TestReopenFindsTheChain(t *testing.T) {
	e := newEnv(t, 4)
	growTo(t, e.h, 4)
	want := scanAll(t, e.h)
	require.NoError(t, e.pool.FlushAll())
	require.NoError(t, e.dm.Close())

	dm2, err := storage.OpenDiskManager(e.path)
	require.NoError(t, err)
	defer dm2.Close()
	h2, err := Open(buffer.NewPool(dm2, 4), testSchema(t), e.h.FirstPage())
	require.NoError(t, err)
	assert.Equal(t, want, scanAll(t, h2))

	rid := mustInsert(t, h2, row(999, "after"))
	all := scanAll(t, h2)
	require.Len(t, all, len(want)+1)
	assert.Equal(t, rid, all[len(all)-1].rid, "the new row goes to the tail")
}

func TestOpenRejectsADamagedChain(t *testing.T) {
	tests := []struct {
		name   string
		target func(first storage.PageID) storage.PageID
		want   error
	}{
		{"cycle back to the start", func(first storage.PageID) storage.PageID { return first }, ErrCorrupt},
		{"link to a page past the file", func(storage.PageID) storage.PageID { return 999 }, storage.ErrPageNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, 4)
			rids := growTo(t, e.h, 2)
			last := rids[len(rids)-1].Page

			p, err := e.pool.Fetch(last)
			require.NoError(t, err)
			p.SetNext(tc.target(e.h.FirstPage()))
			require.NoError(t, e.pool.Unpin(last, true))

			_, err = Open(e.pool, testSchema(t), e.h.FirstPage())
			assert.ErrorIs(t, err, tc.want)
			assertNoPins(t, e.pool)
		})
	}
}

// Random inserts, updates and deletes through a three frame pool, checked
// against a plain map. Updates that move a row change its RID, and the model
// follows the RID that Update returns.
func TestRandomOperationsAgainstAModel(t *testing.T) {
	const steps = 5000
	rng := rand.New(rand.NewSource(1))
	e := newEnv(t, 3)
	model := map[RID][]types.Value{}
	var live []RID

	randRow := func(id int64) []types.Value {
		return row(id, strings.Repeat("r", rng.Intn(400)))
	}

	for step := 0; step < steps; step++ {
		switch op := rng.Intn(10); {
		case op < 4 || len(live) == 0: // insert
			v := randRow(int64(step))
			rid, err := e.h.Insert(v)
			require.NoError(t, err, "step %d", step)
			_, taken := model[rid]
			require.False(t, taken, "step %d: RID %s handed out twice", step, rid)
			model[rid] = v
			live = append(live, rid)
		case op < 7: // update
			i := rng.Intn(len(live))
			old := live[i]
			v := randRow(int64(step))
			rid, err := e.h.Update(old, v)
			require.NoError(t, err, "step %d", step)
			delete(model, old)
			_, taken := model[rid]
			require.False(t, taken, "step %d: RID %s handed out twice", step, rid)
			model[rid] = v
			live[i] = rid
		default: // delete
			i := rng.Intn(len(live))
			require.NoError(t, e.h.Delete(live[i]), "step %d", step)
			delete(model, live[i])
			live[i] = live[len(live)-1]
			live = live[:len(live)-1]
		}

		if step%250 == 0 || step == steps-1 {
			require.Equal(t, model, scanMap(t, e.h), "step %d", step)
			assertNoPins(t, e.pool)
		}
	}
	for rid, want := range model {
		got, err := e.h.Get(rid)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
}

// run with -race: writers insert while readers scan
func TestConcurrentWritersAndReaders(t *testing.T) {
	const writers, each, readers = 4, 200, 2
	e := newEnv(t, 8)

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
				it := e.h.Scan()
				for it.Next() {
					v := it.Values()
					assert.Equal(t, fmt.Sprintf("row-%d", v[0].Int()), v[1].Text())
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
			for i := 0; i < each; i++ {
				id := int64(w*each + i)
				_, err := e.h.Insert(row(id, fmt.Sprintf("row-%d", id)))
				assert.NoError(t, err)
			}
		}(w)
	}
	wg.Wait()
	close(stop)
	rwg.Wait()

	got := scanAll(t, e.h)
	assert.Len(t, got, writers*each)
	ids := map[int64]bool{}
	for _, en := range got {
		ids[en.vals[0].Int()] = true
	}
	assert.Len(t, ids, writers*each, "every row exactly once")
	assertNoPins(t, e.pool)
}
