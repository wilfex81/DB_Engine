package query

import (
	"errors"
	"math"
	"math/big"
	"math/rand"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wilfex81/DB_Engine/internal/sql"
	"github.com/wilfex81/DB_Engine/internal/types"
)

func iv(n int64) types.Value      { return types.NewInt(n) }
func fv(f float64) types.Value    { return types.NewFloat(f) }
func sv(s string) types.Value     { return types.NewText(s) }
func bv(b bool) types.Value       { return types.NewBool(b) }
func nv(t types.Type) types.Value { return types.Null(t) }

// evalConst runs "SELECT expr" with no table: parse, bind, evaluate.
func evalConst(tb testing.TB, expr string) (types.Value, error) {
	tb.Helper()
	s, err := sql.ParseStatement("SELECT " + expr)
	require.NoError(tb, err, "parsing %q", expr)
	b, err := Bind(testCatalog(tb), s)
	require.NoError(tb, err, "binding %q", expr)
	return Eval(b.(*Select).Items[0].Expr, nil)
}

func TestEvalValues(t *testing.T) {
	tests := []struct {
		expr string
		want types.Value
	}{
		// INT arithmetic, with division truncating toward zero
		{"1 + 2", iv(3)}, {"7 - 10", iv(-3)}, {"6 * 7", iv(42)},
		{"7 / 2", iv(3)}, {"-7 / 2", iv(-3)}, {"7 / -2", iv(-3)}, {"-7 / -2", iv(3)},
		{"7 % 3", iv(1)}, {"-7 % 3", iv(-1)}, {"7 % -3", iv(1)},
		{"2 + 3 * 4", iv(14)}, {"(2 + 3) * 4", iv(20)}, {"- (2 + 3)", iv(-5)}, {"2 * 3 + 4", iv(10)},
		{"9223372036854775807 + 0", iv(math.MaxInt64)},
		{"-9223372036854775808 + 9223372036854775807", iv(-1)},
		{"-9223372036854775808 % -1", iv(0)},
		{"9223372036854775807 * -1", iv(-math.MaxInt64)},
		{"-9223372036854775808 * 1", iv(math.MinInt64)},
		{"0 * -9223372036854775808", iv(0)},
		{"3037000499 * 3037000499", iv(9223372030926249001)},

		// FLOAT arithmetic, and INT mixed with FLOAT
		{"1.5 + 2.5", fv(4)}, {"1 + 0.5", fv(1.5)}, {"7 / 2.0", fv(3.5)}, {"2.5 * 2", fv(5)},
		{"1.0 / 3", fv(1.0 / threeF)}, {"-1.5", fv(-1.5)}, {"- (1.5)", fv(-1.5)}, {"1.5 - 2", fv(-0.5)},

		// comparisons
		{"1 < 2", bv(true)}, {"2 < 1", bv(false)}, {"1 <= 1", bv(true)}, {"1 >= 2", bv(false)},
		{"1 = 1.0", bv(true)}, {"1 <> 1.0", bv(false)}, {"2 > 1.5", bv(true)},
		{"0.1 + 0.2 = 0.3", bv(false)}, // floats are not exact
		{"'a' < 'b'", bv(true)}, {"'abc' < 'abd'", bv(true)}, {"'' < 'a'", bv(true)}, {"'a' = 'a'", bv(true)},
		{"'B' < 'a'", bv(true)}, // bytes, not dictionary order
		{"'é' > 'z'", bv(true)}, // UTF-8 bytes
		{"TRUE > FALSE", bv(true)}, {"FALSE < TRUE", bv(true)}, {"TRUE = TRUE", bv(true)}, {"TRUE <> FALSE", bv(true)},

		// NULL in arithmetic and comparisons
		{"NULL + 1", nv(types.Int)}, {"1 + NULL", nv(types.Int)}, {"NULL * 2.5", nv(types.Float)},
		{"NULL / 0", nv(types.Int)}, {"1 / NULL", nv(types.Int)}, {"-NULL", nv(types.Int)},
		{"NULL = NULL", nv(types.Bool)}, {"NULL <> NULL", nv(types.Bool)}, {"1 = NULL", nv(types.Bool)},
		{"NULL < 1", nv(types.Bool)}, {"'a' = NULL", nv(types.Bool)},

		// IS NULL
		{"NULL IS NULL", bv(true)}, {"1 IS NULL", bv(false)}, {"NULL IS NOT NULL", bv(false)}, {"1 IS NOT NULL", bv(true)},
		{"(1 = NULL) IS NULL", bv(true)}, {"(NULL + 1) IS NULL", bv(true)},

		// NOT
		{"NOT TRUE", bv(false)}, {"NOT FALSE", bv(true)}, {"NOT NULL", nv(types.Bool)},

		// short circuit: the right side is not evaluated when the left decides
		{"FALSE AND 1 / 0 = 1", bv(false)},
		{"TRUE OR 1 / 0 = 1", bv(true)},
	}
	for _, tc := range tests {
		t.Run(tc.expr, func(t *testing.T) {
			got, err := evalConst(t, tc.expr)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// threeF keeps 1.0 / 3 from being folded at compile time.
var threeF = 3.0

// The truth tables of three valued logic, written out by hand.
func TestThreeValuedLogic(t *testing.T) {
	T, F, N := bv(true), bv(false), nv(types.Bool)
	lit := map[string]string{"T": "TRUE", "F": "FALSE", "N": "NULL"}
	val := map[string]types.Value{"T": T, "F": F, "N": N}

	and := map[string]string{
		"TT": "T", "TF": "F", "TN": "N",
		"FT": "F", "FF": "F", "FN": "F",
		"NT": "N", "NF": "F", "NN": "N",
	}
	or := map[string]string{
		"TT": "T", "TF": "T", "TN": "T",
		"FT": "T", "FF": "F", "FN": "N",
		"NT": "T", "NF": "N", "NN": "N",
	}
	for pair, want := range and {
		got, err := evalConst(t, lit[pair[:1]]+" AND "+lit[pair[1:]])
		require.NoError(t, err)
		assert.Equal(t, val[want], got, "%s AND %s", pair[:1], pair[1:])
	}
	for pair, want := range or {
		got, err := evalConst(t, lit[pair[:1]]+" OR "+lit[pair[1:]])
		require.NoError(t, err)
		assert.Equal(t, val[want], got, "%s OR %s", pair[:1], pair[1:])
	}
}

func TestEvalErrors(t *testing.T) {
	tests := []struct {
		expr string
		want string
	}{
		{"1 / 0", "division by zero"},
		{"1 % 0", "division by zero"},
		{"1.0 / 0.0", "division by zero"},
		{"1 / 0.0", "division by zero"},
		{"1.5 / 0", "division by zero"},
		{"9223372036854775807 + 1", "integer overflow"},
		{"-9223372036854775808 - 1", "integer overflow"},
		{"9223372036854775807 - -9223372036854775808", "integer overflow"},
		{"9223372036854775807 * 2", "integer overflow"},
		{"3037000500 * 3037000500", "integer overflow"},
		{"-3037000500 * 3037000500", "integer overflow"},
		{"-9223372036854775808 * -1", "integer overflow"},
		{"-1 * -9223372036854775808", "integer overflow"},
		{"-9223372036854775808 / -1", "integer overflow"},
		{"-(-9223372036854775808)", "integer overflow"},
		{"1e308 * 10", "FLOAT overflow"},
		{"1e308 + 1e308", "FLOAT overflow"},
		{"-1e308 - 1e308", "FLOAT overflow"},

		// the right side of AND and OR is evaluated when the left does not decide
		{"TRUE AND 1 / 0 = 1", "division by zero"},
		{"NULL AND 1 / 0 = 1", "division by zero"},
		{"FALSE OR 1 / 0 = 1", "division by zero"},
		{"NULL OR 1 / 0 = 1", "division by zero"},

		// arithmetic looks at both sides before it looks at NULL
		{"NULL + 1 / 0", "division by zero"},
	}
	for _, tc := range tests {
		t.Run(tc.expr, func(t *testing.T) {
			_, err := evalConst(t, tc.expr)
			var re *RuntimeError
			require.True(t, errors.As(err, &re), "got %v", err)
			assert.Contains(t, re.Msg, tc.want)
		})
	}
}

func TestRuntimeErrorPositions(t *testing.T) {
	tests := []struct {
		src       string
		line, col int
	}{
		{"SELECT 1 / 0", 1, 10},
		{"SELECT 9223372036854775807 + 1", 1, 28},
		{"SELECT -(-9223372036854775808)", 1, 8},
		{"SELECT 1 +\n  1 / 0", 2, 5},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			s, err := sql.ParseStatement(tc.src)
			require.NoError(t, err)
			b, err := Bind(testCatalog(t), s)
			require.NoError(t, err)
			_, err = Eval(b.(*Select).Items[0].Expr, nil)
			var re *RuntimeError
			require.True(t, errors.As(err, &re), "got %v", err)
			assert.Equal(t, tc.line, re.Pos.Line)
			assert.Equal(t, tc.col, re.Pos.Col)
		})
	}
}

// Columns: a table with nullable columns, three rows, and expressions that
// are evaluated over each row.
func TestEvalOverRows(t *testing.T) {
	cat := fakeCatalog{tables: map[string]TableInfo{"nums": {Name: "nums", Schema: schemaOf(t,
		types.Column{Name: "a", Type: types.Int},
		types.Column{Name: "b", Type: types.Int},
		types.Column{Name: "f", Type: types.Float},
		types.Column{Name: "s", Type: types.Text},
		types.Column{Name: "ok", Type: types.Bool},
	)}}}
	rows := [][]types.Value{
		{iv(1), iv(2), fv(0.5), sv("x"), bv(true)},
		{nv(types.Int), iv(2), nv(types.Float), nv(types.Text), nv(types.Bool)},
		{iv(math.MaxInt64), iv(1), fv(1.0), sv(""), bv(false)},
	}

	type result struct {
		v   types.Value
		err string
	}
	tests := []struct {
		expr string
		want [3]result
	}{
		{"a + b", [3]result{{v: iv(3)}, {v: nv(types.Int)}, {err: "integer overflow"}}},
		{"a + f", [3]result{{v: fv(1.5)}, {v: nv(types.Float)}, {v: fv(float64(math.MaxInt64) + 1.0)}}},
		{"b / a", [3]result{{v: iv(2)}, {v: nv(types.Int)}, {v: iv(0)}}},
		{"-a", [3]result{{v: iv(-1)}, {v: nv(types.Int)}, {v: iv(-math.MaxInt64)}}},
		{"f * 2", [3]result{{v: fv(1)}, {v: nv(types.Float)}, {v: fv(2)}}},
		{"s = 'x'", [3]result{{v: bv(true)}, {v: nv(types.Bool)}, {v: bv(false)}}},
		{"a IS NULL", [3]result{{v: bv(false)}, {v: bv(true)}, {v: bv(false)}}},
		{"NOT ok", [3]result{{v: bv(false)}, {v: nv(types.Bool)}, {v: bv(true)}}},
		{"ok AND a > 0", [3]result{{v: bv(true)}, {v: nv(types.Bool)}, {v: bv(false)}}},
		{"ok OR a IS NULL", [3]result{{v: bv(true)}, {v: bv(true)}, {v: bv(false)}}},
	}
	for _, tc := range tests {
		t.Run(tc.expr, func(t *testing.T) {
			s, err := sql.ParseStatement("SELECT " + tc.expr + " FROM nums")
			require.NoError(t, err)
			b, err := Bind(cat, s)
			require.NoError(t, err)
			e := b.(*Select).Items[0].Expr

			for i, row := range rows {
				got, err := Eval(e, row)
				if want := tc.want[i]; want.err != "" {
					var re *RuntimeError
					require.True(t, errors.As(err, &re), "row %d: got %v", i, err)
					assert.Contains(t, re.Msg, want.err, "row %d", i)
				} else {
					require.NoError(t, err, "row %d", i)
					assert.Equal(t, want.v, got, "row %d", i)
				}
			}
		})
	}

	// WHERE: only TRUE keeps a row. FALSE and NULL both drop it.
	s, err := sql.ParseStatement("SELECT a FROM nums WHERE a > 0")
	require.NoError(t, err)
	b, err := Bind(cat, s)
	require.NoError(t, err)
	where := b.(*Select).Where
	var kept []bool
	for _, row := range rows {
		ok, err := Matches(where, row)
		require.NoError(t, err)
		kept = append(kept, ok)
	}
	assert.Equal(t, []bool{true, false, true}, kept)

	ok, err := Matches(nil, nil)
	require.NoError(t, err)
	assert.True(t, ok, "no WHERE keeps every row")
}

// A tiny executor for a SELECT with no FROM, to see the whole front end work
// together: text in, values out.
func TestSelectWithoutFromEndToEnd(t *testing.T) {
	run := func(src string) [][]types.Value {
		s, err := sql.ParseStatement(src)
		require.NoError(t, err, src)
		b, err := Bind(testCatalog(t), s)
		require.NoError(t, err, src)
		sel := b.(*Select)
		ok, err := Matches(sel.Where, nil)
		require.NoError(t, err, src)
		if !ok {
			return nil
		}
		row := make([]types.Value, len(sel.Items))
		for i, it := range sel.Items {
			row[i], err = Eval(it.Expr, nil)
			require.NoError(t, err, src)
		}
		return [][]types.Value{row}
	}

	assert.Equal(t, [][]types.Value{{iv(3)}}, run("SELECT 1 + 2"))
	assert.Equal(t, [][]types.Value{{iv(14), iv(2), iv(2), fv(2.5)}}, run("SELECT 2 * (3 + 4), 10 / 4, 10 % 4, 10 / 4.0"))
	assert.Equal(t, [][]types.Value{{bv(true), bv(true), bv(true), nv(types.Bool)}},
		run("SELECT 'a' = 'a', 1 < 2 AND NOT FALSE, NULL IS NULL, 1 = NULL"))
	assert.Equal(t, [][]types.Value{{iv(1)}}, run("SELECT 1 WHERE 1 = 1"))
	assert.Nil(t, run("SELECT 1 WHERE 1 = 2"))
	assert.Nil(t, run("SELECT 1 WHERE NULL"))
	assert.Nil(t, run("SELECT 1 WHERE 1 = NULL"))
}

// ---- integer arithmetic against exact big integers

type intNode struct {
	op   string // "+", "-", "*", "/", "%", "neg", or "" for a literal
	l, r *intNode
	lit  int64
}

var intLits = []int64{0, 1, -1, 2, 3, -7, 100, math.MaxInt64, math.MinInt64, 3037000499, -3037000500}

func genInt(rng *rand.Rand, depth int) *intNode {
	if depth == 0 || rng.Intn(4) == 0 {
		return &intNode{lit: intLits[rng.Intn(len(intLits))]}
	}
	if rng.Intn(6) == 0 {
		return &intNode{op: "neg", l: genInt(rng, depth-1)}
	}
	ops := []string{"+", "-", "*", "/", "%"}
	return &intNode{op: ops[rng.Intn(len(ops))], l: genInt(rng, depth-1), r: genInt(rng, depth-1)}
}

func (n *intNode) sql() string {
	switch n.op {
	case "":
		return strconv.FormatInt(n.lit, 10)
	case "neg":
		return "-(" + n.l.sql() + ")"
	}
	return "(" + n.l.sql() + " " + n.op + " " + n.r.sql() + ")"
}

// ref computes the exact value. ok is false if anything on the way divides by
// zero or leaves the range of an int64, which is where Eval must fail.
func (n *intNode) ref() (*big.Int, bool) {
	if n.op == "" {
		return big.NewInt(n.lit), true
	}
	x, ok := n.l.ref()
	if !ok {
		return nil, false
	}
	if n.op == "neg" {
		z := new(big.Int).Neg(x)
		return z, z.IsInt64()
	}
	y, ok := n.r.ref()
	if !ok {
		return nil, false
	}
	z := new(big.Int)
	switch n.op {
	case "+":
		z.Add(x, y)
	case "-":
		z.Sub(x, y)
	case "*":
		z.Mul(x, y)
	case "/", "%":
		if y.Sign() == 0 {
			return nil, false
		}
		if n.op == "/" {
			z.Quo(x, y) // truncated, like Go and PostgreSQL
		} else {
			z.Rem(x, y) // the sign follows the left side
		}
	}
	return z, z.IsInt64()
}

func TestIntegerArithmeticAgainstBigInts(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 4000; i++ {
		tree := genInt(rng, 4)
		src := tree.sql()
		want, ok := tree.ref()
		got, err := evalConst(t, src)
		if !ok {
			var re *RuntimeError
			require.True(t, errors.As(err, &re), "%s: expected an error, got %v and %v", src, got, err)
			require.True(t, strings.Contains(re.Msg, "overflow") || strings.Contains(re.Msg, "division by zero"), "%s: %s", src, re.Msg)
			continue
		}
		require.NoError(t, err, src)
		require.Equal(t, iv(want.Int64()), got, src)
	}
}
