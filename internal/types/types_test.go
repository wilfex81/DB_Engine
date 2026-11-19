package types

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValueAccessors(t *testing.T) {
	assert.Equal(t, int64(-7), NewInt(-7).Int())
	assert.Equal(t, 2.5, NewFloat(2.5).Float())
	assert.True(t, NewBool(true).Bool())
	assert.False(t, NewBool(false).Bool())
	assert.Equal(t, "hi", NewText("hi").Text())

	assert.False(t, NewInt(1).IsNull())
	assert.True(t, Null(Text).IsNull())
	assert.Equal(t, Text, Null(Text).Type())
}

func TestWrongAccessorPanics(t *testing.T) {
	assert.Panics(t, func() { NewInt(1).Text() })
	assert.Panics(t, func() { NewText("x").Int() })
	assert.Panics(t, func() { Null(Int).Int() }) // a NULL has no integer inside
	assert.Panics(t, func() { Value{}.Int() })   // a zero Value was never set
}

func TestValuesCompareWithEquals(t *testing.T) {
	assert.True(t, NewInt(3) == NewInt(3))
	assert.False(t, NewInt(3) == NewInt(4))
	assert.False(t, NewInt(0) == Null(Int)) // NULL is not zero
	assert.True(t, Null(Text) == Null(Text))
}

func TestValueString(t *testing.T) {
	tests := []struct {
		v    Value
		want string
	}{
		{NewInt(42), "42"},
		{NewInt(-1), "-1"},
		{NewFloat(3.5), "3.5"},
		{NewBool(true), "true"},
		{NewBool(false), "false"},
		{NewText("Nairobi"), "Nairobi"},
		{Null(Int), "NULL"},
		{Value{}, "INVALID"},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, tc.v.String())
	}
}

func TestTypeString(t *testing.T) {
	assert.Equal(t, "INT", Int.String())
	assert.Equal(t, "FLOAT", Float.String())
	assert.Equal(t, "BOOL", Bool.String())
	assert.Equal(t, "TEXT", Text.String())
	assert.Equal(t, "Type(9)", Type(9).String())
}
