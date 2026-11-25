package sql

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func parseStmtOK(tb testing.TB, src string) Stmt {
	tb.Helper()
	s, err := ParseStatement(src)
	require.NoError(tb, err, "source %q", src)
	return s
}

// The expected text is the canonical printed form.
func TestParseStatements(t *testing.T) {
	tests := []struct{ src, want string }{
		// CREATE and DROP
		{"CREATE TABLE t (a INT)", "CREATE TABLE t (a INT)"},
		{"create table People (id integer primary key, name text not null, score real, ok boolean, d double)",
			"CREATE TABLE People (id INT PRIMARY KEY, name TEXT NOT NULL, score FLOAT, ok BOOL, d FLOAT)"},
		{"CREATE TABLE t (a INT PRIMARY KEY NOT NULL)", "CREATE TABLE t (a INT NOT NULL PRIMARY KEY)"},
		{"CREATE TABLE t (text TEXT, int INT, count FLOAT)", "CREATE TABLE t (text TEXT, int INT, count FLOAT)"},
		{"create unique index ix on t (a)", "CREATE UNIQUE INDEX ix ON t (a)"},
		{"DROP TABLE t", "DROP TABLE t"},
		{"drop index ix", "DROP INDEX ix"},

		// INSERT
		{"INSERT INTO t VALUES (1, 'a')", "INSERT INTO t VALUES (1, 'a')"},
		{"insert into t (a, b) values (1, 'x'), (2, NULL)", "INSERT INTO t (a, b) VALUES (1, 'x'), (2, NULL)"},
		{"INSERT INTO t VALUES (1 + 2, -3)", "INSERT INTO t VALUES ((1 + 2), -3)"},

		// SELECT
		{"SELECT 1", "SELECT 1"},
		{"SELECT 1 + 2 AS three", "SELECT (1 + 2) AS three"},
		{"select * from t", "SELECT * FROM t"},
		{"SELECT t.*, a x, b AS y FROM tbl t", "SELECT t.*, a AS x, b AS y FROM tbl AS t"},
		{"SELECT a FROM t WHERE a > 1 AND b IS NULL ORDER BY a DESC, b ASC, c LIMIT 10",
			"SELECT a FROM t WHERE ((a > 1) AND (b IS NULL)) ORDER BY a DESC, b, c LIMIT 10"},
		{"SELECT count(*) FROM t GROUP BY a, b", "SELECT count(*) FROM t GROUP BY a, b"},
		{"SELECT * FROM a JOIN b ON a.x = b.y INNER JOIN c cc ON TRUE",
			"SELECT * FROM a JOIN b ON (a.x = b.y) JOIN c AS cc ON TRUE"},
		{"SELECT a FROM t LIMIT 0", "SELECT a FROM t LIMIT 0"},
		{"SELECT a IS NULL flag FROM t", "SELECT (a IS NULL) AS flag FROM t"},

		// UPDATE and DELETE
		{"UPDATE t SET a = 1", "UPDATE t SET a = 1"},
		{"update t set a = a + 1, b = 'x' where id = 5", "UPDATE t SET a = (a + 1), b = 'x' WHERE (id = 5)"},
		{"DELETE FROM t", "DELETE FROM t"},
		{"delete from t where a < 3", "DELETE FROM t WHERE (a < 3)"},

		// a trailing semicolon, comments
		{"SELECT 1;", "SELECT 1"},
		{"-- first\nSELECT 1 -- second\n;", "SELECT 1"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			assert.Equal(t, tc.want, parseStmtOK(t, tc.src).String())
		})
	}
}

func TestSelectStructure(t *testing.T) {
	s := parseStmtOK(t, "SELECT a x, t.*, * FROM tbl AS t JOIN u ON t.id = u.id WHERE a GROUP BY a ORDER BY a DESC LIMIT 5").(*Select)

	require.Len(t, s.Items, 3)
	assert.Equal(t, "x", s.Items[0].Alias)
	assert.Equal(t, "a", s.Items[0].Expr.String())
	assert.Equal(t, "t", s.Items[1].TableStar)
	assert.True(t, s.Items[2].Star)

	require.NotNil(t, s.From)
	assert.Equal(t, "tbl", s.From.Name)
	assert.Equal(t, "t", s.From.Alias)
	require.Len(t, s.Joins, 1)
	assert.Equal(t, "u", s.Joins[0].Table.Name)
	assert.Equal(t, "(t.id = u.id)", s.Joins[0].On.String())
	assert.Equal(t, "a", s.Where.String())
	assert.Len(t, s.GroupBy, 1)
	require.Len(t, s.OrderBy, 1)
	assert.True(t, s.OrderBy[0].Desc)
	require.NotNil(t, s.Limit)
	assert.Equal(t, int64(5), *s.Limit)

	bare := parseStmtOK(t, "SELECT 1 + 2").(*Select)
	assert.Nil(t, bare.From, "a SELECT may have no FROM")
	assert.Nil(t, bare.Where)
	assert.Nil(t, bare.Limit)
}

func TestCreateTableStructure(t *testing.T) {
	s := parseStmtOK(t, "CREATE TABLE People (id INTEGER PRIMARY KEY, name TEXT NOT NULL, score DOUBLE)").(*CreateTable)
	assert.Equal(t, "People", s.Name)
	require.Len(t, s.Columns, 3)

	assert.Equal(t, "id", s.Columns[0].Name)
	assert.Equal(t, TypeInt, s.Columns[0].Type)
	assert.True(t, s.Columns[0].PrimaryKey)
	assert.False(t, s.Columns[0].NotNull, "PRIMARY KEY is recorded as written, the binder adds NOT NULL")
	assert.Equal(t, TypeText, s.Columns[1].Type)
	assert.True(t, s.Columns[1].NotNull)
	assert.Equal(t, TypeFloat, s.Columns[2].Type)
	assert.Equal(t, 21, s.Columns[0].At.Offset, "a column points at its name")
}

func TestInsertStructure(t *testing.T) {
	s := parseStmtOK(t, "INSERT INTO t (a, b) VALUES (1, 2),\n  (3, 4)").(*Insert)
	assert.Equal(t, "t", s.Table)
	assert.Equal(t, []string{"a", "b"}, s.Columns)
	require.Len(t, s.Rows, 2)
	assert.Len(t, s.Rows[0].Values, 2)
	assert.Equal(t, Pos{Offset: 28, Line: 1, Col: 29}, s.Rows[0].At)
	assert.Equal(t, Pos{Offset: 38, Line: 2, Col: 3}, s.Rows[1].At)

	plain := parseStmtOK(t, "INSERT INTO t VALUES (1)").(*Insert)
	assert.Nil(t, plain.Columns, "no column list is nil, not empty")
}

func TestUpdateStructure(t *testing.T) {
	s := parseStmtOK(t, "UPDATE t SET a = 1, b = b + 1 WHERE id = 7").(*Update)
	assert.Equal(t, "t", s.Table)
	require.Len(t, s.Set, 2)
	assert.Equal(t, "a", s.Set[0].Column)
	assert.Equal(t, "1", s.Set[0].Value.String())
	assert.Equal(t, "(b + 1)", s.Set[1].Value.String())
	assert.Equal(t, "(id = 7)", s.Where.String())
}

func TestScripts(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{"empty", "", nil},
		{"only a semicolon", ";", nil},
		{"only a comment", "-- nothing\n", nil},
		{"two statements", "SELECT 1; SELECT 2", []string{"SELECT 1", "SELECT 2"}},
		{"empty statements between", "SELECT 1;;; ;SELECT 2;", []string{"SELECT 1", "SELECT 2"}},
		{"a script", "CREATE TABLE t (a INT);\nINSERT INTO t VALUES (1);\nSELECT * FROM t;",
			[]string{"CREATE TABLE t (a INT)", "INSERT INTO t VALUES (1)", "SELECT * FROM t"}},
		{"a semicolon inside a string", "SELECT 'a;b'; SELECT 2", []string{"SELECT 'a;b'", "SELECT 2"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stmts, err := ParseScript(tc.src)
			require.NoError(t, err)
			var got []string
			for _, s := range stmts {
				got = append(got, s.String())
			}
			assert.Equal(t, tc.want, got)
		})
	}

	stmts, err := ParseScript("SELECT 1;\n  DELETE FROM t")
	require.NoError(t, err)
	require.Len(t, stmts, 2)
	assert.Equal(t, Pos{Offset: 0, Line: 1, Col: 1}, stmts[0].Start())
	assert.Equal(t, Pos{Offset: 12, Line: 2, Col: 3}, stmts[1].Start())
}

func TestParseStatementErrors(t *testing.T) {
	tests := []struct {
		src       string
		line, col int
		want      string
	}{
		{"", 1, 1, "expected a statement, found end of input"},
		{"FOO", 1, 1, "expected a statement"},
		{"SELECT 1; SELECT 2", 1, 11, "expected one statement, found 2"},
		{"SELECT 1 SELECT 2", 1, 10, "unexpected SELECT after the statement"},
		{"SELECT @", 1, 8, "unexpected character '@'"},

		// SELECT
		{"SELECT", 1, 7, "expected an expression, found end of input"},
		{"SELECT FROM t", 1, 8, "expected an expression, found FROM"},
		{"SELECT a, FROM t", 1, 11, "expected an expression, found FROM"},
		{"SELECT * FROM", 1, 14, "expected a table name, found end of input"},
		{"SELECT * FROM t, u", 1, 16, "use JOIN"},
		{"SELECT * FROM a INNER b", 1, 23, "expected JOIN after INNER"},
		{"SELECT * FROM a JOIN b", 1, 23, "expected ON, found end of input"},
		{"SELECT a FROM t LIMIT -1", 1, 23, "expected a whole number after LIMIT"},
		{"SELECT a FROM t LIMIT 1.5", 1, 23, "expected a whole number after LIMIT"},
		{"SELECT a FROM t LIMIT 99999999999999999999", 1, 23, "out of range"},
		{"SELECT a FROM t ORDER a", 1, 23, "expected BY"},
		{"SELECT a FROM t GROUP a", 1, 23, "expected BY"},
		{"SELECT a FROM t LIMIT 1 WHERE a", 1, 25, "unexpected WHERE after the statement"},
		{"SELECT a b c FROM t", 1, 12, `unexpected name "c" after the statement`},
		{"SELECT * a", 1, 10, `unexpected name "a" after the statement`},

		// INSERT
		{"INSERT t VALUES (1)", 1, 8, "expected INTO"},
		{"INSERT INTO t", 1, 14, "expected VALUES"},
		{"INSERT INTO t VALUES", 1, 21, `expected "(", found end of input`},
		{"INSERT INTO t VALUES ()", 1, 23, `expected an expression, found ")"`},
		{"INSERT INTO t (a,) VALUES (1)", 1, 18, `expected a column name, found ")"`},
		{"INSERT INTO t VALUES (1,)", 1, 25, `expected an expression, found ")"`},
		{"INSERT INTO t VALUES (1) (2)", 1, 26, `unexpected "(" after the statement`},

		// UPDATE and DELETE
		{"UPDATE t", 1, 9, "expected SET"},
		{"UPDATE t SET", 1, 13, "expected a column name"},
		{"UPDATE t SET a", 1, 15, `expected "="`},
		{"UPDATE t SET a = ", 1, 18, "expected an expression"},
		{"DELETE t", 1, 8, "expected FROM"},
		{"DELETE FROM", 1, 12, "expected a table name"},

		// CREATE and DROP
		{"CREATE INDEX i ON t (a)", 1, 8, "only UNIQUE indexes are supported"},
		{"CREATE UNIQUE i", 1, 15, "expected INDEX"},
		{"CREATE UNIQUE INDEX i ON t (a, b)", 1, 30, "an index covers exactly one column"},
		{"CREATE VIEW v", 1, 8, "expected TABLE or UNIQUE INDEX after CREATE"},
		{"CREATE TABLE t", 1, 15, `expected "("`},
		{"CREATE TABLE t ()", 1, 17, `expected a column name, found ")"`},
		{"CREATE TABLE t (a)", 1, 18, "expected a type"},
		{"CREATE TABLE t (a VARCHAR)", 1, 19, `unknown type "VARCHAR"`},
		{"CREATE TABLE t (a INT,)", 1, 23, `expected a column name, found ")"`},
		{"CREATE TABLE t (a INT NOT)", 1, 26, "expected NULL after NOT"},
		{"CREATE TABLE t (a INT NOT NULL NOT NULL)", 1, 32, "NOT NULL given twice"},
		{"CREATE TABLE t (a INT PRIMARY KEY PRIMARY KEY)", 1, 35, "PRIMARY KEY given twice"},
		{"CREATE TABLE t (a INT PRIMARY)", 1, 30, "expected KEY after PRIMARY"},
		{"DROP t", 1, 6, "expected TABLE or INDEX after DROP"},
		{"DROP TABLE", 1, 11, "expected a table name"},
		{"DROP INDEX", 1, 11, "expected an index name"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			_, err := ParseStatement(tc.src)
			var se *SyntaxError
			require.True(t, errors.As(err, &se), "got %v", err)
			assert.Equal(t, tc.line, se.Pos.Line)
			assert.Equal(t, tc.col, se.Pos.Col)
			assert.Contains(t, se.Msg, tc.want)
		})
	}
}

func TestStatementNestingIsLimited(t *testing.T) {
	src := "SELECT " + strings.Repeat("(", 300) + "1" + strings.Repeat(")", 300)
	_, err := ParseStatement(src)
	assert.ErrorContains(t, err, "nested too deeply")

	// the count starts again for every expression, so a long script is fine
	script := strings.Repeat("SELECT ((((1)))); ", 5000)
	stmts, err := ParseScript(script)
	require.NoError(t, err)
	assert.Len(t, stmts, 5000)
}

func TestHugeInsertIsParsedInLinearTime(t *testing.T) {
	src := "INSERT INTO t VALUES " + strings.Repeat("(1, 'a'), ", 100000) + "(2, 'b')"
	s := parseStmtOK(t, src).(*Insert)
	assert.Len(t, s.Rows, 100001)
}

// The properties: ParseScript never panics, its errors are syntax errors, and
// the printed form of what it parsed parses again to the same printed form. Run
// it for real with:
//
//	go test -fuzz=FuzzParseScript -fuzztime=20s -fuzzminimizetime=0 ./internal/sql/
func FuzzParseScript(f *testing.F) {
	for _, s := range []string{
		"SELECT 1", "SELECT a x, t.* FROM t u JOIN v ON u.a = v.b WHERE a IS NULL GROUP BY a ORDER BY a DESC LIMIT 3",
		"INSERT INTO t (a) VALUES (1), (-2)", "UPDATE t SET a = a + 1 WHERE b", "DELETE FROM t",
		"CREATE TABLE t (a INT PRIMARY KEY, b TEXT NOT NULL)", "CREATE UNIQUE INDEX i ON t (a)", "DROP TABLE t; DROP INDEX i",
		"SELECT 'a;b'; ;; SELECT 2", "SELECT count(*) FROM t",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		stmts, err := ParseScript(src)
		if err != nil {
			var se *SyntaxError
			require.ErrorAs(t, err, &se)
			return
		}
		parts := make([]string, len(stmts))
		for i, s := range stmts {
			parts[i] = s.String()
		}
		printed := strings.Join(parts, "; ")
		if maxParenDepth(printed) > 80 {
			return // the printed form of a long chain is nested more deeply than the limit allows
		}
		again, err := ParseScript(printed)
		require.NoError(t, err, "the printed form %q must parse again", printed)
		require.Len(t, again, len(stmts))
		for i := range again {
			require.Equal(t, parts[i], again[i].String())
		}
	})
}
