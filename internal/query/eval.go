package query

import (
	"fmt"
	"math"
	"strings"

	"github.com/wilfex81/DB_Engine/internal/sql"
	"github.com/wilfex81/DB_Engine/internal/types"
)

// RuntimeError is a failure while evaluating a bound expression: division by
// Zero, or a number that does not fit. Pos is where in the SQL text it happened.
type RuntimeError struct {
	Pos sql.Pos
	Msg string
}

func (e *RuntimeError) Error() string { return fmt.Sprintf("runtime error at %s: %s", e.Pos, e.Msg) }

func runtimeErrorf(pos sql.Pos, format string, args ...any) error {
	return &RuntimeError{Pos: pos, Msg: fmt.Sprintf(format, args...)}
}

// Eval computes e for one row. A row holds the columns of the table in order.
//
// NULL follows SQL: arithmetic and comparisons with a NULL give NULL, and AND,
// OR and NOT use three valued logic. AND and OR go left to right and stop as
// soon as the answer is known, son "b <> 0 AND a / b > 1" is safe to write.
// Everything else evaluates both sides before looking at NULLS
func Eval(e Expr, row []types.Value) (types.Value, error) {
	switch e := e.(type) {
	case *Const:
		if e.untyped {
			return types.Value{}, fmt.Errorf("query: internal error: an untyped NULL reached the evaluator")
		}
		return e.Value, nil
	case *ColRef:
		if e.Pos < 0 || e.Pos >= len(row) {
			return types.Value{}, fmt.Errorf("query: internal error: column %q is position %d but the row has %d values", e.Name, e.Pos, len(row))
		}
		return row[e.Pos], nil
	case *Cast:
		v, err := Eval(e.X, row)
		if err != nil {
			return types.Value{}, err
		}
		if e.To != types.Float || v.Type() != types.Int {
			return types.Value{}, fmt.Errorf("query: internal error: cannot cast %s to %s", v.Type(), e.To)
		}
		if v.IsNull() {
			return types.Null(types.Float), nil
		}
		return types.NewFloat(float64(v.Int())), nil

	case *Unary:
		return evalUnary(e, row)

	case *Binary:
		switch e.Op {
		case sql.OpAnd:
			return evalAnd(e, row)
		case sql.OpOr:
			return evalOr(e, row)
		}
		l, err := Eval(e.L, row)
		if err != nil {
			return types.Value{}, err
		}
		r, err := Eval(e.R, row)
		if err != nil {
			return types.Value{}, err
		}
		switch e.Op {
		case sql.OpEq, sql.OpNotEq, sql.OpLt, sql.OpLtEq, sql.OpGt, sql.OpGtEq:
			return evalCompare(e, l, r)
		}
		return evalArith(e, l, r)

	case *IsNull:
		v, err := Eval(e.X, row)
		if err != nil {
			return types.Value{}, err
		}
		return types.NewBool(v.IsNull() != e.Negated), nil
	}
	return types.Value{}, fmt.Errorf("query: internal error: cannot evaluate %T", e)
}

// Matches reports whether a WHERE condition holds for a row. A nil condition
// always holds, and NULL counts as not holding.
func Matches(cond Expr, row []types.Value) (bool, error) {
	if cond == nil {
		return true, nil
	}
	v, err := Eval(cond, row)
	if err != nil {
		return false, err
	}
	return !v.IsNull() && v.Bool(), nil
}

func evalUnary(e *Unary, row []types.Value) (types.Value, error) {
	v, err := Eval(e.X, row)
	if err != nil {
		return types.Value{}, err
	}
	if e.Op == sql.OpNot {
		if v.IsNull() {
			return types.Null(types.Bool), nil
		}
		return types.NewBool(!v.Bool()), nil
	}
	if v.IsNull() {
		return v, nil
	}
	if v.Type() == types.Int {
		if v.Int() == math.MinInt64 {
			return types.Value{}, runtimeErrorf(e.At, "integer overflow")
		}
		return types.NewInt(-v.Int()), nil
	}
	return types.NewFloat(-v.Float()), nil
}

func evalAnd(e *Binary, row []types.Value) (types.Value, error) {
	l, err := Eval(e.L, row)
	if err != nil {
		return types.Value{}, err
	}
	if !l.IsNull() && !l.Bool() {
		return types.NewBool(false), nil // FALSE AND anything is FALSE
	}
	r, err := Eval(e.R, row)
	if err != nil {
		return types.Value{}, err
	}
	switch {
	case !r.IsNull() && !r.Bool():
		return types.NewBool(false), nil
	case l.IsNull() || r.IsNull():
		return types.Null(types.Bool), nil
	}
	return types.NewBool(true), nil
}

func evalOr(e *Binary, row []types.Value) (types.Value, error) {
	l, err := Eval(e.L, row)
	if err != nil {
		return types.Value{}, err
	}
	if !l.IsNull() && l.Bool() {
		return types.NewBool(true), nil // TRUE OR anything is TRUE
	}
	r, err := Eval(e.R, row)
	if err != nil {
		return types.Value{}, err
	}
	switch {
	case !r.IsNull() && r.Bool():
		return types.NewBool(true), nil
	case l.IsNull() || r.IsNull():
		return types.Null(types.Bool), nil
	}
	return types.NewBool(false), nil
}

// evalCompare compares two values of the same type: the binder has already
// turned an INT next to a FLOAT into two FLOATs. Text compares byte by byte,
// and FALSE is smaller than TRUE
func evalCompare(e *Binary, l, r types.Value) (types.Value, error) {
	if l.IsNull() || r.IsNull() {
		return types.Null(types.Bool), nil
	}
	if l.Type() != r.Type() {
		return types.Value{}, fmt.Errorf("query: internal error: comparing %s with %s", l.Type(), r.Type())
	}
	c := compareNonNull(l, r)
	switch l.Type() {
	case types.Int:
		c = cmp(l.Int(), r.Int())
	case types.Float:
		c = cmp(l.Float(), r.Float())
	case types.Text:
		c = strings.Compare(l.Text(), r.Text())
	case types.Bool:
		c = cmp(b2i(l.Bool()), b2i(r.Bool()))
	}
	var ok bool
	switch e.Op {
	case sql.OpEq:
		ok = c == 0
	case sql.OpNotEq:
		ok = c != 0
	case sql.OpLt:
		ok = c < 0
	case sql.OpLtEq:
		ok = c <= 0
	case sql.OpGt:
		ok = c > 0
	case sql.OpGtEq:
		ok = c >= 0
	}
	return types.NewBool(ok), nil
}

// compareNonNull orders two values of the same type, neither of them NULL:
// -1, 0 or 1. Text compares byte by byte, and FALSE is smaller than TRUE.
func compareNonNull(l, r types.Value) int {
	switch l.Type() {
	case types.Int:
		return cmp(l.Int(), r.Int())
	case types.Float:
		return cmp(l.Float(), r.Float())
	case types.Text:
		return strings.Compare(l.Text(), r.Text())
	case types.Bool:
		return cmp(b2i(l.Bool()), b2i(r.Bool()))
	}
	return 0
}

func cmp[T int | int64 | float64](a, b T) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// evalArith does + - * / %. A NULL on either side gives NULL, checked before
// anything that could fail. INT results are checked for overflow, and an
// FLOAT result must be a finite number, so the database never holds an
// infinity or NAN made by  a calculation.
func evalArith(e *Binary, l, r types.Value) (types.Value, error) {
	if l.IsNull() || r.IsNull() {
		return types.Null(e.Typ), nil
	}
	if e.Typ == types.Int {
		return arithInt(e, l.Int(), r.Int())
	}
	return arithFloat(e, l.Float(), r.Float())
}

func arithInt(e *Binary, a, b int64) (types.Value, error) {
	overflow := func() (types.Value, error) { return types.Value{}, runtimeErrorf(e.At, "integer overflow") }
	switch e.Op {
	case sql.OpAdd:
		c := a + b
		if (b > 0 && c < a) || (b < 0 && c > a) {
			return overflow()
		}
		return types.NewInt(c), nil
	case sql.OpSub:
		c := a - b
		if (b > 0 && c > a) || (b < 0 && c < a) {
			return overflow()
		}
		return types.NewInt(c), nil
	case sql.OpMul:
		c := a * b
		if a != 0 && (c/a != b || (a == -1 && b == math.MinInt64)) {
			return overflow()
		}
		return types.NewInt(c), nil
	case sql.OpDiv:
		if b == 0 {
			return types.Value{}, runtimeErrorf(e.At, "division by zero")
		}
		if a == math.MinInt64 && b == -1 {
			return overflow()
		}
		return types.NewInt(a / b), nil // truncates towards zero, like PostgreSQL
	case sql.OpMod:
		if b == 0 {
			return types.Value{}, runtimeErrorf(e.At, "division by zero")
		}
		return types.NewInt(a % b), nil // the sign follows the left side
	}
	return types.Value{}, fmt.Errorf("query: internal error: operator %s on INT", e.Op)

}

func arithFloat(e *Binary, a, b float64) (types.Value, error) {
	var c float64
	switch e.Op {
	case sql.OpAdd:
		c = a + b
	case sql.OpSub:
		c = a - b
	case sql.OpMul:
		c = a * b
	case sql.OpDiv:
		if b == 0 {
			return types.Value{}, runtimeErrorf(e.At, "division by zero")
		}
		c = a / b
	default:
		return types.Value{}, fmt.Errorf("query: internal error: operator %s on FLOAT", e.Op)
	}
	if math.IsNaN(c) {
		return types.Value{}, runtimeErrorf(e.At, "the FLOAT result is not a number")
	}
	if math.IsInf(c, 0) {
		return types.Value{}, runtimeErrorf(e.At, "FLOAT overflow")
	}
	return types.NewFloat(c), nil
}
