package query

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wilfex81/DB_Engine/internal/buffer"
	"github.com/wilfex81/DB_Engine/internal/catalog"
	"github.com/wilfex81/DB_Engine/internal/sql"
	"github.com/wilfex81/DB_Engine/internal/storage"
	"github.com/wilfex81/DB_Engine/internal/table"
	"github.com/wilfex81/DB_Engine/internal/types"
)

func newDB(tb testing.TB) (*catalog.Catalog, *buffer.Pool) {
	tb.Helper()
	dm, err := storage.OpenDiskManager(filepath.Join(tb.TempDir(), "q.db"))
	require.NoError(tb, err)
	tb.Cleanup(func() { _ = dm.Close() })
	pool := buffer.NewPool(dm, 32)
	cat, err := catalog.Create(pool)
	require.NoError(tb, err)
	return cat, pool
}

// seedPeople makes people(id INT NOT NULL, name TEXT NOT NULL, age INT, score FLOAT)
// with 100 rows, and unique indexes people_id and people_age. Row i has id i,
// name "n<i>", score i/4, and age 1000+i, or NULL when i is a multiple of 10.
// Rows are in the heap in id order. The executor of C6 will do this work
// itself, so for now the test keeps the indexes in step by hand.
func seedPeople(tb testing.TB, cat *catalog.Catalog) (*catalog.Table, *catalog.Index) {
	tb.Helper()
	people, err := cat.CreateTable("people", []types.Column{
		{Name: "id", Type: types.Int, NotNull: true},
		{Name: "name", Type: types.Text, NotNull: true},
		{Name: "age", Type: types.Int},
		{Name: "score", Type: types.Float},
	})
	require.NoError(tb, err)
	idIx, err := cat.CreateIndex("people", "people_id", "id")
	require.NoError(tb, err)
	ageIx, err := cat.CreateIndex("people", "people_age", "age")
	require.NoError(tb, err)

	for i := int64(1); i <= 100; i++ {
		age := types.Null(types.Int)
		if i%10 != 0 {
			age = types.NewInt(1000 + i)
		}
		rid, err := people.Heap.Insert([]types.Value{
			types.NewInt(i), types.NewText(fmt.Sprintf("n%d", i)), age, types.NewFloat(float64(i) / 4),
		})
		require.NoError(tb, err)
		require.NoError(tb, idIx.Tree.Insert(i, rid))
		if !age.IsNull() {
			require.NoError(tb, ageIx.Tree.Insert(1000+i, rid))
		}
	}
	return people, idIx
}

func planSQL(tb testing.TB, cat *catalog.Catalog, src string, opts ...PlanOption) *Plan {
	tb.Helper()
	s, err := sql.ParseStatement(src)
	require.NoError(tb, err, src)
	b, err := Bind(NewCatalog(cat), s)
	require.NoError(tb, err, src)
	plan, err := PlanSelect(cat, b.(*Select), opts...)
	require.NoError(tb, err, src)
	return plan
}

func rowsOf(tb testing.TB, cat *catalog.Catalog, src string, opts ...PlanOption) [][]types.Value {
	tb.Helper()
	rows, err := planSQL(tb, cat, src, opts...).Rows()
	require.NoError(tb, err, src)
	return rows
}

func TestExplain(t *testing.T) {
	cat, _ := newDB(t)
	seedPeople(t, cat)

	all := "Project id, name, age, score"
	tests := []struct {
		src  string
		want []string
	}{
		// no index helps
		{"SELECT * FROM people", []string{all, "  SeqScan people"}},
		{"SELECT * FROM people WHERE name = 'n5'", []string{all, "  Filter (name = 'n5')", "    SeqScan people"}},
		{"SELECT * FROM people WHERE id <> 5", []string{all, "  Filter (id <> 5)", "    SeqScan people"}},
		{"SELECT * FROM people WHERE id = 5 OR id = 6", []string{all, "  Filter ((id = 5) OR (id = 6))", "    SeqScan people"}},
		{"SELECT * FROM people WHERE id = 5.0", []string{all, "  Filter (CAST(id AS FLOAT) = 5.0)", "    SeqScan people"}},

		// lookups and ranges
		{"SELECT * FROM people WHERE id = 5", []string{all, "  IndexScan people using people_id (id = 5)"}},
		{"SELECT * FROM people WHERE 5 = id", []string{all, "  IndexScan people using people_id (id = 5)"}},
		{"SELECT * FROM people WHERE id > 10 AND id <= 20", []string{all, "  IndexScan people using people_id (id BETWEEN 11 AND 20)"}},
		{"SELECT * FROM people WHERE id >= 10", []string{all, "  IndexScan people using people_id (id >= 10)"}},
		{"SELECT * FROM people WHERE id < 10", []string{all, "  IndexScan people using people_id (id <= 9)"}},
		{"SELECT * FROM people WHERE 10 < id", []string{all, "  IndexScan people using people_id (id >= 11)"}},
		{"SELECT * FROM people WHERE id > 10 AND id < 5", []string{all, "  IndexScan people using people_id (id BETWEEN 11 AND 4)"}},
		{"SELECT * FROM people WHERE id = 5 AND id = 6", []string{all, "  IndexScan people using people_id (id BETWEEN 6 AND 5)"}},
		{"SELECT * FROM people WHERE age = 1005", []string{all, "  IndexScan people using people_age (age = 1005)"}},

		// what the index does not answer stays in a Filter
		{"SELECT * FROM people WHERE id = 5 AND name = 'n5'", []string{all, "  Filter (name = 'n5')", "    IndexScan people using people_id (id = 5)"}},
		{"SELECT * FROM people WHERE id = 5 AND age = 1005", []string{all, "  Filter (id = 5)", "    IndexScan people using people_age (age = 1005)"}},
		{"SELECT * FROM people WHERE age > 1050 AND id < 70", []string{all, "  Filter (id < 70)", "    IndexScan people using people_age (age >= 1051)"}},

		// ORDER BY and LIMIT
		{"SELECT * FROM people ORDER BY id", []string{all, "  Sort id", "    SeqScan people"}},
		{"SELECT * FROM people ORDER BY id LIMIT 3", []string{all, "  Limit 3", "    IndexScan people using people_id (id ordered)"}},
		{"SELECT * FROM people ORDER BY id DESC LIMIT 3", []string{all, "  Limit 3", "    Sort id DESC", "      SeqScan people"}},
		{"SELECT * FROM people ORDER BY name LIMIT 3", []string{all, "  Limit 3", "    Sort name", "      SeqScan people"}},
		{"SELECT * FROM people ORDER BY age LIMIT 3", []string{all, "  Limit 3", "    Sort age", "      SeqScan people"}}, // age may be NULL
		{"SELECT * FROM people WHERE id > 10 ORDER BY id", []string{all, "  IndexScan people using people_id (id >= 11)"}},
		{"SELECT * FROM people WHERE id > 10 ORDER BY id DESC", []string{all, "  Sort id DESC", "    IndexScan people using people_id (id >= 11)"}},
		{"SELECT * FROM people WHERE age = 1005 ORDER BY id", []string{all, "  Sort id", "    IndexScan people using people_age (age = 1005)"}},
		{"SELECT id FROM people WHERE age > 1050 ORDER BY age LIMIT 2",
			[]string{"Project id", "  Limit 2", "    IndexScan people using people_age (age >= 1051)"}},
		{"SELECT id FROM people WHERE id = 1 LIMIT 0", []string{"Project id", "  Limit 0", "    IndexScan people using people_id (id = 1)"}},

		// no table
		{"SELECT 1 + 2", []string{"Project (1 + 2)", "  Dual"}},
		{"SELECT 1 WHERE 1 = 1", []string{"Project 1", "  Filter (1 = 1)", "    Dual"}},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			assert.Equal(t, strings.Join(tc.want, "\n"), Explain(planSQL(t, cat, tc.src).Root))
		})
	}

	t.Run("without indexes", func(t *testing.T) {
		got := Explain(planSQL(t, cat, "SELECT * FROM people WHERE id = 5", WithoutIndexes()).Root)
		assert.Equal(t, strings.Join([]string{all, "  Filter (id = 5)", "    SeqScan people"}, "\n"), got)
	})
}

func TestSelectResults(t *testing.T) {
	cat, _ := newDB(t)
	seedPeople(t, cat)

	tests := []struct {
		src  string
		want [][]types.Value
	}{
		{"SELECT id, name FROM people WHERE id = 5", [][]types.Value{{iv(5), sv("n5")}}},
		{"SELECT id FROM people WHERE id >= 98", [][]types.Value{{iv(98)}, {iv(99)}, {iv(100)}}},
		{"SELECT id FROM people WHERE id < 3", [][]types.Value{{iv(1)}, {iv(2)}}},
		{"SELECT id FROM people WHERE id > 100", nil},
		{"SELECT id FROM people WHERE id > 10 AND id < 5", nil},
		{"SELECT id FROM people ORDER BY id DESC LIMIT 3", [][]types.Value{{iv(100)}, {iv(99)}, {iv(98)}}},
		{"SELECT id FROM people ORDER BY id LIMIT 2", [][]types.Value{{iv(1)}, {iv(2)}}},
		{"SELECT id FROM people ORDER BY age LIMIT 3", [][]types.Value{{iv(1)}, {iv(2)}}[:0:0]}, // replaced below
		{"SELECT id * 2, name FROM people WHERE id = 7", [][]types.Value{{iv(14), sv("n7")}}},
		{"SELECT age FROM people WHERE id = 10", [][]types.Value{{nv(types.Int)}}},
		{"SELECT score FROM people WHERE id = 6", [][]types.Value{{fv(1.5)}}},
		{"SELECT id FROM people WHERE age = 1005", [][]types.Value{{iv(5)}}},
		{"SELECT id FROM people WHERE age IS NULL AND id < 25", [][]types.Value{{iv(10)}, {iv(20)}}},
		{"SELECT id FROM people WHERE id = 1 LIMIT 0", nil},
		{"SELECT 1 + 2 WHERE 1 = 1", [][]types.Value{{iv(3)}}},
		{"SELECT 1 WHERE 1 = 2", nil},
		{"SELECT 1 WHERE NULL", nil},
		{"SELECT 1 LIMIT 0", nil},
	}
	// NULL is the largest value: it ends an ascending sort and starts a descending one.
	tests[7].want = [][]types.Value{{iv(1)}, {iv(2)}, {iv(3)}}
	tests = append(tests, struct {
		src  string
		want [][]types.Value
	}{"SELECT id FROM people ORDER BY age DESC LIMIT 2", [][]types.Value{{iv(10)}, {iv(20)}}}) // NULL ties keep heap order

	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			assert.Equal(t, tc.want, rowsOf(t, cat, tc.src))
		})
	}
}

func TestPlanColumns(t *testing.T) {
	cat, _ := newDB(t)
	seedPeople(t, cat)
	plan := planSQL(t, cat, "SELECT id, name AS n, id + 1, score, NULL FROM people")
	assert.Equal(t, []Column{
		{Name: "id", Type: types.Int},
		{Name: "n", Type: types.Text},
		{Name: "?column?", Type: types.Int},
		{Name: "score", Type: types.Float},
		{Name: "?column?", Type: types.Text},
	}, plan.Columns)
}

func TestIndexEntryWithoutARowIsAnError(t *testing.T) {
	cat, _ := newDB(t)
	people, idIx := seedPeople(t, cat)
	require.NoError(t, idIx.Tree.Insert(9999, table.RID{Page: people.Heap.FirstPage(), Slot: 500}))

	_, err := planSQL(t, cat, "SELECT * FROM people WHERE id = 9999").Rows()
	assert.ErrorContains(t, err, "points at a missing row")
}

func TestRuntimeErrorsComeOutOfPlans(t *testing.T) {
	cat, _ := newDB(t)
	seedPeople(t, cat)
	_, err := planSQL(t, cat, "SELECT 10 / (id - 5) FROM people").Rows()
	var re *RuntimeError
	require.ErrorAs(t, err, &re)
	assert.Contains(t, re.Msg, "division by zero")

	// with an index on the WHERE, row 5 is never looked at
	rows := rowsOf(t, cat, "SELECT 10 / (id - 5) FROM people WHERE id > 5 AND id <= 7")
	assert.Equal(t, [][]types.Value{{iv(10)}, {iv(5)}}, rows)
}

func TestOperatorsHoldNoPinsBetweenRows(t *testing.T) {
	cat, pool := newDB(t)
	seedPeople(t, cat)
	for _, q := range []string{
		"SELECT * FROM people",
		"SELECT * FROM people WHERE id > 10",
		"SELECT * FROM people ORDER BY id LIMIT 50",
		"SELECT * FROM people ORDER BY name",
	} {
		plan := planSQL(t, cat, q)
		_, ok, err := plan.Root.Next()
		require.NoError(t, err, q)
		require.True(t, ok, q)
		for _, fi := range pool.Frames() {
			assert.Zero(t, fi.Pins, "%s: frame %d", q, fi.Frame)
		}
	}
}

// Random queries, run once with indexes and once without. The answers must
// be the same, so the index plans are only ever a faster way to the same rows.
func TestIndexPlansMatchSequentialScans(t *testing.T) {
	cat, _ := newDB(t)
	seedPeople(t, cat)
	rng := rand.New(rand.NewSource(1))
	ops := []string{"=", "<", "<=", ">", ">="}
	term := func() string {
		switch rng.Intn(12) {
		case 0, 1, 2:
			return fmt.Sprintf("id %s %d", ops[rng.Intn(5)], rng.Intn(125)-5)
		case 3:
			return fmt.Sprintf("%d %s id", rng.Intn(125)-5, ops[rng.Intn(5)])
		case 4, 5:
			return fmt.Sprintf("age %s %d", ops[rng.Intn(5)], 995+rng.Intn(120))
		case 6:
			return fmt.Sprintf("name = 'n%d'", rng.Intn(110))
		case 7:
			return fmt.Sprintf("score > %d.5", rng.Intn(30))
		case 8:
			return fmt.Sprintf("id <> %d", rng.Intn(110))
		case 9:
			return []string{"age IS NULL", "age IS NOT NULL", "id IS NULL"}[rng.Intn(3)]
		case 10:
			return fmt.Sprintf("NOT (id < %d)", rng.Intn(110))
		}
		return fmt.Sprintf("(id < %d OR id > %d)", rng.Intn(60), 40+rng.Intn(70))
	}
	orders := []string{"", "ORDER BY id", "ORDER BY id DESC", "ORDER BY age", "ORDER BY age DESC", "ORDER BY name"}

	usedIndex := 0
	for i := 0; i < 3000; i++ {
		q := "SELECT id, name, age FROM people"
		if n := rng.Intn(4); n > 0 {
			terms := make([]string, n)
			for j := range terms {
				terms[j] = term()
			}
			q += " WHERE " + strings.Join(terms, " AND ")
		}
		order := orders[rng.Intn(len(orders))]
		if order != "" {
			q += " " + order
			if rng.Intn(2) == 0 { // a LIMIT without ORDER BY would pick different rows
				q += fmt.Sprintf(" LIMIT %d", rng.Intn(13))
			}
		}

		withIndexes := planSQL(t, cat, q)
		if strings.Contains(Explain(withIndexes.Root), "IndexScan") {
			usedIndex++
		}
		got, err := withIndexes.Rows()
		require.NoError(t, err, q)
		want := rowsOf(t, cat, q, WithoutIndexes())

		if order == "" { // no order was asked for, so compare as sets
			byID := func(rows [][]types.Value) {
				sort.SliceStable(rows, func(a, b int) bool { return rows[a][0].Int() < rows[b][0].Int() })
			}
			byID(got)
			byID(want)
		}
		require.Equal(t, want, got, q)
	}
	assert.Greater(t, usedIndex, 500, "the test must really use the index plans")
}
