package sql

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func parseOK(tb testing.TB, src string) Expr {
	tb.Helper()
	e, err := ParseExpr(src)
	require.NoError(tb, err, "source %q", src)
	return e
}

// The expected text is the fully parenthesized form, which shows the grouping.
func TestParseExpr(t *testing.T) {
	tests := []struct{ src, want string }{
		// precedence and grouping of arithmetic
		{"1 + 2 * 3", "(1 + (2 * 3))"},
		{"1 * 2 + 3", "((1 * 2) + 3)"},
		{"1 - 2 - 3", "((1 - 2) - 3)"},
		{"8 / 4 / 2", "((8 / 4) / 2)"},
		{"1 + 2 % 3", "(1 + (2 % 3))"},
		{"(1 + 2) * 3", "((1 + 2) * 3)"},
		{"((a))", "a"},
		{"-a * b", "((- a) * b)"},
		{"- - a", "(- (- a))"},
		{"a + -b", "(a + (- b))"},

		// a minus in front of a number belongs to the number
		{"-5", "-5"},
		{"- 5", "-5"},
		{"-1.5", "-1.5"},
		{"-5 * 3", "(-5 * 3)"},
		{"a - -5", "(a - -5)"},
		{"a -5", "(a - 5)"},
		{"-(5)", "(- (5))"},
		{"- -5", "(- (-5))"},
		{"-9223372036854775808", "-9223372036854775808"},
		{"9223372036854775807", "9223372036854775807"},

		// comparisons
		{"a = 1", "(a = 1)"},
		{"a != 1", "(a <> 1)"},
		{"a <> 1", "(a <> 1)"},
		{"a <= b + 1", "(a <= (b + 1))"},
		{"a + 1 > b * 2", "((a + 1) > (b * 2))"},

		// logic
		{"a OR b AND c", "(a OR (b AND c))"},
		{"a AND b OR c", "((a AND b) OR c)"},
		{"a and B Or NOT c", "((a AND B) OR (NOT c))"},
		{"NOT a AND b", "((NOT a) AND b)"},
		{"NOT a = b", "(NOT (a = b))"},
		{"NOT NOT a", "(NOT (NOT a))"},
		{"a AND NOT b", "(a AND (NOT b))"},
		{"a = 1 AND b < 2 OR c", "(((a = 1) AND (b < 2)) OR c)"},

		// IS NULL sits between NOT and the comparisons
		{"a IS NULL", "(a IS NULL)"},
		{"a IS NOT NULL", "(a IS NOT NULL)"},
		{"a = b IS NULL", "((a = b) IS NULL)"},
		{"a IS NULL AND b", "((a IS NULL) AND b)"},
		{"a + 1 IS NOT NULL", "((a + 1) IS NOT NULL)"},
		{"NOT a IS NULL", "(NOT (a IS NULL))"},
		{"a IS NULL IS NULL", "((a IS NULL) IS NULL)"},

		// literals
		{"42", "42"},
		{"1.5", "1.5"},
		{"1e3", "1000.0"},
		{"0.1", "0.1"},
		{"1e21", "1e+21"},
		{"'it''s'", "'it''s'"},
		{"''", "''"},
		{"TRUE", "TRUE"},
		{"false", "FALSE"},
		{"null", "NULL"},

		// names and calls
		{"a", "a"},
		{"t.col", "t.col"},
		{"Users.Name", "Users.Name"},
		{"count(*)", "count(*)"},
		{"f()", "f()"},
		{"sum(a + 1)", "sum((a + 1))"},
		{"max(a, b)", "max(a, b)"},
		{"f(g(1))", "f(g(1))"},

		// space and comments
		{"1 -- a comment\n + 2", "(1 + 2)"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			assert.Equal(t, tc.want, parseOK(t, tc.src).String())
		})
	}
}

func TestLiteralValues(t *testing.T) {
	assert.Equal(t, int64(-9223372036854775808), parseOK(t, "-9223372036854775808").(*IntLiteral).Value)
	assert.Equal(t, int64(7), parseOK(t, "007").(*IntLiteral).Value)
	assert.Equal(t, 1500.0, parseOK(t, "1.5e3").(*FloatLiteral).Value)
	assert.Equal(t, "it's", parseOK(t, "'it''s'").(*StringLiteral).Value)
	assert.True(t, parseOK(t, "true").(*BoolLiteral).Value)

	call := parseOK(t, "COUNT(*)").(*FuncCall)
	assert.Equal(t, "COUNT", call.Name)
	assert.True(t, call.Star)
	ref := parseOK(t, "t.c").(*ColumnRef)
	assert.Equal(t, "t", ref.Table)
	assert.Equal(t, "c", ref.Name)
}

func TestNodePositions(t *testing.T) {
	e := parseOK(t, "(a + b) * c").(*BinaryExpr)
	assert.Equal(t, 8, e.Start().Offset, "an operator points at itself")
	assert.Equal(t, 3, e.L.Start().Offset, "the + inside the parentheses")
	assert.Equal(t, 10, e.R.Start().Offset)

	assert.Equal(t, 0, parseOK(t, "-x").Start().Offset)
	assert.Equal(t, 0, parseOK(t, "-5").Start().Offset, "a negative literal starts at its sign")
	assert.Equal(t, 2, parseOK(t, "a IS NULL").Start().Offset, "IS NULL points at IS")
	assert.Equal(t, 0, parseOK(t, "f(1)").Start().Offset)
	assert.Equal(t, 0, parseOK(t, "t.c").Start().Offset)

	multi := parseOK(t, "x +\n  y").(*BinaryExpr)
	assert.Equal(t, Pos{Offset: 2, Line: 1, Col: 3}, multi.Start())
	assert.Equal(t, Pos{Offset: 6, Line: 2, Col: 3}, multi.R.Start())
}

func TestParseExprErrors(t *testing.T) {
	tests := []struct {
		src       string
		line, col int
		want      string
	}{
		{"", 1, 1, "expected an expression, found end of input"},
		{"1 +", 1, 4, "expected an expression, found end of input"},
		{"NOT", 1, 4, "expected an expression, found end of input"},
		{"1 2", 1, 3, "unexpected number 2 after the expression"},
		{"a IS NULL NULL", 1, 11, "unexpected NULL after the expression"},
		{"(1 + 2", 1, 7, `expected ")", found end of input`},
		{")", 1, 1, `expected an expression, found ")"`},
		{"a = = b", 1, 5, `expected an expression, found "="`},
		{"a +* b", 1, 4, `expected an expression, found "*"`},
		{"+5", 1, 1, `expected an expression, found "+"`},
		{"SELECT", 1, 1, "expected an expression, found SELECT"},
		{"1 +\n  )", 2, 3, `expected an expression, found ")"`},
		{"a < b < c", 1, 7, "cannot be chained"},
		{"a = b = c", 1, 7, "cannot be chained"},
		{"a = b <> c", 1, 7, "cannot be chained"},
		{"a IS 5", 1, 6, "expected NULL or NOT NULL after IS"},
		{"a IS NOT 5", 1, 10, "expected NULL after IS NOT"},
		{"t.", 1, 3, `expected a column name after "."`},
		{"t.5", 1, 3, `expected a column name after "."`},
		{"f(1,", 1, 5, "expected an expression, found end of input"},
		{"f(1 2)", 1, 5, `expected ")", found number 2`},
		{"f(*, 1)", 1, 4, `expected ")", found ","`},
		{"9223372036854775808", 1, 1, "out of range"},
		{"-9223372036854775809", 1, 2, "out of range"},
		{"1e999", 1, 1, "out of range"},
		{"a @", 1, 3, "unexpected character '@'"}, // a lexer error comes through unchanged
		{"'abc", 1, 1, "unterminated string"},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			_, err := ParseExpr(tc.src)
			var se *SyntaxError
			require.True(t, errors.As(err, &se), "got %v", err)
			assert.Equal(t, tc.line, se.Pos.Line)
			assert.Equal(t, tc.col, se.Pos.Col)
			assert.Contains(t, se.Msg, tc.want)
		})
	}
}

func TestNestingIsLimited(t *testing.T) {
	tests := []struct {
		name         string
		build        func(n int) string
		okAt, failAt int
	}{
		{"parentheses", func(n int) string { return strings.Repeat("(", n) + "1" + strings.Repeat(")", n) }, 100, 300},
		{"NOT", func(n int) string { return strings.Repeat("NOT ", n) + "a" }, 100, 300},
		{"unary minus", func(n int) string { return strings.Repeat("- ", n) + "a" }, 100, 300},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseExpr(tc.build(tc.okAt))
			assert.NoError(t, err)
			_, err = ParseExpr(tc.build(tc.failAt))
			assert.ErrorContains(t, err, "nested too deeply")
		})
	}
}

// A long chain is read with a loop, so its length does not use the stack.
func TestLongChainsAreFine(t *testing.T) {
	e, err := ParseExpr(strings.Repeat("1+", 100000) + "1")
	require.NoError(t, err)
	assert.IsType(t, &BinaryExpr{}, e)

	e, err = ParseExpr(strings.Repeat("a AND ", 100000) + "a")
	require.NoError(t, err)
	assert.IsType(t, &BinaryExpr{}, e)
}

func maxParenDepth(s string) int {
	depth, max := 0, 0
	for _, c := range s {
		switch c {
		case '(':
			depth++
			if depth > max {
				max = depth
			}
		case ')':
			depth--
		}
	}
	return max
}

// The properties: ParseExpr never panics, its errors are syntax errors, and the
// printed form of what it parsed parses again to the same printed form. Run it
// for real with:
//
//	go test -fuzz=FuzzParseExpr -fuzztime=20s -fuzzminimizetime=0 ./internal/sql/
func FuzzParseExpr(f *testing.F) {
	for _, s := range []string{
		"1 + 2 * 3", "a AND NOT b OR c IS NULL", "-9223372036854775808", "f(*, 1)", "t.c <= -1.5e3",
		"'it''s'", "((a))", "NOT NOT a", "a < b < c", "-(5)", "- -5", "a - -5", "count(*) + 1",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		e, err := ParseExpr(src)
		if err != nil {
			var se *SyntaxError
			require.ErrorAs(t, err, &se)
			return
		}
		printed := e.String()
		if maxParenDepth(printed) > 80 {
			return // the printed form of a long chain is nested more deeply than the limit allows
		}
		again, err := ParseExpr(printed)
		require.NoError(t, err, "the printed form %q must parse again", printed)
		require.Equal(t, printed, again.String())
	})
}
