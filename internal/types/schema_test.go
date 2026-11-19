package types

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustSchema(tb testing.TB, cols ...Column) *Schema {
	tb.Helper()
	s, err := NewSchema(cols)
	require.NoError(tb, err)
	return s
}

func peopleSchema(tb testing.TB) *Schema {
	return mustSchema(tb,
		Column{Name: "id", Type: Int, NotNull: true},
		Column{Name: "name", Type: Text},
		Column{Name: "active", Type: Bool},
		Column{Name: "score", Type: Float},
	)
}

func TestNewSchemaRejectsBadInput(t *testing.T) {
	many := make([]Column, MaxColumns+1)
	for i := range many {
		many[i] = Column{Name: fmt.Sprintf("c%d", i), Type: Int}
	}

	tests := []struct {
		name string
		cols []Column
		want string
	}{
		{"no columns", nil, "at least one"},
		{"blank name", []Column{{Name: "  ", Type: Int}}, "no name"},
		{"zero type", []Column{{Name: "a", Type: Type(0)}}, "invalid type"},
		{"unknown type", []Column{{Name: "a", Type: Type(9)}}, "invalid type"},
		{"duplicate ignoring case", []Column{{Name: "a", Type: Int}, {Name: "A", Type: Text}}, "duplicate"},
		{"too many", many, "too many"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewSchema(tc.cols)
			assert.ErrorContains(t, err, tc.want)
		})
	}
}

func TestSchemaLookup(t *testing.T) {
	s := peopleSchema(t)
	assert.Equal(t, 4, s.Len())

	i, ok := s.Index("NAME") // names ignore case
	assert.True(t, ok)
	assert.Equal(t, 1, i)
	_, ok = s.Index("missing")
	assert.False(t, ok)
	assert.Equal(t, "name", s.Column(1).Name)

	// the schema owns its data: changing what we got back or what we passed in changes nothing
	cols := s.Columns()
	cols[0].Name = "hacked"
	assert.Equal(t, "id", s.Column(0).Name)

	in := []Column{{Name: "a", Type: Int}}
	s2 := mustSchema(t, in...)
	in[0].Name = "b"
	assert.Equal(t, "a", s2.Column(0).Name)
}

func TestRowRoundTrip(t *testing.T) {
	s := peopleSchema(t)
	tests := []struct {
		name string
		vals []Value
	}{
		{"full row", []Value{NewInt(1), NewText("Wilfex"), NewBool(true), NewFloat(98.5)}},
		{"nulls", []Value{NewInt(2), Null(Text), Null(Bool), Null(Float)}},
		{"empty text", []Value{NewInt(3), NewText(""), NewBool(false), NewFloat(0)}},
		{"smallest int, -infinity", []Value{NewInt(math.MinInt64), NewText("x"), NewBool(true), NewFloat(math.Inf(-1))}},
		{"largest int, unicode", []Value{NewInt(math.MaxInt64), NewText("Jambo, Nairobi: 你好"), NewBool(false), NewFloat(math.SmallestNonzeroFloat64)}},
		{"longest text", []Value{NewInt(4), NewText(strings.Repeat("a", MaxTextLen)), NewBool(true), NewFloat(1)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := s.Encode(tc.vals)
			require.NoError(t, err)
			got, err := s.Decode(b)
			require.NoError(t, err)
			assert.Equal(t, tc.vals, got)
		})
	}
}

// more than 8 columns, so the null bitmap spans several bytes
func TestWideSchemaNullBitmap(t *testing.T) {
	cols := make([]Column, 20)
	for i := range cols {
		cols[i] = Column{Name: fmt.Sprintf("c%d", i), Type: Int}
	}
	s := mustSchema(t, cols...)

	vals := make([]Value, 20)
	for i := range vals {
		if i%2 == 1 {
			vals[i] = Null(Int)
		} else {
			vals[i] = NewInt(int64(i))
		}
	}

	b, err := s.Encode(vals)
	require.NoError(t, err)
	assert.Len(t, b, 3+10*8) // 3 bitmap bytes, and 10 ints that are not NULL

	got, err := s.Decode(b)
	require.NoError(t, err)
	assert.Equal(t, vals, got)
}

// These exact bytes end up in database files. If this test ever needs a
// change, the file format changed, and old files stop working.
func TestEncodedFormatIsStable(t *testing.T) {
	one := mustSchema(t, Column{Name: "a", Type: Int, NotNull: true})
	b, err := one.Encode([]Value{NewInt(1)})
	require.NoError(t, err)
	assert.Equal(t, []byte{0, 1, 0, 0, 0, 0, 0, 0, 0}, b) // empty bitmap, then 1 as 8 bytes

	two := mustSchema(t, Column{Name: "a", Type: Int}, Column{Name: "b", Type: Text})
	b, err = two.Encode([]Value{Null(Int), NewText("hi")})
	require.NoError(t, err)
	assert.Equal(t, []byte{1, 2, 0, 'h', 'i'}, b) // bit 0 set, then length 2, then "hi"

	three := mustSchema(t, Column{Name: "ok", Type: Bool}, Column{Name: "x", Type: Float})
	b, err = three.Encode([]Value{NewBool(true), NewFloat(1)})
	require.NoError(t, err)
	// 1.0 is 0x3FF0000000000000, stored low byte first
	assert.Equal(t, []byte{0, 1, 0, 0, 0, 0, 0, 0, 0xF0, 0x3F}, b)
}

func TestEncodeRejectsBadRows(t *testing.T) {
	s := peopleSchema(t)
	good := []Value{NewInt(1), NewText("a"), NewBool(true), NewFloat(1)}
	with := func(i int, v Value) []Value {
		out := append([]Value(nil), good...)
		out[i] = v
		return out
	}

	tests := []struct {
		name string
		vals []Value
		want string
	}{
		{"too few values", good[:3], "4 columns"},
		{"too many values", append(append([]Value(nil), good...), NewInt(9)), "4 columns"},
		{"wrong type", with(1, NewInt(5)), "expects TEXT"},
		{"NULL in a NOT NULL column", with(0, Null(Int)), "cannot be NULL"},
		{"text too long", with(1, NewText(strings.Repeat("a", MaxTextLen+1))), "too long"},
		{"invalid UTF-8", with(1, NewText("\xff\xfe")), "UTF-8"},
		{"NaN", with(3, NewFloat(math.NaN())), "NaN"},
		{"a value nobody set", with(2, Value{}), "expects BOOL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Encode(tc.vals)
			assert.ErrorContains(t, err, tc.want)
		})
	}
}

func TestDecodeRejectsDamagedBytes(t *testing.T) {
	s := peopleSchema(t)
	good, err := s.Encode([]Value{NewInt(1), NewText("hi"), NewBool(true), NewFloat(1)})
	require.NoError(t, err)

	// layout: [0] bitmap, [1..8] id, [9..10] text length, [11..12] "hi", [13] bool, [14..21] float
	damage := func(f func(b []byte)) []byte {
		b := append([]byte(nil), good...)
		f(b)
		return b
	}

	tests := []struct {
		name string
		b    []byte
		want string
	}{
		{"empty", nil, "bitmap"},
		{"extra byte at the end", append(append([]byte(nil), good...), 0), "trailing"},
		{"unused bitmap bit set", damage(func(b []byte) { b[0] |= 1 << 6 }), "bitmap"},
		{"NULL in a NOT NULL column", damage(func(b []byte) { b[0] |= 1 }), "NULL"},
		{"bool that is not 0 or 1", damage(func(b []byte) { b[13] = 2 }), "bool"},
		{"text that is not UTF-8", damage(func(b []byte) { b[11] = 0xff }), "UTF-8"},
		{"NaN", damage(func(b []byte) {
			binary.LittleEndian.PutUint64(b[14:], math.Float64bits(math.NaN()))
		}), "NaN"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Decode(tc.b)
			assert.ErrorIs(t, err, ErrCorrupt)
			assert.ErrorContains(t, err, tc.want)
		})
	}
}

// a row cut off anywhere, as a crash can leave it, must be rejected
func TestEveryTruncationIsRejected(t *testing.T) {
	s := peopleSchema(t)
	good, err := s.Encode([]Value{NewInt(1), NewText("hello"), NewBool(true), NewFloat(2.5)})
	require.NoError(t, err)

	for i := 0; i < len(good); i++ {
		_, err := s.Decode(good[:i])
		assert.ErrorIs(t, err, ErrCorrupt, "cut at %d of %d bytes", i, len(good))
	}
}

// Pages are reused by the buffer pool, so a decoded row must not point into
// the page it came from.
func TestDecodedValuesOwnTheirBytes(t *testing.T) {
	s := peopleSchema(t)
	b, err := s.Encode([]Value{NewInt(1), NewText("hello"), NewBool(true), NewFloat(1)})
	require.NoError(t, err)

	got, err := s.Decode(b)
	require.NoError(t, err)

	for i := range b {
		b[i] = 0xAA // the page gets reused for something else
	}
	assert.Equal(t, "hello", got[1].Text())
}

// The property: any bytes that decode must encode back to exactly the same
// bytes, and nothing may panic. Run it for real with:
//
//	go test -fuzz=FuzzDecode -fuzztime=20s ./internal/types/
func FuzzDecode(f *testing.F) {
	s := peopleSchema(f)
	good, err := s.Encode([]Value{NewInt(1), NewText("hi"), NewBool(true), NewFloat(1)})
	require.NoError(f, err)
	f.Add(good)
	f.Add([]byte{})
	f.Add([]byte{0xff})

	f.Fuzz(func(t *testing.T, data []byte) {
		vals, err := s.Decode(data)
		if err != nil {
			return // rejecting bad bytes is fine
		}
		again, err := s.Encode(vals)
		require.NoError(t, err)
		require.Equal(t, data, again)
	})
}
