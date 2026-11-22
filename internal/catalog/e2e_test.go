package catalog

import (
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wilfex81/DB_Engine/internal/storage"
	"github.com/wilfex81/DB_Engine/internal/types"
)

func TestMilestoneBEndToEnd(t *testing.T) {
	const initial, extra, orderRows = 5000, 1000, 300
	rng := rand.New(rand.NewSource(7))
	randName := func() string { return strings.Repeat("n", rng.Intn(150)) }
	person := func(id int64, name string) []types.Value {
		return []types.Value{types.NewInt(id), types.NewText(name), types.NewBool(id%2 == 0)}
	}
	model := map[int64]string{} // id -> name

	// session 1: a table with an index, filled
	e := newEnv(t, 8)
	people, err := e.cat.CreateTable("people", peopleCols())
	require.NoError(t, err)
	pix, err := e.cat.CreateIndex("people", "people_id", "id")
	require.NoError(t, err)
	for id := int64(0); id < initial; id++ {
		name := randName()
		rid, err := people.Heap.Insert(person(id, name))
		require.NoError(t, err)
		require.NoError(t, pix.Tree.Insert(id, rid))
		model[id] = name
	}

	// session 2: delete a third, update a third, add more, and add a table
	e = reopen(t, e, 8)
	people, _ = e.cat.Table("people")
	pix, _ = people.IndexOn("id")
	for id := int64(0); id < initial; id++ {
		switch rng.Intn(3) {
		case 0:
			rid, found, err := pix.Tree.Search(id)
			require.NoError(t, err)
			require.True(t, found, "key %d", id)
			require.NoError(t, people.Heap.Delete(rid))
			require.NoError(t, pix.Tree.Delete(id))
			delete(model, id)
		case 1:
			rid, found, err := pix.Tree.Search(id)
			require.NoError(t, err)
			require.True(t, found, "key %d", id)
			name := randName()
			moved, err := people.Heap.Update(rid, person(id, name))
			require.NoError(t, err)
			if moved != rid { // the row moved, so the index must follow it
				require.NoError(t, pix.Tree.Delete(id))
				require.NoError(t, pix.Tree.Insert(id, moved))
			}
			model[id] = name
		}
	}
	for id := int64(initial); id < initial+extra; id++ {
		name := randName()
		rid, err := people.Heap.Insert(person(id, name))
		require.NoError(t, err)
		require.NoError(t, pix.Tree.Insert(id, rid))
		model[id] = name
	}
	orders, err := e.cat.CreateTable("orders", ordersCols())
	require.NoError(t, err)
	oix, err := e.cat.CreateIndex("orders", "orders_id", "order_id")
	require.NoError(t, err)
	for i := int64(0); i < orderRows; i++ {
		rid, err := orders.Heap.Insert([]types.Value{types.NewInt(i), types.NewFloat(float64(i) / 4)})
		require.NoError(t, err)
		require.NoError(t, oix.Tree.Insert(i, rid))
	}

	// session 3: check everything
	e = reopen(t, e, 8)
	require.Equal(t, []string{"orders", "people"}, e.cat.Tables())
	people, _ = e.cat.Table("people")
	pix, ok := people.IndexOn("id")
	require.True(t, ok)

	// the heap holds exactly the rows of the model
	got := map[int64]string{}
	rows := people.Heap.Scan()
	for rows.Next() {
		v := rows.Values()
		_, dup := got[v[0].Int()]
		require.False(t, dup, "row %d appears twice", v[0].Int())
		got[v[0].Int()] = v[1].Text()
	}
	require.NoError(t, rows.Err())
	require.Equal(t, model, got)

	// the index holds exactly the same keys, in order, and each points at its row
	want := make([]int64, 0, len(model))
	for id := range model {
		want = append(want, id)
	}
	sort.Slice(want, func(a, b int) bool { return want[a] < want[b] })

	var keys []int64
	entries := pix.Tree.Scan()
	for entries.Next() {
		keys = append(keys, entries.Key())
		vals, err := people.Heap.Get(entries.RID())
		require.NoError(t, err)
		require.Equal(t, entries.Key(), vals[0].Int())
		require.Equal(t, model[entries.Key()], vals[1].Text())
	}
	require.NoError(t, entries.Err())
	require.Equal(t, want, keys)
	n, err := pix.Tree.Verify()
	require.NoError(t, err)
	require.Equal(t, len(model), n)

	// a range of the index
	var wantRange, gotRange []int64
	for _, k := range want {
		if k >= 1000 && k <= 1100 {
			wantRange = append(wantRange, k)
		}
	}
	span := pix.Tree.Range(1000, 1100)
	for span.Next() {
		gotRange = append(gotRange, span.Key())
	}
	require.NoError(t, span.Err())
	require.Equal(t, wantRange, gotRange)

	// the second table lives in the same file
	orders, _ = e.cat.Table("orders")
	oix, ok = orders.IndexOn("order_id")
	require.True(t, ok)
	require.Equal(t, orderRows, countRows(t, orders.Heap))
	for i := int64(0); i < orderRows; i++ {
		rid, found, err := oix.Tree.Search(i)
		require.NoError(t, err)
		require.True(t, found, "order %d", i)
		vals, err := orders.Heap.Get(rid)
		require.NoError(t, err)
		require.Equal(t, []types.Value{types.NewInt(i), types.NewFloat(float64(i) / 4)}, vals)
	}

	// every page of the file is a valid page
	var p storage.Page
	for id := 0; id < e.dm.NumPages(); id++ {
		require.NoError(t, e.dm.ReadPage(storage.PageID(id), &p), "page %d", id)
	}
	assertNoPins(t, e.pool)
}