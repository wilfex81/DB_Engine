// Package sql turns SQL text into a syntax tree: a hand written layer, and
// later the parser. It uses nothing but the standard library, so it can be
// tested alone. Binding, planning, and execution live in other packages
package sql

import (
	"fmt"
)

// Kind is the type of a token
type Kind uint8

const (
	EOF Kind = iota
	Ident
	IntLit
	FloatLit
	StringLit

	LParen
	RParen
	Comma
	Semicolon
	Dot
	Star
	Plus
	Minus
	Slash
	Percent
	Eq
	NotEq
	Lt
	LtEq
	Gt
	GtEq

	// Reserved words. They cannot be used as names.
	KwSelect
	KwFrom
	KwWhere
	KwInsert
	KwInto
	KwValues
	KwUpdate
	KwSet
	KwDelete
	KwCreate
	KwDrop
	KwTable
	KwIndex
	KwUnique
	KwOn
	KwPrimary
	KwKey
	KwNot
	KwNull
	KwAnd
	KwOr
	KwIs
	KwTrue
	KwFalse
	KwOrder
	KwBy
	KwAsc
	KwDesc
	KwLimit
	KwAs
	KwJoin
	KwInner
	KwGroup

	kindCount
)

var kindNames = [kindCount]string{
	EOF: "end of input", Ident: "identifier", IntLit: "integer", FloatLit: "number", StringLit: "string",

	LParen: "(", RParen: ")", Comma: ",", Semicolon: ";", Dot: ".", Star: "*", Plus: "+", Minus: "-",
	Slash: "/", Percent: "%", Eq: "=", NotEq: "<>", Lt: "<", LtEq: "<=", Gt: ">", GtEq: ">=",

	KwSelect: "SELECT", KwFrom: "FROM", KwWhere: "WHERE", KwInsert: "INSERT", KwInto: "INTO",
	KwValues: "VALUES", KwUpdate: "UPDATE", KwSet: "SET", KwDelete: "DELETE", KwCreate: "CREATE",
	KwDrop: "DROP", KwTable: "TABLE", KwIndex: "INDEX", KwUnique: "UNIQUE", KwOn: "ON",
	KwPrimary: "PRIMARY", KwKey: "KEY", KwNot: "NOT", KwNull: "NULL", KwAnd: "AND", KwOr: "OR",
	KwIs: "IS", KwTrue: "TRUE", KwFalse: "FALSE", KwOrder: "ORDER", KwBy: "BY", KwAsc: "ASC",
	KwDesc: "DESC", KwLimit: "LIMIT", KwAs: "AS", KwJoin: "JOIN", KwInner: "INNER", KwGroup: "GROUP",
}

// String is how a kind appears in an error messages: a word for names and
// literals, the symbol itself for punctuation, the upper case word for keywords.
func (k Kind) String() string {
	if k < kindCount {
		return kindNames[k]
	}
	return fmt.Sprintf("Kind(%d)", uint8(k))
}

// IsKeyword reports whether k is a reserved word.
func (k Kind) IsKeyword() bool { return k >= KwSelect && k <= KwGroup }

// keywords maps na upper case word to its kind
var keywords = func() map[string]Kind {
	m := make(map[string]Kind)
	for k := KwSelect; k <= KwGroup; k++ {
		m[kindNames[k]] = k
	}
	return m
}()

// Pos is a place in the source. Line and Col count from 1, and Col counts
// characters, not bytes. Offset counts bytes from 0.
type Pos struct {
	Offset, Line, Col int
}

func (p Pos) String() string { return fmt.Sprintf("%d:%d", p.Line, p.Col) }

// Token is one piece of the source. Text is the source text, expect for a
// string, which holds its value with the quotes removed and ” turned into '.
// Numbers keep their digits as text: the parser converts them, so that the
// smallest int64, which has no positive twin, can be read.
type Token struct {
	Kind Kind
	Text string
	Pos  Pos
}

// SyntaxError is a problem in the SQL text, with its place.
type SyntaxError struct {
	Pos Pos
	Msg string
}

func (e *SyntaxError) Error() string { return fmt.Sprintf("syntax error at %s: %s", e.Pos, e.Msg) }

func syntaxErrorf(p Pos, format string, args ...any) error {
	return &SyntaxError{Pos: p, Msg: fmt.Sprintf(format, args...)}
}
