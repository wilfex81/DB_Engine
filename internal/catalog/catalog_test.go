package catalog

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wilfex81/DB_Engine/internal/buffer"
	"github.com/wilfex81/DB_Engine/internal/storage"
	"github.com/wilfex81/DB_Engine/internal/table"
	"github.com/wilfex81/DB_Engine/internal/types"
)

type env struct {
	cat  *Catalog
	pool *buffer.Pool
	dm   *storage.DiskManager
	path string
}

func newEnv(tb testing.TB, frames int) *env {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "cat.db")
	dm, err := storage.OpenDiskManager(path)
	require.NoError(tb, err)
	tb.Cleanup(func() { _ = dm.Close() })
	pool := buffer.NewPool(dm, frames)
	cat, err := Create(pool)
	require.NoError(tb, err)
	return &env{cat: cat, pool: pool, dm: dm, path: path}
}

// reopenPool flushes and closes the file, then opens it again and returns a
// new pool over it. The catalog is not opened, so a test can look at the error.
func reopenPool(tb testing.TB, e *env, frames int) (*buffer.Pool, *storage.DiskManager) {
	tb.Helper()
	require.NoError(tb, e.pool.FlushAll())
	require.NoError(tb, e.dm.Close())
	dm, err := storage.OpenDiskManager(e.path)
	require.NoError(tb, err)
	tb.Cleanup(func() { _ = dm.Close() })
	return buffer.NewPool(dm, frames), dm
}

func reopen(tb testing.TB, e *env, frames int) *env {
	tb.Helper()
	pool, dm := reopenPool(tb, e, frames)
	cat, err := Open(pool)
	require.NoError(tb, err)
	return &env{cat: cat, pool: pool, dm: dm, path: e.path}
}

func peopleCols() []types.Column {
	return []types.Column{
		{Name: "id", Type: types.Int, NotNull: true},
		{Name: "name", Type: types.Text},
		{Name: "active", Type: types.Bool},
	}
}

func ordersCols() []types.Column {
	return []types.Column{
		{Name: "order_id", Type: types.Int, NotNull: true},
		{Name: "total", Type: types.Float},
	}
}

func personRow(id int64) []types.Value {
	return []types.Value{types.NewInt(id), types.NewText(fmt.Sprintf("person %d", id)), types.NewBool(id%2 == 0)}
}

func countRows(tb testing.TB, h *table.Heap) int {
	tb.Helper()
	n := 0
	it := h.Scan()
	for it.Next() {
		n++
	}
	require.NoError(tb, it.Err())
	return n
}

func assertNoPins(tb testing.TB, pool *buffer.Pool) {
	tb.Helper()
	for _, fi := range pool.Frames() {
		assert.Zero(tb, fi.Pins, "frame %d is still pinned", fi.Frame)
	}
}

// These bytes end up in database files, like every other format.
func TestCatalogRowFormatIsStable(t *testing.T) {
	b, err := rowSchema.Encode(tableRow("t", 5))
	require.NoError(t, err)
	want := []byte{
		0x78,                   // ref, pos, typ and nn are NULL
		1, 0, 0, 0, 0, 0, 0, 0, // kind: table
		1, 0, 't', // tbl
		1, 0, 't', // name
		5, 0, 0, 0, 0, 0, 0, 0, // page
	}
	assert.Equal(t, want, b)
}

func TestCreateNeedsAnEmptyFile(t *testing.T) {
	e := newEnv(t, 8)
	_, err := Create(e.pool)
	assert.ErrorIs(t, err, ErrNotEmpty)
}

func TestOpenOfAnEmptyFileFails(t *testing.T) {
	dm, err := storage.OpenDiskManager(filepath.Join(t.TempDir(), "empty.db"))
	require.NoError(t, err)
	defer dm.Close()
	_, err = Open(buffer.NewPool(dm, 4))
	assert.ErrorIs(t, err, storage.ErrPageNotFound)
}

func TestCreateTableAndLookup(t *testing.T) {
	e := newEnv(t, 8)
	people, err := e.cat.CreateTable("People", peopleCols())
	require.NoError(t, err)
	_, err = e.cat.CreateTable("orders", ordersCols())
	require.NoError(t, err)

	got, ok := e.cat.Table("PEOPLE") // names ignore case
	require.True(t, ok)
	assert.Same(t, people, got)
	assert.Equal(t, "People", got.Name)
	assert.Equal(t, peopleCols(), got.Schema.Columns())
	_, ok = e.cat.Table("missing")
	assert.False(t, ok)
	assert.Equal(t, []string{"orders", "People"}, e.cat.Tables())

	rid, err := people.Heap.Insert(personRow(1))
	require.NoError(t, err)
	vals, err := people.Heap.Get(rid)
	require.NoError(t, err)
	assert.Equal(t, personRow(1), vals)
}

func TestTableNamesAreChecked(t *testing.T) {
	tests := []struct {
		name  string
		valid bool
	}{
		{"a", true},
		{"_x", true},
		{"Users2", true},
		{strings.Repeat("a", 64), true},
		{"", false},
		{"1abc", false},
		{"a b", false},
		{"a-b", false},
		{"é", false},
		{strings.Repeat("a", 65), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, 8)
			_, err := e.cat.CreateTable(tc.name, peopleCols())
			if tc.valid {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, ErrInvalidName)
			}
		})
	}
}

func TestCreateTableErrors(t *testing.T) {
	tests := []struct {
		name  string
		table string
		cols  []types.Column
		want  string
		is    error
	}{
		{"duplicate name, other case", "PEOPLE", peopleCols(), "", ErrTableExists},
		{"no columns", "t", nil, "at least one column", nil},
		{"duplicate column", "t", []types.Column{{Name: "a", Type: types.Int}, {Name: "A", Type: types.Text}}, "duplicate", nil},
		{"bad column name", "t", []types.Column{{Name: "a b", Type: types.Int}}, "", ErrInvalidName},
		{"bad column type", "t", []types.Column{{Name: "a", Type: types.Type(9)}}, "invalid type", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, 8)
			_, err := e.cat.CreateTable("people", peopleCols())
			require.NoError(t, err)
			pages := e.dm.NumPages()

			_, err = e.cat.CreateTable(tc.table, tc.cols)
			require.Error(t, err)
			if tc.is != nil {
				assert.ErrorIs(t, err, tc.is)
			}
			if tc.want != "" {
				assert.ErrorContains(t, err, tc.want)
			}
			assert.Equal(t, pages, e.dm.NumPages(), "a refused table must not allocate pages")
			assert.Equal(t, []string{"people"}, e.cat.Tables())
			assertNoPins(t, e.pool)
		})
	}
}

func TestEverythingSurvivesReopen(t *testing.T) {
	e := newEnv(t, 16)
	people, err := e.cat.CreateTable("People", peopleCols())
	require.NoError(t, err)
	_, err = e.cat.CreateTable("orders", ordersCols())
	require.NoError(t, err)
	ix, err := e.cat.CreateIndex("People", "people_id", "ID") // the column name ignores case too
	require.NoError(t, err)
	assert.Equal(t, "id", ix.Column, "the index keeps the spelling of the schema")

	for i := int64(0); i < 100; i++ {
		rid, err := people.Heap.Insert(personRow(i))
		require.NoError(t, err)
		require.NoError(t, ix.Tree.Insert(i, rid))
	}

	e2 := reopen(t, e, 16)
	assert.Equal(t, []string{"orders", "People"}, e2.cat.Tables())

	p2, ok := e2.cat.Table("people")
	require.True(t, ok)
	assert.Equal(t, "People", p2.Name)
	assert.Equal(t, peopleCols(), p2.Schema.Columns())
	assert.Equal(t, 100, countRows(t, p2.Heap))

	ix2, ok := p2.IndexOn("id")
	require.True(t, ok)
	assert.Equal(t, "people_id", ix2.Name)
	assert.Equal(t, 0, ix2.ColumnPos)
	for i := int64(0); i < 100; i++ {
		rid, found, err := ix2.Tree.Search(i)
		require.NoError(t, err)
		require.True(t, found, "key %d", i)
		vals, err := p2.Heap.Get(rid)
		require.NoError(t, err)
		require.Equal(t, personRow(i), vals)
	}
	byName, ok := e2.cat.Index("PEOPLE_ID")
	require.True(t, ok)
	assert.Same(t, ix2, byName)

	_, err = e2.cat.CreateTable("later", ordersCols()) // the reopened catalog still works
	require.NoError(t, err)
	e3 := reopen(t, e2, 16)
	assert.Equal(t, []string{"later", "orders", "People"}, e3.cat.Tables())
	assertNoPins(t, e3.pool)
}

func TestDropTable(t *testing.T) {
	e := newEnv(t, 16)
	_, err := e.cat.CreateTable("people", peopleCols())
	require.NoError(t, err)
	_, err = e.cat.CreateIndex("people", "people_id", "id")
	require.NoError(t, err)
	_, err = e.cat.CreateTable("orders", ordersCols())
	require.NoError(t, err)

	assert.ErrorIs(t, e.cat.DropTable("missing"), ErrTableNotFound)
	require.NoError(t, e.cat.DropTable("PEOPLE"))
	_, ok := e.cat.Table("people")
	assert.False(t, ok)
	_, ok = e.cat.Index("people_id")
	assert.False(t, ok, "the indexes go with the table")
	assert.Equal(t, []string{"orders"}, e.cat.Tables())
	assert.ErrorIs(t, e.cat.DropTable("people"), ErrTableNotFound)

	// the name and the index name can be used again, with a different shape
	again, err := e.cat.CreateTable("people", ordersCols())
	require.NoError(t, err)
	_, err = e.cat.CreateIndex("people", "people_id", "order_id")
	require.NoError(t, err)

	e2 := reopen(t, e, 16)
	assert.Equal(t, []string{"orders", "people"}, e2.cat.Tables())
	p2, _ := e2.cat.Table("people")
	assert.Equal(t, again.Schema.Columns(), p2.Schema.Columns())
	ix, ok := p2.IndexOn("order_id")
	require.True(t, ok)
	assert.Equal(t, "people_id", ix.Name)

	// orders: 1 table row and 2 columns. people: 1 table row, 2 columns and 1 index.
	assert.Equal(t, 3+3+1, countRows(t, e2.cat.rows))
}

func TestIndexes(t *testing.T) {
	e := newEnv(t, 16)
	_, err := e.cat.CreateTable("people", peopleCols())
	require.NoError(t, err)
	_, err = e.cat.CreateTable("orders", ordersCols())
	require.NoError(t, err)
	_, err = e.cat.CreateIndex("people", "people_id", "id")
	require.NoError(t, err)

	tests := []struct {
		name                 string
		table, index, column string
		want                 error
	}{
		{"unknown table", "nope", "i1", "id", ErrTableNotFound},
		{"unknown column", "people", "i1", "nope", ErrColumnNotFound},
		{"a TEXT column", "people", "i1", "name", ErrNotIndexable},
		{"a FLOAT column", "orders", "i1", "total", ErrNotIndexable},
		{"same name, other case, other table", "orders", "PEOPLE_ID", "order_id", ErrIndexExists},
		{"bad index name", "orders", "my index", "order_id", ErrInvalidName},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pages := e.dm.NumPages()
			_, err := e.cat.CreateIndex(tc.table, tc.index, tc.column)
			assert.ErrorIs(t, err, tc.want)
			assert.Equal(t, pages, e.dm.NumPages(), "a refused index must not allocate pages")
		})
	}

	people, _ := e.cat.Table("people")
	assert.Len(t, people.Indexes(), 1)
	_, ok := people.IndexOn("name")
	assert.False(t, ok)

	assert.ErrorIs(t, e.cat.DropIndex("missing"), ErrIndexNotFound)
	require.NoError(t, e.cat.DropIndex("People_ID"))
	assert.Empty(t, people.Indexes())
	_, ok = e.cat.Index("people_id")
	assert.False(t, ok)

	_, err = e.cat.CreateIndex("orders", "people_id", "order_id") // the name is free again
	require.NoError(t, err)
	e2 := reopen(t, e, 16)
	o2, _ := e2.cat.Table("orders")
	ix, ok := o2.IndexOn("order_id")
	require.True(t, ok)
	require.NoError(t, ix.Tree.Insert(7, table.RID{Page: 1, Slot: 2}))
	p2, _ := e2.cat.Table("people")
	assert.Empty(t, p2.Indexes(), "the dropped index stays dropped")
}

// A crash in CreateTable leaves column rows without a table row, and a crash
// in DropTable leaves rows behind after the table row is gone. Open removes
// them, so the name is free again and nothing is counted twice.
func TestOrphanRowsAreRemovedOnOpen(t *testing.T) {
	e := newEnv(t, 16)
	_, err := e.cat.CreateTable("keep", peopleCols())
	require.NoError(t, err)
	for i, col := range ordersCols() {
		_, err := e.cat.rows.Insert(columnRow("ghost", col, i))
		require.NoError(t, err)
	}
	_, err = e.cat.rows.Insert(indexRow("ghost", "ghost_ix", "order_id", 7))
	require.NoError(t, err)

	e2 := reopen(t, e, 16)
	assert.Equal(t, []string{"keep"}, e2.cat.Tables())
	assert.Equal(t, 1+3, countRows(t, e2.cat.rows), "the orphans are gone")

	_, err = e2.cat.CreateTable("ghost", []types.Column{{Name: "only", Type: types.Text}})
	require.NoError(t, err)
	e3 := reopen(t, e2, 16)
	g, ok := e3.cat.Table("ghost")
	require.True(t, ok)
	assert.Equal(t, []types.Column{{Name: "only", Type: types.Text}}, g.Schema.Columns())
}

func TestOpenRejectsADamagedCatalog(t *testing.T) {
	people := func(t *testing.T, e *env) *Table {
		tb, err := e.cat.CreateTable("people", peopleCols())
		require.NoError(t, err)
		return tb
	}
	insert := func(t *testing.T, e *env, vals []types.Value) {
		_, err := e.cat.rows.Insert(vals)
		require.NoError(t, err)
	}

	tests := []struct {
		name  string
		setup func(t *testing.T, e *env)
		want  error
	}{
		{"unknown row kind", func(t *testing.T, e *env) {
			row := tableRow("x", 0)
			row[0] = types.NewInt(9)
			insert(t, e, row)
		}, ErrCorrupt},
		{"a gap in the column positions", func(t *testing.T, e *env) {
			tb := people(t, e)
			require.NoError(t, e.cat.rows.Delete(tb.rids[2]))
			insert(t, e, columnRow("people", tb.Schema.Column(2), 5))
		}, ErrCorrupt},
		{"a column position that is NULL", func(t *testing.T, e *env) {
			tb := people(t, e)
			row := columnRow("people", tb.Schema.Column(0), 0)
			row[4] = types.Null(types.Int)
			insert(t, e, row)
		}, ErrCorrupt},
		{"the same table twice", func(t *testing.T, e *env) {
			tb := people(t, e)
			insert(t, e, tableRow("PEOPLE", tb.Heap.FirstPage()))
		}, ErrCorrupt},
		{"an index on a TEXT column", func(t *testing.T, e *env) {
			people(t, e)
			insert(t, e, indexRow("people", "bad", "name", 0))
		}, ErrCorrupt},
		{"an index on a column that does not exist", func(t *testing.T, e *env) {
			people(t, e)
			insert(t, e, indexRow("people", "bad", "nope", 0))
		}, ErrCorrupt},
		{"the same index name twice", func(t *testing.T, e *env) {
			people(t, e)
			ix, err := e.cat.CreateIndex("people", "ix", "id")
			require.NoError(t, err)
			insert(t, e, indexRow("people", "IX", "id", ix.Tree.MetaPage()))
		}, ErrCorrupt},
		{"a heap page past the file", func(t *testing.T, e *env) {
			insert(t, e, columnRow("z", types.Column{Name: "a", Type: types.Int}, 0))
			insert(t, e, tableRow("z", 9999))
		}, storage.ErrPageNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, 16)
			tc.setup(t, e)
			pool, _ := reopenPool(t, e, 16)
			_, err := Open(pool)
			assert.ErrorIs(t, err, tc.want)
			assertNoPins(t, pool)
		})
	}
}

// run with -race: tables and indexes are created from many goroutines
func TestConcurrentUse(t *testing.T) {
	const workers = 8
	e := newEnv(t, 32)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			name := fmt.Sprintf("t%d", w)
			tb, err := e.cat.CreateTable(name, peopleCols())
			if !assert.NoError(t, err) {
				return
			}
			ix, err := e.cat.CreateIndex(name, name+"_ix", "id")
			if !assert.NoError(t, err) {
				return
			}
			for i := int64(0); i < 50; i++ {
				rid, err := tb.Heap.Insert(personRow(i))
				assert.NoError(t, err)
				assert.NoError(t, ix.Tree.Insert(i, rid))
				_, ok := e.cat.Table(name)
				assert.True(t, ok)
				_ = e.cat.Tables()
				_ = tb.Indexes()
			}
		}(w)
	}
	wg.Wait()
	assert.Len(t, e.cat.Tables(), workers)

	e2 := reopen(t, e, 32)
	for w := 0; w < workers; w++ {
		tb, ok := e2.cat.Table(fmt.Sprintf("t%d", w))
		require.True(t, ok)
		assert.Equal(t, 50, countRows(t, tb.Heap))
		assert.Len(t, tb.Indexes(), 1)
	}
	assertNoPins(t, e2.pool)
}
