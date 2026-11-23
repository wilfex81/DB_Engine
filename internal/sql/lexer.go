package sql

import (
	"strings"
	"unicode/utf8"
)

type lexer struct {
	src      string
	i        int // the next byte read
	toks     []Token
	line     int // the line of the byte at i, from 1
	cacheOff int // a byte offset on the current line whose column is known
	cacheCol int // and thay column, so a column never needs rescan of the line
}

// Lex splits src into tokens. The last token is always EOF
//
// Names are ASCII: a letter or underscore, then letters, digits, and
// underscores, which is exactly what the catalog accepts. Words are keywords
// in any case. A -- starts a comment that runs to the end of the line
func Lex(src string) ([]Token, error) {
	l := &lexer{src: src, line: 1, cacheCol: 1}
	for {
		l.skip()
		if l.i >= len(src) {
			l.emit(EOF, "", l.pos())
			return l.toks, nil
		}
		if err := l.next(); err != nil {
			return nil, err
		}
	}

}

// pos is the position of the byte at i. i only moves forward, which is what
// keeps the whole lexer linear, even for one very long line.
func (l *lexer) pos() Pos {
	col := l.cacheCol + utf8.RuneCountInString(l.src[l.cacheOff:l.i])
	l.cacheOff, l.cacheCol = l.i, col
	return Pos{Offset: l.i, Line: l.line, Col: col}
}

// neline records that the byte at index at is a line feed
func (l *lexer) newline(at int) {
	l.line++
	l.cacheOff, l.cacheCol = at+1, 1
}

func (l *lexer) emit(k Kind, text string, p Pos) {
	l.toks = append(l.toks, Token{Kind: k, Text: text, Pos: p})
}

func (l *lexer) skip() {
	src := l.src
	for l.i < len(src) {
		switch c := src[l.i]; {
		case c == '\n':
			l.newline(l.i)
			l.i++
		case c == ' ' || c == '\t' || c == '\r':
			l.i++
		case c == '-' && l.i+1 < len(src) && src[l.i+1] == '-':
			for l.i < len(src) && src[l.i] != '\n' {
				l.i++
			}
		default:
			return
		}
	}
}

var (
	twoChar = map[string]Kind{"<>": NotEq, "!=": NotEq, "<=": LtEq, ">=": GtEq}
	singles = map[byte]Kind{
		'(': LParen, ')': RParen, ',': Comma, ';': Semicolon, '.': Dot, '*': Star, '+': Plus,
		'-': Minus, '/': Slash, '%': Percent, '=': Eq, '<': Lt, '>': Gt,
	}
)

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isDigit(c byte) bool  { return c >= '0' && c <= '9' }

func (l *lexer) next() error {
	start := l.pos()
	c := l.src[l.i]
	switch {
	case isLetter(c) || c == '-':
		l.ident(start)
		return nil
	case isDigit(c):
		return l.number(start)
	case c == '\'':
		return l.str(start)
	case c == '"' || c == '`':
		return syntaxErrorf(start, "quoted identifiers are not supported")
	}

	if l.i+1 < len(l.src) {
		if k, ok := twoChar[l.src[l.i:l.i+2]]; ok {
			l.op(k, 2, start)
			return nil
		}
	}
	if k, ok := singles[c]; ok {
		l.op(k, 1, start)
		return nil
	}
	r, _ := utf8.DecodeRuneInString(l.src[l.i:])
	return syntaxErrorf(start, "Unexpected character %q", r)
}

func (l *lexer) op(k Kind, n int, start Pos) {
	l.emit(k, l.src[l.i:l.i+n], start)
	l.i += n
}

func (l *lexer) ident(start Pos) {
	src := l.src
	j := l.i + 1
	for j < len(src) && (isLetter(src[j]) || isDigit(src[j]) || src[j] == '_') {
		j++
	}
	text := src[l.i:j]
	kind := Ident
	if kw, ok := keyword[strings.ToUpper(text)]; ok {
		kind = kw
	}
	l.emit(kind, text, start)
	l.i = j
}

// number reads digits, an optional fraction ( a dot with digits after it), and
// an optional exponent. A number may not run stright into a letter, so 12abc
// is an error and not a number followed by a name
func (l *lexer) number(start Pos) error {
	src, n := l.src, len(l.src)
	j := l.i
	for j < n && isDigit(src[j]) {
		j++
	}
	kind := IntLit
	if j+1 < n && src[j] == '.' && isDigit(src[j+1]) {
		kind = FloatLit
		j++
		for j < n && isDigit(src[j]) {
			j++
		}
	}
	if j < n && (src[j] == 'e' || src[j] == 'E') {
		k := j + 1
		if k < n && (src[k] == '+' || src[k] == '-') {
			k++
		}
		if k >= n || !isDigit(src[k]) {
			return l.malformed(start, j)
		}
		if k < n && isDigit(src[k]) {
			k++
		}
		kind, j = FloatLit, k
	}
	if j < n && (isLetter(src[j]) || src[j] == '_') {
		return l.malformed(start, j)
	}
	l.emit(kind, src[l.i:j], start)
	l.i = j
	return nil
}

func (l *lexer) malformed(start Pos, from int) error {
	end := from
	for end < len(l.src) && (isLetter(l.src[end]) || isDigit(l.src[end]) || l.src[end] == '_') {
		end++
	}
	return syntaxErrorf(start, "malformed number %q", l.src[l.i:end])
}

// str reads a string in single quotes. Two quotes in a row stand for one
// quote. A string may contain line feeds, and must be valid UTF-8, because
// it can become a TEXT value.
func (l *lexer) str(start Pos) error {
	src := l.src
	var b strings.Builder
	i := l.i + 1
	for i < len(src) {
		switch c := src[i]; {
		case c == '\'':
			if i+1 < len(src) && src[i+1] == '\'' {
				b.WriteByte('\'')
				i += 2
				continue
			}
			l.i = i + 1
			l.emit(StringLit, b.String(), start)
			return nil
		case c == '\n':
			l.newline(i)
			b.WriteByte(c)
			i++
		case c >= utf8.RuneSelf:
			r, size := utf8.DecodeRuneInString(src[i:])
			if r == utf8.RuneError && size == 1 {
				l.i = i
				return syntaxErrorf(l.pos(), "invalid UTF-8 in a string")
			}
			b.WriteString(src[i : i+size])
			i += size
		default:
			b.WriteByte(c)
			i++
		}
	}
	return syntaxErrorf(start, "unterminated string")
}
