package query

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wilfex81/DB_Engine/internal/sql"
	"github.com/wilfex81/DB_Engine/internal/types"
)

type fakeCatalog struct {
	tables  map[string]TableInfo
	indexes map[string]bool
}

func (f fakeCatalog) Table(name string) (TableInfo, bool) {
	t, ok := f.tables[strings.ToLower(name)]
	return t, ok
}

func (f fakeCatalog) HasIndex(name string) bool { return f.indexes[strings.ToLower(name)] }

func schemaOf(tb testing.TB, cols ...types.Column) *types.Schema {
	tb.Helper()
	s, err := types.NewSchema(cols)
	require.NoError(tb, err)
	return s
}

// testCatalog has people(id INT NOT NULL, name TEXT, score FLOAT, active BOOL),
// orders(order_id INT NOT NULL, total FLOAT), and an index called people_id.
func testCatalog(tb testing.TB) Catalog {
	return fakeCatalog{
		tables: map[string]TableInfo{
			"people": {Name: "people", Schema: schemaOf(tb,
				types.Column{Name: "id", Type: types.Int, NotNull: true},
				types.Column{Name: "name", Type: types.Text},
				types.Column{Name: "score", Type: types.Float},
				types.Column{Name: "active", Type: types.Bool},
			)},
			"orders": {Name: "orders", Schema: schemaOf(tb,
				types.Column{Name: "order_id", Type: types.Int, NotNull: true},
				types.Column{Name: "total", Type: types.Float},
			)},
		},
		indexes: map[string]bool{"people_id": true},
	}
}

func bindSQL(tb testing.TB, src string) (Stmt, error) {
	tb.Helper()
	s, err := sql.ParseStatement(src)
	require.NoError(tb, err, "the test statement %q must parse", src)
	return Bind(testCatalog(tb), s)
}

func bindOK(tb testing.TB, src string) Stmt {
	tb.Helper()
	s, err := bindSQL(tb, src)
	require.NoError(tb, err, "source %q", src)
	return s
}

func TestBindSelect(t *testing.T) {
	tests := []struct{ src, want string }{
		// columns and names
		{"SELECT * FROM people", "SELECT id AS id, name AS name, score AS score, active AS active FROM people"},
		{"select P.* from People p", "SELECT id AS id, name AS name, score AS score, active AS active FROM people"},
		{"SELECT ID, Name FROM PEOPLE", "SELECT id AS id, name AS name FROM people"},
		{"SELECT id AS k, name nm FROM people", "SELECT id AS k, name AS nm FROM people"},
		{"SELECT p.id FROM people p WHERE p.id = 1", "SELECT id AS id FROM people WHERE (id = 1)"},

		// INT and FLOAT mix by turning the INT into a FLOAT
		{"SELECT id + 1 FROM people", "SELECT (id + 1) AS ?column? FROM people"},
		{"SELECT id + 1.5 FROM people", "SELECT (CAST(id AS FLOAT) + 1.5) AS ?column? FROM people"},
		{"SELECT p.score > 1 FROM people p", "SELECT (score > 1.0) AS ?column? FROM people"},
		{"SELECT score + 1, 1 + score FROM people", "SELECT (score + 1.0) AS ?column?, (1.0 + score) AS ?column? FROM people"},
		{"SELECT score <= id FROM people", "SELECT (score <= CAST(id AS FLOAT)) AS ?column? FROM people"},
		{"SELECT -id, -score FROM people", "SELECT (- id) AS ?column?, (- score) AS ?column? FROM people"},

		// no FROM, literals
		{"SELECT 1 + 2", "SELECT (1 + 2) AS ?column?"},
		{"SELECT 1.0, 2.50", "SELECT 1.0 AS ?column?, 2.5 AS ?column?"},
		{"SELECT 'it''s'", "SELECT 'it''s' AS ?column?"},
		{"SELECT NULL", "SELECT NULL AS ?column?"},
		{"SELECT NULL = NULL, NULL + 1, NOT NULL, NULL IS NULL",
			"SELECT (NULL = NULL) AS ?column?, (NULL + 1) AS ?column?, (NOT NULL) AS ?column?, (NULL IS NULL) AS ?column?"},

		// comparisons and logic
		{"SELECT active = TRUE, name = 'x' FROM people", "SELECT (active = TRUE) AS ?column?, (name = 'x') AS ?column? FROM people"},
		{"SELECT id FROM people WHERE name IS NULL AND NOT active", "SELECT id AS id FROM people WHERE ((name IS NULL) AND (NOT active))"},
		{"SELECT id FROM people WHERE NULL", "SELECT id AS id FROM people WHERE NULL"},

		// ORDER BY: expressions, names of outputs, positions
		{"SELECT id AS k FROM people ORDER BY k DESC, 1, name LIMIT 5", "SELECT id AS k FROM people ORDER BY id DESC, id, name LIMIT 5"},
		{"SELECT id FROM people ORDER BY score + 1", "SELECT id AS id FROM people ORDER BY (score + 1.0)"},
		{"SELECT id AS name FROM people ORDER BY name", "SELECT id AS name FROM people ORDER BY id"},
		{"SELECT id, id FROM people ORDER BY id", "SELECT id AS id, id AS id FROM people ORDER BY id"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			assert.Equal(t, tc.want, bindOK(t, tc.src).String())
		})
	}
}

func TestExpressionTypes(t *testing.T) {
	tests := []struct {
		expr string
		want types.Type
	}{
		{"1", types.Int}, {"1.5", types.Float}, {"'a'", types.Text}, {"TRUE", types.Bool},
		{"NULL", types.Text}, // nothing says what it is, so it is TEXT, as in PostgreSQL
		{"1 + 1.5", types.Float}, {"1 / 2", types.Int}, {"7 % 3", types.Int},
		{"1 = 1", types.Bool}, {"1 < 1.5", types.Bool}, {"'a' < 'b'", types.Bool}, {"TRUE > FALSE", types.Bool},
		{"NULL + 1", types.Int}, {"NULL + 1.5", types.Float}, {"NULL = NULL", types.Bool},
		{"NULL AND TRUE", types.Bool}, {"NOT NULL", types.Bool}, {"-NULL", types.Int}, {"NULL IS NULL", types.Bool},
		{"1 IS NULL", types.Bool}, {"id", types.Int}, {"name", types.Text}, {"score", types.Float}, {"active", types.Bool},
		{"id * score", types.Float}, {"-score", types.Float},
	}
	for _, tc := range tests {
		t.Run(tc.expr, func(t *testing.T) {
			sel := bindOK(t, "SELECT "+tc.expr+" FROM people").(*Select)
			assert.Equal(t, tc.want, sel.Items[0].Expr.Type())
		})
	}
}

func TestBindSelectStructure(t *testing.T) {
	sel := bindOK(t, "SELECT name, id + 1 AS next FROM people WHERE id > 3 ORDER BY next DESC LIMIT 2").(*Select)
	assert.Equal(t, "people", sel.Table)
	require.Len(t, sel.Items, 2)
	assert.Equal(t, 1, sel.Items[0].Expr.(*ColRef).Pos, "name is the second column")
	assert.Equal(t, "next", sel.Items[1].Name)
	assert.Equal(t, types.Bool, sel.Where.Type())
	require.Len(t, sel.OrderBy, 1)
	assert.True(t, sel.OrderBy[0].Desc)
	assert.Equal(t, "(id + 1)", sel.OrderBy[0].Expr.String())
	require.NotNil(t, sel.Limit)
	assert.Equal(t, int64(2), *sel.Limit)

	bare := bindOK(t, "SELECT 1").(*Select)
	assert.Equal(t, "", bare.Table)
}

func TestBindInsert(t *testing.T) {
	tests := []struct{ src, want string }{
		{"INSERT INTO people VALUES (1, 'a', 2.5, TRUE)", "INSERT INTO people (id, name, score, active) VALUES (1, 'a', 2.5, TRUE)"},
		{"insert into PEOPLE (name, id) values ('a', 1)", "INSERT INTO people (name, id) VALUES ('a', 1)"},
		{"INSERT INTO people (id) VALUES (1), (2)", "INSERT INTO people (id) VALUES (1), (2)"},
		{"INSERT INTO people (id, score) VALUES (1, 2)", "INSERT INTO people (id, score) VALUES (1, 2.0)"},
		{"INSERT INTO people (id, name) VALUES (1, NULL)", "INSERT INTO people (id, name) VALUES (1, NULL)"},
		{"INSERT INTO people (id) VALUES (1 + 2 * 3)", "INSERT INTO people (id) VALUES ((1 + (2 * 3)))"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			assert.Equal(t, tc.want, bindOK(t, tc.src).String())
		})
	}

	ins := bindOK(t, "INSERT INTO people (name, id) VALUES ('a', 1)").(*Insert)
	assert.Equal(t, []int{1, 0}, ins.Columns)

	null := bindOK(t, "INSERT INTO people (id, name) VALUES (1, NULL)").(*Insert)
	assert.Equal(t, types.Text, null.Rows[0][1].Type(), "NULL takes the type of its column")
}

func TestBindUpdateAndDelete(t *testing.T) {
	tests := []struct{ src, want string }{
		{"UPDATE people SET score = score + 1, name = 'x' WHERE id = 3", "UPDATE people SET score = (score + 1.0), name = 'x' WHERE (id = 3)"},
		{"update PEOPLE set ID = 1", "UPDATE people SET id = 1"},
		{"UPDATE people SET score = 2", "UPDATE people SET score = 2.0"},
		{"UPDATE people SET name = NULL", "UPDATE people SET name = NULL"},
		{"DELETE FROM people", "DELETE FROM people"},
		{"delete from People where active", "DELETE FROM people WHERE active"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			assert.Equal(t, tc.want, bindOK(t, tc.src).String())
		})
	}
}

func TestBindDDL(t *testing.T) {
	ct := bindOK(t, "CREATE TABLE t (a INT PRIMARY KEY, b TEXT NOT NULL, c FLOAT)").(*CreateTable)
	assert.Equal(t, "t", ct.Name)
	assert.Equal(t, 0, ct.PrimaryKey)
	assert.Equal(t, []types.Column{
		{Name: "a", Type: types.Int, NotNull: true}, // PRIMARY KEY implies NOT NULL
		{Name: "b", Type: types.Text, NotNull: true},
		{Name: "c", Type: types.Float},
	}, ct.Columns)
	assert.Equal(t, "CREATE TABLE t (a INT NOT NULL PRIMARY KEY, b TEXT NOT NULL, c FLOAT)", ct.String())

	assert.Equal(t, -1, bindOK(t, "CREATE TABLE t (a INT)").(*CreateTable).PrimaryKey)
	assert.Equal(t, "DROP TABLE people", bindOK(t, "DROP TABLE PEOPLE").String(), "the catalog's spelling")
	assert.Equal(t, "CREATE UNIQUE INDEX ix ON people (id)", bindOK(t, "create unique index ix on PEOPLE (ID)").String())
	assert.Equal(t, "DROP INDEX people_id", bindOK(t, "DROP INDEX people_id").String())
}

func TestBindErrors(t *testing.T) {
	long := strings.Repeat("a", 65)
	tests := []struct {
		src       string
		line, col int
		want      string
	}{
		// SELECT
		{"SELECT x FROM people", 1, 8, `column "x" does not exist`},
		{"SELECT * ", 1, 8, "SELECT * needs a FROM clause"},
		{"SELECT id FROM nope", 1, 16, `table "nope" does not exist`},
		{"SELECT q.id FROM people", 1, 8, `missing FROM-clause entry for table "q"`},
		{"SELECT people.id FROM people p", 1, 8, `missing FROM-clause entry for table "people"`},
		{"SELECT x.* FROM people", 1, 8, `missing FROM-clause entry for table "x"`},
		{"SELECT name + 1 FROM people", 1, 13, "operator + is not defined for TEXT and INT"},
		{"SELECT 1 + 'a'", 1, 10, "operator + is not defined for INT and TEXT"},
		{"SELECT TRUE + 1", 1, 13, "operator + is not defined for BOOL and INT"},
		{"SELECT score % 2 FROM people", 1, 14, "operator % is not defined for FLOAT and INT"},
		{"SELECT -name FROM people", 1, 8, "operator - is not defined for TEXT"},
		{"SELECT NOT id FROM people", 1, 12, "argument of NOT must be BOOL, found INT"},
		{"SELECT id AND active FROM people", 1, 8, "argument of AND must be BOOL, found INT"},
		{"SELECT name < 1 FROM people", 1, 13, "cannot compare TEXT with INT"},
		{"SELECT id FROM people WHERE id", 1, 29, "WHERE must be a BOOL expression, found INT"},
		{"SELECT id FROM people WHERE name", 1, 29, "WHERE must be a BOOL expression, found TEXT"},
		{"SELECT count(*) FROM people", 1, 8, "functions are not supported yet"},
		{"SELECT * FROM people JOIN orders ON TRUE", 1, 22, "JOIN is not supported yet"},
		{"SELECT id FROM people GROUP BY id", 1, 32, "GROUP BY is not supported yet"},
		{"SELECT id FROM people ORDER BY 9", 1, 32, "ORDER BY position 9 is not in the select list"},
		{"SELECT id FROM people ORDER BY 0", 1, 32, "ORDER BY position 0 is not in the select list"},
		{"SELECT 1 FROM people ORDER BY nope", 1, 31, `column "nope" does not exist`},
		{"SELECT id AS a, name AS a FROM people ORDER BY a", 1, 48, `ORDER BY "a" is ambiguous`},
		{"SELECT id\nFROM people\nWHERE nope = 1", 3, 7, `column "nope" does not exist`},
		{"SELECT x", 1, 8, "the query has no FROM clause"},

		// INSERT
		{"INSERT INTO nope VALUES (1)", 1, 13, `table "nope" does not exist`},
		{"INSERT INTO people (nope) VALUES (1)", 1, 21, `column "nope" does not exist in table "people"`},
		{"INSERT INTO people (id, ID) VALUES (1, 2)", 1, 25, `column "ID" is listed twice`},
		{"INSERT INTO people (id, name) VALUES (1)", 1, 38, "the row has 1 values but 2 columns were listed"},
		{"INSERT INTO people VALUES (1)", 1, 27, "the row has 1 values but the table has 4 columns"},
		{"INSERT INTO orders (total) VALUES (1.5)", 1, 13, `column "order_id" is NOT NULL and needs a value`},
		{"INSERT INTO people (id) VALUES ('x')", 1, 33, `cannot store TEXT in INT column "id"`},
		{"INSERT INTO people (id) VALUES (1.5)", 1, 33, `cannot store FLOAT in INT column "id"`},
		{"INSERT INTO people (id) VALUES (NULL)", 1, 33, `column "id" cannot be NULL`},
		{"INSERT INTO people (id) VALUES (id)", 1, 33, "VALUES cannot refer to columns"},
		{"INSERT INTO people (id) VALUES (count(*))", 1, 33, "functions are not supported yet"},
		{"INSERT INTO people (id, active) VALUES (1, 1)", 1, 44, `cannot store INT in BOOL column "active"`},

		// UPDATE and DELETE
		{"UPDATE nope SET a = 1", 1, 8, `table "nope" does not exist`},
		{"UPDATE people SET nope = 1", 1, 19, `column "nope" does not exist in table "people"`},
		{"UPDATE people SET id = 1, ID = 2", 1, 27, `column "ID" is assigned twice`},
		{"UPDATE people SET id = 1.5", 1, 24, `cannot store FLOAT in INT column "id"`},
		{"UPDATE people SET id = NULL", 1, 24, `column "id" cannot be NULL`},
		{"UPDATE people SET name = 1", 1, 26, `cannot store INT in TEXT column "name"`},
		{"UPDATE people SET score = 1 WHERE name", 1, 35, "WHERE must be a BOOL expression, found TEXT"},
		{"DELETE FROM nope", 1, 13, `table "nope" does not exist`},
		{"DELETE FROM people WHERE name", 1, 26, "WHERE must be a BOOL expression, found TEXT"},

		// DDL
		{"CREATE TABLE people (a INT)", 1, 14, `table "people" already exists`},
		{"CREATE TABLE t (a INT, A TEXT)", 1, 24, `column "A" is defined twice`},
		{"CREATE TABLE t (a INT PRIMARY KEY, b INT PRIMARY KEY)", 1, 36, "a table can have only one PRIMARY KEY"},
		{"CREATE TABLE t (a TEXT PRIMARY KEY)", 1, 17, `PRIMARY KEY must be an INT column, "a" is TEXT`},
		{"CREATE TABLE " + long + " (a INT)", 1, 14, "is longer than 64 characters"},
		{"DROP TABLE nope", 1, 12, `table "nope" does not exist`},
		{"CREATE UNIQUE INDEX ix ON nope (a)", 1, 27, `table "nope" does not exist`},
		{"CREATE UNIQUE INDEX ix ON people (nope)", 1, 35, `column "nope" does not exist in table "people"`},
		{"CREATE UNIQUE INDEX ix ON people (name)", 1, 35, `only INT columns can be indexed, "name" is TEXT`},
		{"CREATE UNIQUE INDEX people_id ON people (id)", 1, 21, `index "people_id" already exists`},
		{"DROP INDEX nope", 1, 12, `index "nope" does not exist`},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			_, err := bindSQL(t, tc.src)
			var be *BindError
			require.True(t, errors.As(err, &be), "got %v", err)
			assert.Equal(t, tc.line, be.Pos.Line, "line")
			assert.Equal(t, tc.col, be.Pos.Col, "column")
			assert.Contains(t, be.Msg, tc.want)
		})
	}
}

func TestLongStringLiteralIsRefused(t *testing.T) {
	_, err := bindSQL(t, "SELECT '"+strings.Repeat("a", types.MaxTextLen+1)+"'")
	assert.ErrorContains(t, err, "the string is longer than")
}
