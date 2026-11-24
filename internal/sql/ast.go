package sql

import (
	"strconv"
	"strings"
)

// Expr is a node of an expression tree.
type Expr interface {
	// Start is the place to point at in an error about this node: the
	// operator for an operator, the first character for everything else.
	Start() Pos
	// String prints the expression fully parenthesized, so the grouping can
	// be seen. It is meant for tests and error messages.
	String() string
	exprNode()
}

type node struct{ At Pos }

func (n node) Start() Pos { return n.At }
func (node) exprNode()    {}

// IntLiteral is a whole number. A minus sign directly in front of it is part of it.
type IntLiteral struct {
	node
	Value int64
}

// FloatLiteral is a number with a fraction or an exponent.
type FloatLiteral struct {
	node
	Value float64
}

type StringLiteral struct {
	node
	Value string
}

type BoolLiteral struct {
	node
	Value bool
}

type NullLiteral struct{ node }

// ColumnRef is a column, with the table in front of it if one was written.
type ColumnRef struct {
	node
	Table, Name string
}

type UnaryOp uint8

const (
	OpNeg UnaryOp = iota + 1
	OpNot
)

type UnaryExpr struct {
	node
	Op UnaryOp
	X  Expr
}

type BinaryOp uint8

const (
	OpAdd BinaryOp = iota + 1
	OpSub
	OpMul
	OpDiv
	OpMod
	OpEq
	OpNotEq
	OpLt
	OpLtEq
	OpGt
	OpGtEq
	OpAnd
	OpOr
)

var binaryOpNames = map[BinaryOp]string{
	OpAdd: "+", OpSub: "-", OpMul: "*", OpDiv: "/", OpMod: "%",
	OpEq: "=", OpNotEq: "<>", OpLt: "<", OpLtEq: "<=", OpGt: ">", OpGtEq: ">=",
	OpAnd: "AND", OpOr: "OR",
}

func (op BinaryOp) String() string { return binaryOpNames[op] }

type BinaryExpr struct {
	node
	Op   BinaryOp
	L, R Expr
}

// IsNullExpr is X IS NULL, or X IS NOT NULL when Negated is set.
type IsNullExpr struct {
	node
	X       Expr
	Negated bool
}

// FuncCall is name(args), or name(*). Which functions exist is up to the binder.
type FuncCall struct {
	node
	Name string
	Args []Expr
	Star bool
}

func (e *IntLiteral) String() string    { return strconv.FormatInt(e.Value, 10) }
func (e *FloatLiteral) String() string  { return formatFloat(e.Value) }
func (e *StringLiteral) String() string { return "'" + strings.ReplaceAll(e.Value, "'", "''") + "'" }
func (e *NullLiteral) String() string   { return "NULL" }

func (e *BoolLiteral) String() string {
	if e.Value {
		return "TRUE"
	}
	return "FALSE"
}

func (e *ColumnRef) String() string {
	if e.Table != "" {
		return e.Table + "." + e.Name
	}
	return e.Name
}

// A negative sign in front of a number literal is folded into the literal, so
// a negated number that was not folded, as in -(5), prints its operand in
// parentheses. That keeps the printed form parsing back to the same tree.
func (e *UnaryExpr) String() string {
	if e.Op == OpNot {
		return "(NOT " + e.X.String() + ")"
	}
	operand := e.X.String()
	switch e.X.(type) {
	case *IntLiteral, *FloatLiteral:
		operand = "(" + operand + ")"
	}
	return "(- " + operand + ")"
}

func (e *BinaryExpr) String() string {
	return "(" + e.L.String() + " " + e.Op.String() + " " + e.R.String() + ")"
}

func (e *IsNullExpr) String() string {
	if e.Negated {
		return "(" + e.X.String() + " IS NOT NULL)"
	}
	return "(" + e.X.String() + " IS NULL)"
}

func (e *FuncCall) String() string {
	if e.Star {
		return e.Name + "(*)"
	}
	args := make([]string, len(e.Args))
	for i, a := range e.Args {
		args[i] = a.String()
	}
	return e.Name + "(" + strings.Join(args, ", ") + ")"
}

// formatFloat prints a float so that it cannot be read back as an integer.
func formatFloat(f float64) string {
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eEIN") {
		s += ".0"
	}
	return s
}
