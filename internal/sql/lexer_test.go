package sql

import (
	"errors"
	"math/rand"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type tk struct {
	Kind Kind
	Text string
}

// lexAll lexes src, and returns its tokens without the final EOF
func lexAll(tb testing.TB, src string) []tk {
	tb.Helper()
	toks, err := Lex(src)
	require.NoError(tb, err, "source %q", src)
	require.NotEmpty(tb, toks)
	require.Equal(tb, EOF, toks[len(toks)-1].Kind)
	out := make([]tk, 0, len(toks)-1)
	for _, t := range toks[:len(toks)-1] {
		out = append(out, tk{t.Kind, t.Text})
	}
	return out
}

func TextLexTokens(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []tk
	}{
		{"empty", "", []tk{}},
		{"only space and comments", " \t\r\n-- nothing\n  ", []tk{}},
		{"a statement", "SELECT * FROM t;",
			[]tk{{KwSelect, "SELECT"}, {Star, "*"}, {KwFrom, "FROM"}, {Ident, "t"}, {Semicolon, ";"}}},
		{"keywords ignore case", "select Select SELECT",
			[]tk{{KwSelect, "select"}, {KwSelect, "Select"}, {KwSelect, "SELECT"}}},
		{"names", "a _b c1 Users2",
			[]tk{{Ident, "a"}, {Ident, "_b"}, {Ident, "c1"}, {Ident, "Users2"}}},
		{"words that only start like a keyword", "selected from_x order1",
			[]tk{{Ident, "selected"}, {Ident, "from_x"}, {Ident, "order1"}}},
		{"type names are plain names", "INT text Bool float",
			[]tk{{Ident, "INT"}, {Ident, "text"}, {Ident, "Bool"}, {Ident, "float"}}},
		{"integers", "0 42 007", []tk{{IntLit, "0"}, {IntLit, "42"}, {IntLit, "007"}}},
		{"floats", "1.5 0.25 1e5 1E-3 2.5e+10",
			[]tk{{FloatLit, "1.5"}, {FloatLit, "0.25"}, {FloatLit, "1e5"}, {FloatLit, "1E-3"}, {FloatLit, "2.5e+10"}}},
		{"the smallest int64 stays as digits", "9223372036854775808", []tk{{IntLit, "9223372036854775808"}}},
		{"a dot that is not a fraction", "1.x 1. t.col",
			[]tk{{IntLit, "1"}, {Dot, "."}, {Ident, "x"}, {IntLit, "1"}, {Dot, "."}, {Ident, "t"}, {Dot, "."}, {Ident, "col"}}},
		{"operators", "= <> != < <= > >= + - * / %",
			[]tk{{Eq, "="}, {NotEq, "<>"}, {NotEq, "!="}, {Lt, "<"}, {LtEq, "<="}, {Gt, ">"}, {GtEq, ">="},
				{Plus, "+"}, {Minus, "-"}, {Star, "*"}, {Slash, "/"}, {Percent, "%"}}},
		{"operators without spaces", "a<=b<>c>=d",
			[]tk{{Ident, "a"}, {LtEq, "<="}, {Ident, "b"}, {NotEq, "<>"}, {Ident, "c"}, {GtEq, ">="}, {Ident, "d"}}},
		{"minus is its own token", "-5 1-2",
			[]tk{{Minus, "-"}, {IntLit, "5"}, {IntLit, "1"}, {Minus, "-"}, {IntLit, "2"}}},
		{"a double minus starts a comment", "1--2\n3", []tk{{IntLit, "1"}, {IntLit, "3"}}},
		{"punctuation", "( , ) ;", []tk{{LParen, "("}, {Comma, ","}, {RParen, ")"}, {Semicolon, ";"}}},
		{"strings", "'abc' '' 'it''s' 'a''''b'",
			[]tk{{StringLit, "abc"}, {StringLit, ""}, {StringLit, "it's"}, {StringLit, "a''b"}}},
		{"a string with a line feed", "'a\nb'", []tk{{StringLit, "a\nb"}}},
		{"a string with unicode", "'Nairobi 你好'", []tk{{StringLit, "Nairobi 你好"}}},
		{"a string that looks like other things", "'-- not a comment; SELECT'",
			[]tk{{StringLit, "-- not a comment; SELECT"}}},
		{"a comment before", "-- hi\nSELECT", []tk{{KwSelect, "SELECT"}}},
		{"a comment after", "SELECT -- trailing", []tk{{KwSelect, "SELECT"}}},
		{"a comment with no line end", "--", []tk{}},
		{"a comment glued to a name", "a--b", []tk{{Ident, "a"}}},
		{"windows line ends", "a\r\nb", []tk{{Ident, "a"}, {Ident, "b"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, lexAll(t, tc.src))
		})
	}
}

func TestLexPositions(t *testing.T) {
	// line 2 has a string with 2 and 3 byte characters, so bytes and columns differ
	src := "SELECT a,\n  'é你' , b -- c\n,z"
	toks, err := Lex(src)
	require.NoError(t, err)

	want := []struct{ line, col, off int }{
		{1, 1, 0},   // SELECT
		{1, 8, 7},   // a
		{1, 9, 8},   // ,
		{2, 3, 12},  // 'é你'
		{2, 8, 20},  // ,
		{2, 10, 22}, // b
		{3, 1, 29},  // ,
		{3, 2, 30},  // z
		{3, 3, 31},  // EOF
	}
	require.Len(t, toks, len(want))
	for i, w := range want {
		assert.Equal(t, w.line, toks[i].Pos.Line, "token %d line", i)
		assert.Equal(t, w.col, toks[i].Pos.Col, "token %d column", i)
		assert.Equal(t, w.off, toks[i].Pos.Offset, "token %d offset", i)
	}
}

func TestLexErrors(t *testing.T) {
	tests := []struct {
		name      string
		src       string
		line, col int
		want      string
	}{
		{"unexpected character", "SELECT @", 1, 8, "unexpected character '@'"},
		{"a lone bang", "a ! b", 1, 3, "unexpected character '!'"},
		{"on the third line", "SELECT\n\n  ~", 3, 3, "unexpected character '~'"},
		{"a non ASCII letter", "é", 1, 1, "unexpected character 'é'"},
		{"double quoted name", `"quoted"`, 1, 1, "quoted identifiers are not supported"},
		{"back quoted name", "`x`", 1, 1, "quoted identifiers are not supported"},
		{"unterminated string", "'abc", 1, 1, "unterminated string"},
		{"unterminated string on line 2", "x\n  'abc\n", 2, 3, "unterminated string"},
		{"invalid UTF-8 in a string", "'a\xffb'", 1, 3, "invalid UTF-8"},
		{"letters after a number", "12abc", 1, 1, "malformed number"},
		{"exponent without digits", "1e", 1, 1, "malformed number"},
		{"exponent sign without digits", "1e+", 1, 1, "malformed number"},
		{"fraction and exponent without digits", "2.5e", 1, 1, "malformed number"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Lex(tc.src)
			var se *SyntaxError
			require.True(t, errors.As(err, &se), "got %v", err)
			assert.Equal(t, tc.line, se.Pos.Line)
			assert.Equal(t, tc.col, se.Pos.Col)
			assert.Contains(t, se.Msg, tc.want)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestEveryKindHasAUniqueName(t *testing.T) {
	seen := map[string]Kind{}
	for k := Kind(0); k < kindCount; k++ {
		name := k.String()
		require.NotEmpty(t, name, "kind %d", k)
		prev, dup := seen[name]
		require.False(t, dup, "kind %d and %d are both called %q", prev, k, name)
		seen[name] = k
	}
	assert.Equal(t, "Kind(200)", Kind(200).String())
}

func TestKeywordsInAnyCase(t *testing.T) {
	for k := KwSelect; k <= KwGroup; k++ {
		require.True(t, k.IsKeyword())
		word := k.String()
		for _, src := range []string{word, strings.ToLower(word)} {
			assert.Equal(t, []tk{{k, src}}, lexAll(t, src))
		}
		assert.Equal(t, []tk{{Ident, word + "x"}}, lexAll(t, word+"x"), "a longer word is a name")
	}
	assert.False(t, Ident.IsKeyword())
	assert.False(t, GtEq.IsKeyword())
}

// oraclePos finds a position the slow and obvious way.
func oraclePos(src string, off int) (line, col int) {
	line, start := 1, 0
	for i := 0; i < off; i++ {
		if src[i] == '\n' {
			line++
			start = i + 1
		}
	}
	return line, utf8.RuneCountInString(src[start:off]) + 1
}

// checkPositions lexes src and compares every position, of tokens and of
// errors with the slow oracle
func checkPositions(tb testing.TB, src string) {
	tb.Helper()
	toks, err := Lex(src)
	if err != nil {
		var se *SyntaxError
		require.ErrorAs(tb, err, &se)
		require.LessOrEqual(tb, se.Pos.Offset, len(src))
		line, col := oraclePos(src, se.Pos.Offset)
		require.Equal(tb, []int{line, col}, []int{se.Pos.Line, se.Pos.Col}, "error position in %q", src)
		return
	}
	require.Equal(tb, EOF, toks[len(toks)-1].Kind)
	require.Equal(tb, len(src), toks[len(toks)-1].Pos.Offset, "EOF is at the end of %q", src)
	prev := -1
	for _, tok := range toks {
		require.Greater(tb, tok.Pos.Offset, prev, "token offsets must increase in %q", src)
		prev = tok.Pos.Offset
		line, col := oraclePos(src, tok.Pos.Offset)
		require.Equal(tb, []int{line, col}, []int{tok.Pos.Line, tok.Pos.Col}, "token %v in %q", tok.Kind, src)
	}
}

func TestPositionMatchTheOracle(t *testing.T) {
	frags := []string{
		"SELECT", " ", "\n", "\r\n", "  ", "\t", "a", "b1", "'x'", "'é你'", "'two\nlines'",
		"-- c\n", "--c", "12", "1.5", "1e3", ",", "(", ")", ";", "<=", "<>", "!=", "'", "@", "é", `"`,
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 3000; i++ {
		var b strings.Builder
		for n := 1 + rng.Intn(30); n > 0; n-- {
			b.WriteString(frags[rng.Intn(len(frags))])
		}
		checkPositions(t, b.String())
	}
}

func TestLongLineIsLexedInLinearTime(t *testing.T) {
	src := "INSERT INTO t VALUES " + strings.Repeat("(1,'a'),", 50000) + "(2,'b');"
	start := time.Now()
	toks, err := Lex(src)
	require.NoError(t, err)
	assert.Greater(t, len(toks), 300000)
	assert.Less(t, time.Since(start), 5*time.Second, "positions must not be found by rescanning the line")
}

//	 The properties: LEx never panics, positions are right, the tokens come in
//	 order and end with EOF, and a string token is valid UTF-8. Run it after real with:
//
//		go test -fuzz=FuzzLex -fuzztime=20s -fuzzminimizetime=0 ./internal/sql/
func FuzzLex(f *testing.F) {
	for _, s := range []string{
		"", "SELECT * FROM t;", "'it''s'", "a--b\nc", "1.5e+3", "'é\n你'", "a <> b != c", `"x"`, "'unterminated",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		checkPositions(t, src)
		toks, err := Lex(src)
		if err != nil {
			return
		}
		for _, tok := range toks {
			if tok.Kind == StringLit {
				require.True(t, utf8.ValidString(tok.Text))
			}
		}
	})
}
