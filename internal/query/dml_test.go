package query

import (
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wilfex81/DB_Engine/internal/catalog"
	"github.com/wilfex81/DB_Engine/internal/sql"
	"github.com/wilfex81/DB_Engine/internal/table"
	"github.com/wilfex81/DB_Engine/internal/types"
)

// newT makes t(id INT NOT NULL, a INT, b INT, name TEXT) with unique indexes on id and a.
func newT(tb testing.TB) (*catalog.Catalog, *catalog.Table) {
	tb.Helper()
	cat, _ := newDB(tb)
	tbl, err := cat.CreateTable("t", []types.Column{
		{Name: "id", Type: types.Int, NotNull: true},
		{Name: "a", Type: types.Int},
		{Name: "b", Type: types.Int},
		{Name: "name", Type: types.Text},
	})
	require.NoError(tb, err)
	_, err = cat.CreateIndex("t", "t_id", "id")
	require.NoError(tb, err)
	_, err = cat.CreateIndex("t", "t_a", "a")
	require.NoError(tb, err)
	return cat, tbl
}

func execSQL(tb testing.TB, cat *catalog.Catalog, src string, opts ...PlanOption) (int64, error) {
	tb.Helper()
	s, err := sql.ParseStatement(src)
	require.NoError(tb, err, src)
	b, err := Bind(NewCatalog(cat), s)
	require.NoError(tb, err, src)
	switch b := b.(type) {
	case *Insert:
		return ExecInsert(cat, b)
	case *Update:
		return ExecUpdate(cat, b, opts...)
	case *Delete:
		return ExecDelete(cat, b, opts...)
	}
	tb.Fatalf("%s is not a data statement", src)
	return 0, nil
}

func mustExec(tb testing.TB, cat *catalog.Catalog, src string, want int64) {
	tb.Helper()
	n, err := execSQL(tb, cat, src)
	require.NoError(tb, err, src)
	require.Equal(tb, want, n, src)
}

// tableRows reads the table by scanning the heap, so it does not trust the indexes.
func tableRows(tb testing.TB, cat *catalog.Catalog) [][]types.Value {
	tb.Helper()
	return rowsOf(tb, cat, "SELECT id, a, b, name FROM t ORDER BY id", WithoutIndexes())
}

// seedRows is what newSeeded puts in the table: id i, a 10*i, b NULL, name "n<i>".
func seedRows() [][]types.Value {
	var rows [][]types.Value
	for i := int64(1); i <= 5; i++ {
		rows = append(rows, []types.Value{iv(i), iv(10 * i), nv(types.Int), sv(fmt.Sprintf("n%d", i))})
	}
	return rows
}

func newSeeded(tb testing.TB) (*catalog.Catalog, *catalog.Table) {
	tb.Helper()
	cat, tbl := newT(tb)
	mustExec(tb, cat, "INSERT INTO t (id, a, name) VALUES (1, 10, 'n1'), (2, 20, 'n2'), (3, 30, 'n3'), (4, 40, 'n4'), (5, 50, 'n5')", 5)
	return cat, tbl
}

func ridsOf(tb testing.TB, tbl *catalog.Table) map[int64]table.RID {
	tb.Helper()
	out := map[int64]table.RID{}
	it := tbl.Heap.Scan()
	for it.Next() {
		out[it.Values()[0].Int()] = it.RID()
	}
	require.NoError(tb, it.Err())
	return out
}

func TestInsertBasics(t *testing.T) {
	cat, tbl := newT(t)
	mustExec(t, cat, "INSERT INTO t VALUES (1, 10, NULL, 'x'), (2, 20, 5, 'y')", 2)
	mustExec(t, cat, "INSERT INTO t (id, name) VALUES (3, 'z')", 1)
	mustExec(t, cat, "INSERT INTO t (name, id) VALUES ('w', 4)", 1)

	assert.Equal(t, [][]types.Value{
		{iv(1), iv(10), nv(types.Int), sv("x")},
		{iv(2), iv(20), iv(5), sv("y")},
		{iv(3), nv(types.Int), nv(types.Int), sv("z")},
		{iv(4), nv(types.Int), nv(types.Int), sv("w")},
	}, tableRows(t, cat))
	require.NoError(t, CheckTable(tbl))
	assert.Equal(t, [][]types.Value{{sv("y")}}, rowsOf(t, cat, "SELECT name FROM t WHERE a = 20"), "found through the index")
}

func TestInsertIsAllOrNothing(t *testing.T) {
	seed := [][]types.Value{{iv(1), iv(10), nv(types.Int), sv("seed")}}
	tests := []struct {
		name   string
		src    string
		unique bool
		msg    string
	}{
		{"the same id twice", "INSERT INTO t (id, a) VALUES (5, 50), (5, 51)", true, ""},
		{"an id that exists, in the second row", "INSERT INTO t (id, a) VALUES (2, 20), (1, 99)", true, ""},
		{"an a that exists", "INSERT INTO t (id, a) VALUES (6, 10)", true, ""},
		{"the same a twice", "INSERT INTO t (id, a) VALUES (7, 70), (8, 70)", true, ""},
		{"NOT NULL found while running", "INSERT INTO t (id) VALUES (11), (NULL + 1)", false, "cannot be NULL"},
		{"division by zero in the second row", "INSERT INTO t (id) VALUES (12), (1 / 0)", false, "division by zero"},
		{"a row larger than a page", "INSERT INTO t (id, name) VALUES (13, '" + strings.Repeat("a", 5000) + "')", false, "more than a page"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cat, tbl := newT(t)
			mustExec(t, cat, "INSERT INTO t (id, a, name) VALUES (1, 10, 'seed')", 1)

			_, err := execSQL(t, cat, tc.src)
			require.Error(t, err)
			if tc.unique {
				assert.ErrorIs(t, err, ErrUniqueViolation)
			} else {
				assert.ErrorContains(t, err, tc.msg)
			}
			assert.Equal(t, seed, tableRows(t, cat), "nothing may have been written")
			require.NoError(t, CheckTable(tbl))
		})
	}

	t.Run("NULL keys never clash", func(t *testing.T) {
		cat, tbl := newT(t)
		mustExec(t, cat, "INSERT INTO t (id, a) VALUES (9, NULL), (10, NULL)", 2)
		require.NoError(t, CheckTable(tbl))
	})
}

func TestInsertUndoesWhenAWriteFails(t *testing.T) {
	cat, tbl := newT(t)
	mustExec(t, cat, "INSERT INTO t (id, a, name) VALUES (1, 10, 'seed')", 1)
	aIx, ok := tbl.IndexOn("a")
	require.True(t, ok)

	// the check passes, then a key appears in the index before the writes start
	beforeWriteHook = func() {
		require.NoError(t, aIx.Tree.Insert(70, table.RID{Page: tbl.Heap.FirstPage(), Slot: 900}))
	}
	t.Cleanup(func() { beforeWriteHook = nil })
	_, err := execSQL(t, cat, "INSERT INTO t (id, a) VALUES (20, 60), (21, 70), (22, 80)")
	assert.ErrorIs(t, err, ErrUniqueViolation)
	beforeWriteHook = nil

	require.NoError(t, aIx.Tree.Delete(70)) // the entry the test planted
	assert.Equal(t, [][]types.Value{{iv(1), iv(10), nv(types.Int), sv("seed")}}, tableRows(t, cat), "the first row is taken out again")
	require.NoError(t, CheckTable(tbl))
}

func TestUpdate(t *testing.T) {
	t.Run("one row", func(t *testing.T) {
		cat, tbl := newSeeded(t)
		mustExec(t, cat, "UPDATE t SET name = 'renamed' WHERE id = 3", 1)
		want := seedRows()
		want[2][3] = sv("renamed")
		assert.Equal(t, want, tableRows(t, cat))
		require.NoError(t, CheckTable(tbl))
	})

	t.Run("an indexed column", func(t *testing.T) {
		cat, tbl := newSeeded(t)
		mustExec(t, cat, "UPDATE t SET a = a + 1", 5)
		want := seedRows()
		for i := range want {
			want[i][1] = iv(int64(10*(i+1) + 1))
		}
		assert.Equal(t, want, tableRows(t, cat))
		assert.Equal(t, [][]types.Value{{iv(3)}}, rowsOf(t, cat, "SELECT id FROM t WHERE a = 31"))
		assert.Empty(t, rowsOf(t, cat, "SELECT id FROM t WHERE a = 30"))
		require.NoError(t, CheckTable(tbl))
	})

	t.Run("keys shift", func(t *testing.T) {
		cat, tbl := newSeeded(t)
		mustExec(t, cat, "UPDATE t SET id = id + 1", 5) // 1..5 become 2..6, which PostgreSQL would refuse
		want := seedRows()
		for i := range want {
			want[i][0] = iv(int64(i + 2))
		}
		assert.Equal(t, want, tableRows(t, cat))
		require.NoError(t, CheckTable(tbl))
	})

	t.Run("keys permute", func(t *testing.T) {
		cat, tbl := newSeeded(t)
		mustExec(t, cat, "UPDATE t SET id = 6 - id", 5)
		var want [][]types.Value
		for j := int64(1); j <= 5; j++ {
			old := 6 - j
			want = append(want, []types.Value{iv(j), iv(10 * old), nv(types.Int), sv(fmt.Sprintf("n%d", old))})
		}
		assert.Equal(t, want, tableRows(t, cat))
		require.NoError(t, CheckTable(tbl))
	})

	t.Run("a key becomes NULL", func(t *testing.T) {
		cat, tbl := newSeeded(t)
		mustExec(t, cat, "UPDATE t SET a = NULL WHERE id <= 2", 2)
		want := seedRows()
		want[0][1], want[1][1] = nv(types.Int), nv(types.Int)
		assert.Equal(t, want, tableRows(t, cat))
		assert.Empty(t, rowsOf(t, cat, "SELECT id FROM t WHERE a = 10"))
		assert.Equal(t, [][]types.Value{{iv(3)}}, rowsOf(t, cat, "SELECT id FROM t WHERE a = 30"))
		require.NoError(t, CheckTable(tbl))
	})

	t.Run("SET reads the old row", func(t *testing.T) {
		cat, tbl := newSeeded(t)
		mustExec(t, cat, "UPDATE t SET b = a", 5)
		mustExec(t, cat, "UPDATE t SET a = b + 1, b = a", 5) // b gets the old a, not the new one
		var want [][]types.Value
		for i := int64(1); i <= 5; i++ {
			want = append(want, []types.Value{iv(i), iv(10*i + 1), iv(10 * i), sv(fmt.Sprintf("n%d", i))})
		}
		assert.Equal(t, want, tableRows(t, cat))
		require.NoError(t, CheckTable(tbl))
	})

	t.Run("matches are counted", func(t *testing.T) {
		cat, _ := newSeeded(t)
		mustExec(t, cat, "UPDATE t SET name = name WHERE id <= 3", 3) // nothing changes, three rows match
		mustExec(t, cat, "UPDATE t SET name = 'x' WHERE id = 99", 0)
		assert.Equal(t, seedRows(), tableRows(t, cat))
	})

	t.Run("a unique violation changes nothing", func(t *testing.T) {
		for _, src := range []string{
			"UPDATE t SET id = 3",                // five rows get the same key
			"UPDATE t SET id = 3 WHERE id = 1",   // 3 belongs to a row that stays
			"UPDATE t SET a = 20 WHERE id = 1",   // 20 belongs to a row that stays
			"UPDATE t SET a = 99 WHERE id < 3",   // two rows get the same key
		} {
			cat, tbl := newSeeded(t)
			_, err := execSQL(t, cat, src)
			assert.ErrorIs(t, err, ErrUniqueViolation, src)
			assert.Equal(t, seedRows(), tableRows(t, cat), src)
			require.NoError(t, CheckTable(tbl), src)
		}
	})

	t.Run("an error while computing changes nothing", func(t *testing.T) {
		cat, tbl := newSeeded(t)
		_, err := execSQL(t, cat, "UPDATE t SET a = 1 / (id - 3)")
		var re *RuntimeError
		require.ErrorAs(t, err, &re)
		assert.Contains(t, re.Msg, "division by zero")

		_, err = execSQL(t, cat, "UPDATE t SET id = NULL + 1")
		assert.ErrorContains(t, err, "cannot be NULL")

		assert.Equal(t, seedRows(), tableRows(t, cat))
		require.NoError(t, CheckTable(tbl))
	})

	t.Run("rows that move keep their index entries", func(t *testing.T) {
		cat, tbl := newT(t)
		var values []string
		for i := 1; i <= 30; i++ {
			values = append(values, fmt.Sprintf("(%d, %d, '%s')", i, i, strings.Repeat("x", 100)))
		}
		mustExec(t, cat, "INSERT INTO t (id, a, name) VALUES "+strings.Join(values, ", "), 30)
		before := ridsOf(t, tbl)

		mustExec(t, cat, "UPDATE t SET name = '"+strings.Repeat("y", 400)+"'", 30)
		after := ridsOf(t, tbl)
		moved := 0
		for id, rid := range after {
			if rid != before[id] {
				moved++
			}
		}
		assert.Greater(t, moved, 0, "the page is full, so some rows must have moved")
		require.NoError(t, CheckTable(tbl))
		assert.Equal(t, [][]types.Value{{iv(17)}}, rowsOf(t, cat, "SELECT id FROM t WHERE a = 17"))
	})
}

func TestDelete(t *testing.T) {
	t.Run("by key, and the key can be used again", func(t *testing.T) {
		cat, tbl := newSeeded(t)
		mustExec(t, cat, "DELETE FROM t WHERE id = 3", 1)
		assert.Len(t, tableRows(t, cat), 4)
		mustExec(t, cat, "INSERT INTO t (id, a) VALUES (3, 30)", 1)
		require.NoError(t, CheckTable(tbl))
	})

	t.Run("a range", func(t *testing.T) {
		cat, tbl := newSeeded(t)
		mustExec(t, cat, "DELETE FROM t WHERE id > 3", 2)
		assert.Equal(t, seedRows()[:3], tableRows(t, cat))
		require.NoError(t, CheckTable(tbl))
	})

	t.Run("by a column without an index", func(t *testing.T) {
		cat, tbl := newSeeded(t)
		mustExec(t, cat, "DELETE FROM t WHERE name = 'n2'", 1)
		require.NoError(t, CheckTable(tbl))
	})

	t.Run("rows with a NULL key", func(t *testing.T) {
		cat, tbl := newSeeded(t)
		mustExec(t, cat, "UPDATE t SET a = NULL WHERE id = 1", 1)
		mustExec(t, cat, "DELETE FROM t WHERE a IS NULL", 1)
		assert.Len(t, tableRows(t, cat), 4)
		require.NoError(t, CheckTable(tbl))
	})

	t.Run("everything", func(t *testing.T) {
		cat, tbl := newSeeded(t)
		mustExec(t, cat, "DELETE FROM t", 5)
		assert.Empty(t, tableRows(t, cat))
		assert.Empty(t, rowsOf(t, cat, "SELECT id FROM t WHERE id > 0"))
		require.NoError(t, CheckTable(tbl))
	})

	t.Run("nothing", func(t *testing.T) {
		cat, _ := newSeeded(t)
		mustExec(t, cat, "DELETE FROM t WHERE id = 99", 0)
		assert.Equal(t, seedRows(), tableRows(t, cat))
	})

	t.Run("an error while looking for rows deletes nothing", func(t *testing.T) {
		cat, tbl := newSeeded(t)
		_, err := execSQL(t, cat, "DELETE FROM t WHERE 1 / (id - 3) > 0")
		var re *RuntimeError
		require.ErrorAs(t, err, &re)
		assert.Equal(t, seedRows(), tableRows(t, cat))
		require.NoError(t, CheckTable(tbl))
	})
}

func TestCheckTableDetectsDamage(t *testing.T) {
	tests := []struct {
		name   string
		damage func(t *testing.T, tbl *catalog.Table)
	}{
		{"an entry is missing", func(t *testing.T, tbl *catalog.Table) {
			ix, _ := tbl.IndexOn("a")
			require.NoError(t, ix.Tree.Delete(20))
		}},
		{"an entry has no row", func(t *testing.T, tbl *catalog.Table) {
			ix, _ := tbl.IndexOn("a")
			require.NoError(t, ix.Tree.Insert(999, ridsOf(t, tbl)[1]))
		}},
		{"an entry points at the wrong row", func(t *testing.T, tbl *catalog.Table) {
			ix, _ := tbl.IndexOn("a")
			require.NoError(t, ix.Tree.Delete(20))
			require.NoError(t, ix.Tree.Insert(20, ridsOf(t, tbl)[3]))
		}},
		{"two rows have the same key", func(t *testing.T, tbl *catalog.Table) {
			_, err := tbl.Heap.Insert([]types.Value{iv(1), iv(77), nv(types.Int), sv("copy")})
			require.NoError(t, err)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, tbl := newSeeded(t)
			require.NoError(t, CheckTable(tbl))
			tc.damage(t, tbl)
			assert.Error(t, CheckTable(tbl))
		})
	}
}

// ---- random statements against a model

type mrow struct {
	id   int64
	hasA bool
	a    int64
	name string
}

func (r mrow) values() []types.Value {
	a := nv(types.Int)
	if r.hasA {
		a = iv(r.a)
	}
	return []types.Value{iv(r.id), a, nv(types.Int), sv(r.name)}
}

// unique reports whether no id repeats and no non-NULL a repeats.
func unique(rows []mrow) bool {
	ids, as := map[int64]bool{}, map[int64]bool{}
	for _, r := range rows {
		if ids[r.id] {
			return false
		}
		ids[r.id] = true
		if r.hasA {
			if as[r.a] {
				return false
			}
			as[r.a] = true
		}
	}
	return true
}

type predicate struct {
	sql string
	f   func(mrow) bool
}

func randPred(rng *rand.Rand, depth int) predicate {
	k := rng.Int63n(45)
	switch n := rng.Intn(9); {
	case n == 0:
		return predicate{fmt.Sprintf("id < %d", k), func(r mrow) bool { return r.id < k }}
	case n == 1:
		return predicate{fmt.Sprintf("id >= %d", k), func(r mrow) bool { return r.id >= k }}
	case n == 2:
		return predicate{fmt.Sprintf("id = %d", k), func(r mrow) bool { return r.id == k }}
	case n == 3:
		return predicate{fmt.Sprintf("a = %d", k), func(r mrow) bool { return r.hasA && r.a == k }}
	case n == 4:
		return predicate{"a IS NULL", func(r mrow) bool { return !r.hasA }}
	case n == 5:
		m := 2 + rng.Int63n(4)
		rem := rng.Int63n(m)
		return predicate{fmt.Sprintf("id %% %d = %d", m, rem), func(r mrow) bool { return r.id%m == rem }}
	case n == 6:
		return predicate{fmt.Sprintf("NOT (id > %d)", k), func(r mrow) bool { return r.id <= k }}
	case n == 7 && depth == 0:
		p, q := randPred(rng, 1), randPred(rng, 1)
		return predicate{"(" + p.sql + " AND " + q.sql + ")", func(r mrow) bool { return p.f(r) && q.f(r) }}
	}
	return predicate{fmt.Sprintf("a > %d", k), func(r mrow) bool { return r.hasA && r.a > k }}
}

func TestRandomStatementsAgainstAModel(t *testing.T) {
	for _, mode := range []struct {
		name string
		opts []PlanOption
	}{{"with indexes", nil}, {"without indexes", []PlanOption{WithoutIndexes()}}} {
		t.Run(mode.name, func(t *testing.T) {
			rng := rand.New(rand.NewSource(1))
			cat, tbl := newT(t)
			var model []mrow
			counts := map[string]int{}

			for step := 0; step < 1200; step++ {
				var (
					text      string
					next      []mrow
					affected  int64
					violation bool
				)
				switch kind := rng.Intn(10); {
				case kind < 4: // INSERT
					n := 1 + rng.Intn(3)
					var parts []string
					next = append(next, model...)
					for i := 0; i < n; i++ {
						r := mrow{id: 1 + rng.Int63n(45), name: strings.Repeat("x", rng.Intn(300))}
						aText := "NULL"
						if rng.Intn(10) >= 3 {
							r.hasA, r.a = true, 1+rng.Int63n(45)
							aText = strconv.FormatInt(r.a, 10)
						}
						parts = append(parts, fmt.Sprintf("(%d, %s, '%s')", r.id, aText, r.name))
						next = append(next, r)
					}
					text = "INSERT INTO t (id, a, name) VALUES " + strings.Join(parts, ", ")
					affected, violation = int64(n), !unique(next)
					counts["insert"]++

				case kind < 8: // UPDATE
					p := randPred(rng, 0)
					var set string
					var fn func(mrow) mrow
					switch rng.Intn(8) {
					case 0:
						set, fn = "a = a + 1", func(r mrow) mrow {
							if r.hasA {
								r.a++
							}
							return r
						}
					case 1:
						set, fn = "a = NULL", func(r mrow) mrow { r.hasA, r.a = false, 0; return r }
					case 2:
						set, fn = "a = id", func(r mrow) mrow { r.hasA, r.a = true, r.id; return r }
					case 3:
						set, fn = "id = id + 100", func(r mrow) mrow { r.id += 100; return r }
					case 4:
						set, fn = "id = 41 - id", func(r mrow) mrow { r.id = 41 - r.id; return r }
					case 5:
						s := strings.Repeat("y", rng.Intn(300))
						set, fn = "name = '"+s+"'", func(r mrow) mrow { r.name = s; return r }
					case 6:
						set, fn = "a = id + 1", func(r mrow) mrow { r.hasA, r.a = true, r.id+1; return r }
					default:
						set, fn = "id = id + 1", func(r mrow) mrow { r.id++; return r }
					}
					text = "UPDATE t SET " + set + " WHERE " + p.sql
					for _, r := range model {
						if p.f(r) {
							affected++
							r = fn(r)
						}
						next = append(next, r)
					}
					violation = !unique(next)
					counts["update"]++

				default: // DELETE
					p := randPred(rng, 0)
					text = "DELETE FROM t WHERE " + p.sql
					for _, r := range model {
						if p.f(r) {
							affected++
						} else {
							next = append(next, r)
						}
					}
					counts["delete"]++
				}

				n, err := execSQL(t, cat, text, mode.opts...)
				if violation {
					require.ErrorIs(t, err, ErrUniqueViolation, "step %d: %s", step, text)
					counts["violation"]++
				} else {
					require.NoError(t, err, "step %d: %s", step, text)
					require.Equal(t, affected, n, "step %d: %s", step, text)
					model = next
				}

				sorted := append([]mrow(nil), model...)
				sort.Slice(sorted, func(i, j int) bool { return sorted[i].id < sorted[j].id })
				var want [][]types.Value
				for _, r := range sorted {
					want = append(want, r.values())
				}
				require.Equal(t, want, tableRows(t, cat), "step %d: %s", step, text)
				require.NoError(t, CheckTable(tbl), "step %d: %s", step, text)
			}
			assert.Greater(t, counts["violation"], 20, "the test must really hit unique violations")
			assert.Greater(t, counts["update"], 300)
		})
	}
}