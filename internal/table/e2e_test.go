package table

import (
	"math/rand"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wilfex81/DB_Engine/internal/buffer"
	"github.com/wilfex81/DB_Engine/internal/storage"
)

func TestEndToEndAcrossSessions(t *testing.T) {
	const initial, extra = 20000, 5000
	path := filepath.Join(t.TempDir(), "e2e.db")
	schema := testSchema(t)
	rng := rand.New(rand.NewSource(7))
	randName := func() string { return strings.Repeat("e", rng.Intn(200)) }

	model := map[int64]string{} // id -> name
	rids := map[int64]RID{}     // id -> where the row lives now

	// session opens the file with a small pool, runs work, then flushes and closes.
	var first storage.PageID
	session := func(create bool, work func(h *Heap)) {
		dm, err := storage.OpenDiskManager(path)
		require.NoError(t, err)
		pool := buffer.NewPool(dm, 8)
		var h *Heap
		if create {
			h, err = Create(pool, schema)
		} else {
			h, err = Open(pool, schema, first)
		}
		require.NoError(t, err)
		work(h)
		first = h.FirstPage()
		require.NoError(t, pool.FlushAll())
		require.NoError(t, dm.Close())
	}

	// session 1: create and fill
	session(true, func(h *Heap) {
		for id := int64(0); id < initial; id++ {
			name := randName()
			rid, err := h.Insert(row(id, name))
			require.NoError(t, err)
			model[id], rids[id] = name, rid
		}
	})

	// session 2: delete a third, update a third, add more
	session(false, func(h *Heap) {
		for id := int64(0); id < initial; id++ {
			switch rng.Intn(3) {
			case 0:
				require.NoError(t, h.Delete(rids[id]))
				delete(model, id)
				delete(rids, id)
			case 1:
				name := randName()
				rid, err := h.Update(rids[id], row(id, name))
				require.NoError(t, err)
				model[id], rids[id] = name, rid
			}
		}
		for id := int64(initial); id < initial+extra; id++ {
			name := randName()
			rid, err := h.Insert(row(id, name))
			require.NoError(t, err)
			model[id], rids[id] = name, rid
		}
	})

	// session 3: check everything
	session(false, func(h *Heap) {
		got := map[int64]string{}
		for _, e := range scanAll(t, h) {
			id := e.vals[0].Int()
			_, dup := got[id]
			require.False(t, dup, "row %d appears twice", id)
			got[id] = e.vals[1].Text()
		}
		require.Equal(t, model, got)

		for id, rid := range rids {
			vals, err := h.Get(rid)
			require.NoError(t, err)
			require.Equal(t, row(id, model[id]), vals)
		}
	})

	// every page in the file must be a valid page
	dm, err := storage.OpenDiskManager(path)
	require.NoError(t, err)
	defer dm.Close()
	assert.Greater(t, dm.NumPages(), 100)
	var p storage.Page
	for id := 0; id < dm.NumPages(); id++ {
		require.NoError(t, dm.ReadPage(storage.PageID(id), &p), "page %d", id)
	}
}
