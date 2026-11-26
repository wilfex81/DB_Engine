package query

import (
	"strconv"
	"strings"

	"github.com/wilfex81/DB_Engine/internal/sql"
	"github.com/wilfex81/DB_Engine/internal/types"
)

// Expr is a bound expression: names are resolved and every node has a type.
type Expr interface {
	Type() types.Type
	// String prints the expression with full parentheses, for tests and EXPLAIN.
	String() string
	boundExpr()
}

// Const is a literal. A NULL literal starts out untyped, and takes the type
// of whatever it is used with. After binding, every Const has a type.
type Const struct {
	Value   types.Value
	untyped bool
}

// ColRef reads column Pos of the input row.
type ColRef struct {
	Pos  int
	Name string
	Typ  types.Type
}

// Cast turns an INT into a FLOAT. It is the only conversion there is.
type Cast struct {
	X  Expr
	To types.Type
}

type Unary struct {
	At sql.Pos // where to point if evaluating fails
	Op sql.UnaryOp
	X  Expr
}

type Binary struct {
	At   sql.Pos
	Op   sql.BinaryOp
	L, R Expr
	Typ  types.Type
}

type IsNull struct {
	X       Expr
	Negated bool
}

func (*Const) boundExpr()  {}
func (*ColRef) boundExpr() {}
func (*Cast) boundExpr()   {}
func (*Unary) boundExpr()  {}
func (*Binary) boundExpr() {}
func (*IsNull) boundExpr() {}

// Type of an untyped NULL is 0, which is not a valid types.Type.
func (c *Const) Type() types.Type {
	if c.untyped {
		return 0
	}
	return c.Value.Type()
}

func (c *ColRef) Type() types.Type { return c.Typ }
func (c *Cast) Type() types.Type   { return c.To }
func (e *Binary) Type() types.Type { return e.Typ }
func (e *IsNull) Type() types.Type { return types.Bool }

func (e *Unary) Type() types.Type {
	if e.Op == sql.OpNot {
		return types.Bool
	}
	return e.X.Type()
}

func (c *Const) String() string {
	if c.untyped || c.Value.IsNull() {
		return "NULL"
	}
	switch c.Value.Type() {
	case types.Text:
		return "'" + strings.ReplaceAll(c.Value.Text(), "'", "''") + "'"
	case types.Float:
		return formatFloat(c.Value.Float())
	case types.Bool:
		if c.Value.Bool() {
			return "TRUE"
		}
		return "FALSE"
	}
	return c.Value.String()
}

func (c *ColRef) String() string { return c.Name }
func (c *Cast) String() string   { return "CAST(" + c.X.String() + " AS " + c.To.String() + ")" }

func (e *Unary) String() string {
	if e.Op == sql.OpNot {
		return "(NOT " + e.X.String() + ")"
	}
	return "(- " + e.X.String() + ")"
}

func (e *Binary) String() string {
	return "(" + e.L.String() + " " + e.Op.String() + " " + e.R.String() + ")"
}

func (e *IsNull) String() string {
	if e.Negated {
		return "(" + e.X.String() + " IS NOT NULL)"
	}
	return "(" + e.X.String() + " IS NULL)"
}

func formatFloat(f float64) string {
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eEIN") {
		s += ".0"
	}
	return s
}
