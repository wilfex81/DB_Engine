package storage

import (
	"bytes"
	"math/rand"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newPage(tb testing.TB) *Page {
	tb.Helper()
	p := new(Page)
	p.Init()
	return p
}

func fill(n int, b byte) []byte { return bytes.Repeat([]byte{b}, n) }

func mustInsert(tb testing.TB, p *Page, data []byte) int {
	tb.Helper()
	slot, err := p.Insert(data)
	require.NoError(tb, err)
	return slot
}

func mustGet(tb testing.TB, p *Page, slot int) []byte {
	tb.Helper()
	got, err := p.Get(slot)
	require.NoError(tb, err)
	return got
}

// 39 tuples of 100 bytes: each takes 104 with its slot, and 20 bytes are left.
func fullPage(tb testing.TB) *Page {
	p := newPage(tb)
	for i := 0; i < 39; i++ {
		mustInsert(tb, p, fill(100, byte(i+1)))
	}
	return p
}

func TestEmptyPage(t *testing.T) {
	p := newPage(t)
	assert.Equal(t, 0, p.SlotCount())
	assert.Equal(t, MaxTupleSize, p.FreeSpace())
	assert.NoError(t, p.Validate())
}

func TestInsertAndGet(t *testing.T) {
	p := newPage(t)
	tuples := [][]byte{[]byte("first"), []byte("second tuple"), fill(500, 'x'), {0}}
	for i, tup := range tuples {
		assert.Equal(t, i, mustInsert(t, p, tup))
	}
	for i, tup := range tuples {
		assert.Equal(t, tup, mustGet(t, p, i))
	}
	assert.Equal(t, 4, p.SlotCount())
	assert.NoError(t, p.Validate())
}

func TestInsertCopiesItsInput(t *testing.T) {
	p := newPage(t)
	in := []byte("hello")
	slot := mustInsert(t, p, in)
	in[0] = 'J'
	assert.Equal(t, []byte("hello"), mustGet(t, p, slot))
}

func TestInsertRejectsBadSizes(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want error
	}{
		{"empty", nil, ErrEmptyTuple},
		{"one byte too big", fill(MaxTupleSize+1, 1), ErrTupleTooLarge},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := newPage(t)
			_, err := p.Insert(tc.data)
			assert.ErrorIs(t, err, tc.want)
			assert.Equal(t, 0, p.SlotCount())
		})
	}
}

func TestMaxTupleFitsExactly(t *testing.T) {
	p := newPage(t)
	mustInsert(t, p, fill(MaxTupleSize, 7))
	assert.Equal(t, 0, p.FreeSpace())
	_, err := p.Insert([]byte{1})
	assert.ErrorIs(t, err, ErrNoSpace)
	assert.NoError(t, p.Validate())
}

func TestPageFillsUpExactly(t *testing.T) {
	p := fullPage(t)
	assert.Equal(t, 39, p.SlotCount())
	assert.Equal(t, 20, p.FreeSpace())

	_, err := p.Insert(fill(21, 1)) // one byte more than the FreeSpace says
	assert.ErrorIs(t, err, ErrNoSpace)
	mustInsert(t, p, fill(20, 1)) // exactly FreeSpace fits
	assert.Equal(t, 0, p.FreeSpace())
	assert.NoError(t, p.Validate())
}

func TestGetErrors(t *testing.T) {
	p := newPage(t)
	slot := mustInsert(t, p, []byte("a"))
	require.NoError(t, p.Delete(slot))

	tests := []struct {
		name string
		slot int
		want error
	}{
		{"negative", -1, ErrInvalidSlot},
		{"past the end", 5, ErrInvalidSlot},
		{"deleted", slot, ErrSlotEmpty},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.Get(tc.slot)
			assert.ErrorIs(t, err, tc.want)
		})
	}
}

func TestDeleteAndSlotReuse(t *testing.T) {
	p := newPage(t)
	a := mustInsert(t, p, []byte("aaa"))
	b := mustInsert(t, p, []byte("bbb"))
	c := mustInsert(t, p, []byte("ccc"))

	require.NoError(t, p.Delete(b))
	assert.ErrorIs(t, p.Delete(b), ErrSlotEmpty) // twice is an error
	assert.ErrorIs(t, p.Delete(99), ErrInvalidSlot)

	d := mustInsert(t, p, []byte("ddd"))
	assert.Equal(t, b, d, "the deleted slot is reused")
	assert.Equal(t, 3, p.SlotCount())

	assert.Equal(t, []byte("aaa"), mustGet(t, p, a))
	assert.Equal(t, []byte("ddd"), mustGet(t, p, d))
	assert.Equal(t, []byte("ccc"), mustGet(t, p, c))
	assert.NoError(t, p.Validate())
}

func TestInsertCompactsWhenFragmented(t *testing.T) {
	p := fullPage(t)
	for i := 0; i < 39; i += 2 { // delete slots 0, 2, ..., 38
		require.NoError(t, p.Delete(i))
	}
	// 2024 bytes are free, but only 24 of them in one piece
	assert.Equal(t, 2024, p.FreeSpace())

	slot := mustInsert(t, p, fill(1500, 0xEE))
	assert.Equal(t, 0, slot)
	assert.Equal(t, fill(1500, 0xEE), mustGet(t, p, 0))
	for i := 1; i < 39; i += 2 {
		assert.Equal(t, fill(100, byte(i+1)), mustGet(t, p, i), "slot %d", i)
	}
	assert.NoError(t, p.Validate())
}

func TestUpdate(t *testing.T) {
	t.Run("shrink in place", func(t *testing.T) {
		p := newPage(t)
		slot := mustInsert(t, p, []byte("hello world"))
		require.NoError(t, p.Update(slot, []byte("hi")))
		assert.Equal(t, []byte("hi"), mustGet(t, p, slot))
		assert.NoError(t, p.Validate())
	})

	t.Run("grow into free space", func(t *testing.T) {
		p := newPage(t)
		a := mustInsert(t, p, fill(10, 'a'))
		b := mustInsert(t, p, fill(10, 'b'))
		require.NoError(t, p.Update(a, fill(50, 'A')))
		assert.Equal(t, fill(50, 'A'), mustGet(t, p, a))
		assert.Equal(t, fill(10, 'b'), mustGet(t, p, b))
		assert.NoError(t, p.Validate())
	})

	t.Run("grow after compaction", func(t *testing.T) {
		p := fullPage(t)
		for _, i := range []int{1, 3, 5} {
			require.NoError(t, p.Delete(i))
		}
		require.NoError(t, p.Update(0, fill(380, 0xAB))) // needs the 300 freed bytes
		assert.Equal(t, fill(380, 0xAB), mustGet(t, p, 0))
		for _, i := range []int{2, 4, 6, 20, 38} {
			assert.Equal(t, fill(100, byte(i+1)), mustGet(t, p, i), "slot %d", i)
		}
		assert.NoError(t, p.Validate())
	})

	t.Run("no space leaves the page unchanged", func(t *testing.T) {
		p := fullPage(t)
		before := *p
		assert.ErrorIs(t, p.Update(0, fill(500, 1)), ErrNoSpace)
		_, err := p.Insert(fill(500, 1))
		assert.ErrorIs(t, err, ErrNoSpace)
		assert.True(t, before == *p, "page bytes changed after a failed operation")
	})

	t.Run("errors", func(t *testing.T) {
		p := newPage(t)
		slot := mustInsert(t, p, []byte("a"))
		gone := mustInsert(t, p, []byte("b"))
		require.NoError(t, p.Delete(gone))

		assert.ErrorIs(t, p.Update(99, []byte("x")), ErrInvalidSlot)
		assert.ErrorIs(t, p.Update(gone, []byte("x")), ErrSlotEmpty)
		assert.ErrorIs(t, p.Update(slot, nil), ErrEmptyTuple)
		assert.ErrorIs(t, p.Update(slot, fill(MaxTupleSize+1, 1)), ErrTupleTooLarge)
	})
}

func TestGetCannotAppendIntoNeighbour(t *testing.T) {
	p := newPage(t)
	a := mustInsert(t, p, []byte("aaaa"))
	b := mustInsert(t, p, []byte("bbbb")) // sits right below a

	got := mustGet(t, p, b)
	_ = append(got, 'X') // would overwrite a without the capacity limit
	assert.Equal(t, []byte("aaaa"), mustGet(t, p, a))
}

// These bytes end up in database files, like the tuple format in A1.
func TestPageFormatIsStable(t *testing.T) {
	p := newPage(t)
	mustInsert(t, p, []byte("hi"))

	want := []byte{
		1, 0, // one slot
		0xFE, 0x0F, // free end 4094
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, // reserved
		0xFE, 0x0F, 2, 0, // slot 0: offset 4094, length 2
	}
	assert.Equal(t, want, p[:20])
	assert.Equal(t, []byte("hi"), p[4094:])
}

func TestValidateRejectsDamage(t *testing.T) {
	// three tuples of 10 bytes: slot offsets 4086, 4076, 4066, free end 4066
	base := func() *Page {
		p := newPage(t)
		for i := 0; i < 3; i++ {
			mustInsert(t, p, fill(10, byte(i+1)))
		}
		return p
	}
	require.NoError(t, base().Validate())

	tests := []struct {
		name   string
		damage func(p *Page)
	}{
		{"never initialised", func(p *Page) { *p = Page{} }},
		{"free end past the page", func(p *Page) { p.putU16(2, PageSize+1) }},
		{"free end inside the slot directory", func(p *Page) { p.putU16(2, 20) }},
		{"slot count too big", func(p *Page) { p.putU16(0, 5000) }},
		{"tuple below the free end", func(p *Page) { p.setSlot(0, 100, 10) }},
		{"tuple runs past the page end", func(p *Page) { p.setSlot(0, 4090, 10) }},
		{"tuples overlap", func(p *Page) { p.setSlot(1, 4080, 10) }},
		{"deleted slot with a length", func(p *Page) { p.setSlot(1, 0, 10) }},
		{"tuple of length zero", func(p *Page) { p.setSlot(1, 4076, 0) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := base()
			tc.damage(p)
			assert.ErrorIs(t, p.Validate(), ErrCorrupt)
		})
	}
}

// Random inserts, updates and deletes, checked against a plain map after every
// step. The seed is fixed, so a failure can be repeated.
func TestRandomOperationsAgainstAModel(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	p := newPage(t)
	model := map[int][]byte{}

	liveSlots := func() []int {
		keys := make([]int, 0, len(model))
		for k := range model {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		return keys
	}
	randData := func() []byte {
		b := make([]byte, 1+rng.Intn(300))
		rng.Read(b)
		return b
	}

	for step := 0; step < 20000; step++ {
		keys := liveSlots()
		switch op := rng.Intn(10); {
		case op < 4 || len(keys) == 0: // insert
			data := randData()
			slot, err := p.Insert(data)
			if err != nil {
				require.ErrorIs(t, err, ErrNoSpace, "step %d", step)
				break
			}
			_, taken := model[slot]
			require.False(t, taken, "step %d: slot %d handed out twice", step, slot)
			model[slot] = data
		case op < 7: // update
			slot := keys[rng.Intn(len(keys))]
			data := randData()
			if err := p.Update(slot, data); err != nil {
				require.ErrorIs(t, err, ErrNoSpace, "step %d", step)
				break
			}
			model[slot] = data
		default: // delete
			slot := keys[rng.Intn(len(keys))]
			require.NoError(t, p.Delete(slot), "step %d", step)
			delete(model, slot)
		}

		require.NoError(t, p.Validate(), "step %d", step)
		live := 0
		for i := 0; i < p.SlotCount(); i++ {
			if _, err := p.Get(i); err == nil {
				live++
			}
		}
		require.Equal(t, len(model), live, "step %d", step)
		for slot, want := range model {
			require.Equal(t, want, mustGet(t, p, slot), "step %d slot %d", step, slot)
		}
	}
}

// The property: if a page passes Validate, then compacting it keeps every
// tuple and keeps it valid, and nothing panics. Run it for real with:
//
//	go test -fuzz=FuzzPage -fuzztime=20s ./internal/storage/
func FuzzPage(f *testing.F) {
	good := newPage(f)
	mustInsert(f, good, []byte("alpha"))
	gone := mustInsert(f, good, fill(40, 2))
	mustInsert(f, good, []byte("gamma"))
	require.NoError(f, good.Delete(gone))
	f.Add(good.Bytes())
	f.Add(newPage(f).Bytes())

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) != PageSize {
			return
		}
		var p Page
		copy(p[:], data)
		if p.Validate() != nil {
			return // rejecting a bad page is fine
		}

		before := map[int][]byte{}
		for i := 0; i < p.SlotCount(); i++ {
			if b, err := p.Get(i); err == nil {
				before[i] = append([]byte(nil), b...)
			}
		}

		p.Compact()
		require.NoError(t, p.Validate())
		for slot, want := range before {
			got, err := p.Get(slot)
			require.NoError(t, err)
			require.Equal(t, want, got)
		}

		if _, err := p.Insert([]byte("x")); err == nil {
			require.NoError(t, p.Validate())
		}
	})
}
