// defines colum types, values, and how a row becomes bytes
package types

import (
	"fmt"
	"math"
	"strconv"
)

type Type uint8

const (
	Int Type = iota + 1
	Float
	Bool
	Text
)

const MaxTextLen = math.MaxUint16

func (t Type) String() string {
	switch t {
	case Int:
		return "INT"
	case Float:
		return "FLOAT"
	case Bool:
		return "BOOL"
	case Text:
		return "TEXT"
	}
	return fmt.Sprintf("Type(%d)", uint8(t))
}

func (t Type) valid() bool { return t >= Int && t <= Text }

// One field of a row: can be a typed value or a NULL of some type
// Plain data that can be compared with ==
type Value struct {
	typ  Type
	null bool
	i    int64 // an Int, or a Bool (0 or 1)
	f    float64
	s    string
}

func NewInt(v int64) Value     { return Value{typ: Int, i: v} }
func NewFloat(v float64) Value { return Value{typ: Float, f: v} }
func NewText(v string) Value   { return Value{typ: Text, s: v} }
func Null(t Type) Value        { return Value{typ: t, null: true} }

func NewBool(v bool) Value {
	b := Value{typ: Bool}
	if v {
		b.i = 1
	}
	return b
}

func (v Value) Type() Type   { return v.typ }
func (v Value) IsNull() bool { return v.null }

// The accessors panic when the value is the wrong type, or NULL
// Using a value as the wrong type is a bug in this code, and data errors nevwe get here:
// the schema checks every value on the way in and on the way out.
func (v Value) Int() int64     { v.must(Int); return v.i }
func (v Value) Float() float64 { v.must(Float); return v.f }
func (v Value) Bool() bool     { v.must(Bool); return v.i == 1 }
func (v Value) Text() string   { v.must(Text); return v.s }

func (v Value) must(t Type) {
	if v.typ != t || v.null {
		panic(fmt.Sprintf("types: value %v of type %s read as %s", v, v.typ, t))
	}
}

func (v Value) String() string {
	if v.null {
		return "NULL"
	}
	switch v.typ {
	case Int:
		return strconv.FormatInt(v.i, 10)
	case Float:
		return strconv.FormatFloat(v.f, 'g', -1, 64)
	case Bool:
		if v.i == 1 {
			return "true"
		}
		return "false"
	case Text:
		return v.s
	}
	return "INVALID"
}
