package types

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

// ErrCorrupt means stored bytes do not make a valid row for the schema
var ErrCorrupt = errors.New("types: corrupt tuple")

const MaxColumns = 1024

type Column struct {
	Name    string
	Type    Type
	NotNull bool
}

// descirbes the columns of  a table, and turns rows into bytes and back
type Schema struct {
	cols  []Column
	index map[string]int
}

func NewSchema(cols []Column) (*Schema, error) {
	if len(cols) == 0 {
		return nil, errors.New("types: a schema needs atleast oone column")
	}
	if len(cols) > MaxColumns {
		return nil, fmt.Errorf("types: too many columns (%d, the limit is %d)", len(cols), MaxColumns)
	}

	s := &Schema{
		cols:  append([]Column(nil), cols...),
		index: make(map[string]int, len(cols)),
	}
	for i, c := range cols {
		if strings.TrimSpace(c.Name) == "" {
			return nil, fmt.Errorf("types: column %d has no name", i)
		}
		if !c.Type.valid() {
			return nil, fmt.Errorf("types: column %q has an invalid type", c.Name)
		}
		key := strings.ToLower(c.Name) //SQL names are not case SENSITIVE
		if _, dup := s.index[key]; dup {
			return nil, fmt.Errorf("types: duplicate column name %q", c.Name)
		}
		s.index[key] = i
	}
	return s, nil
}

func (s *Schema) Len() int            { return len(s.cols) }
func (s *Schema) Column(i int) Column { return s.cols[i] }

// Columns returns a copy, so callers cannot change the schema.
func (s *Schema) Columns() []Column { return append([]Column(nil), s.cols...) }

// Index finds a column by name, ignoring case.
func (s *Schema) Index(name string) (int, bool) {
	i, ok := s.index[strings.ToLower(name)]
	return i, ok
}

func (s *Schema) bitmapLen() int { return (len(s.cols) + 7) / 8 }

// Encode turns one row into bytes. The layout is:
//
// null bitmap (one bit per column, bit i of byte i/8)
// then each non-NULL field in colum order:
//
//	 	INT, FLOAT	8 bytes,little endian
//			BOOL		1 byte, 0 or 1
//	 	TEXT		2 byte length, then the bytes
func (s *Schema) Encode(vals []Value) ([]byte, error) {
	if len(vals) != len(s.cols) {
		return nil, fmt.Errorf("types: got %d values for a schema with %d columns", len(vals), len(s.cols))
	}

	// check everything and work out the size first, so we allocate once
	size := s.bitmapLen()
	for i, v := range vals {
		c := s.cols[i]
		if v.typ != c.Type {
			return nil, fmt.Errorf("types: column %q expects %s, got %s", c.Name, c.Type, v.typ)
		}
		if v.null {
			if c.NotNull {
				return nil, fmt.Errorf("types: column %q cannot be NULL", c.Name)
			}
			continue
		}
		switch c.Type {
		case Int:
			size += 8
		case Float:
			if math.IsNaN(v.f) {
				// NaN is not equal to itself, which would break sorting and indexes
				return nil, fmt.Errorf("types: column %q cannot store NaN", c.Name)
			}
			size += 8
		case Bool:
			size++
		case Text:
			if len(v.s) > MaxTextLen {
				return nil, fmt.Errorf("types: column %q text is too long (%d bytes, the limit is %d)", c.Name, len(v.s), MaxTextLen)
			}
			if !utf8.ValidString(v.s) {
				return nil, fmt.Errorf("types: column %q text is not valid UTF-8", c.Name)
			}
			size += 2 + len(v.s)
		}
	}
	buf := make([]byte, size)
	pos := s.bitmapLen()
	for i, v := range vals {
		if v.null {
			buf[i/8] |= 1 << (i % 8)
			continue
		}
		switch s.cols[i].Type {
		case Int:
			binary.LittleEndian.PutUint64(buf[pos:], uint64(v.i))
			pos += 8
		case Float:
			binary.LittleEndian.PutUint64(buf[pos:], math.Float64bits(v.f))
			pos += 8
		case Bool:
			buf[pos] = byte(v.i)
			pos++
		case Text:
			binary.LittleEndian.PutUint16(buf[pos:], uint16(len(v.s)))
			pos += 2
			pos += copy(buf[pos:], v.s)
		}
	}
	return buf, nil
}

// Decode turns bytes back into a row
// Anything thay Encode could not have produces is
// reported as ErrCorrupt, so every valid row has exactly one byte form
func (s *Schema) Decode(b []byte) ([]Value, error) {
	n := len(s.cols)
	bl := s.bitmapLen()
	if len(b) < bl {
		return nil, fmt.Errorf("%w: too short for the null bitmap", ErrCorrupt)
	}
	if rem := n % 8; rem != 0 && b[bl-1]>>rem != 0 {
		return nil, fmt.Errorf("%w: unused null bitmap bites are set", ErrCorrupt)
	}

	vals := make([]Value, n)
	pos := bl
	for i, c := range s.cols {
		if b[i/8]&(1<<(i%8)) != 0 {
			if c.NotNull {
				return nil, fmt.Errorf("%w: NULL in NOT NULL column %q", ErrCorrupt, c.Name)
			}
			vals[i] = Null(c.Type)
			continue
		}

		switch c.Type {
		case Int:
			if len(b)-pos < 8 {
				return nil, fmt.Errorf("%w: column %q is cut short", ErrCorrupt, c.Name)
			}
			vals[i] = NewInt(int64(binary.LittleEndian.Uint64(b[pos:])))
			pos += 8
		case Float:
			if len(b)-pos < 8 {
				return nil, fmt.Errorf("%w: column %q is cut short", ErrCorrupt, c.Name)
			}
			f := math.Float64frombits(binary.LittleEndian.Uint64(b[pos:]))
			if math.IsNaN(f) {
				return nil, fmt.Errorf("%w: column %q cannot be NaN", ErrCorrupt, c.Name)
			}
			vals[i] = NewFloat(f)
			pos += 8
		case Bool:
			if len(b)-pos < 1 {
				return nil, fmt.Errorf("%w: column %q is cut short", ErrCorrupt, c.Name)
			}
			if b[pos] > 1 {
				return nil, fmt.Errorf("%w: bool in column %q must be 0 or 1", ErrCorrupt, c.Name)
			}
			vals[i] = NewBool(b[pos] == 1)
			pos++
		case Text:
			if len(b)-pos < 2 {
				return nil, fmt.Errorf("%w: column %q is cut short", ErrCorrupt, c.Name)
			}
			l := int(binary.LittleEndian.Uint16(b[pos:]))
			pos += 2
			if len(b)-pos < l {
				return nil, fmt.Errorf("%w: column %q is cut short", ErrCorrupt, c.Name)
			}
			str := string(b[pos : pos+l]) // string() copies, so the value owns its bytes
			if !utf8.ValidString(str) {
				return nil, fmt.Errorf("%w: column %q text is not valid UTF-8", ErrCorrupt, c.Name)
			}
			vals[i] = NewText(str)
			pos += l
		}
	}

	if pos != len(b) {
		return nil, fmt.Errorf("%w: %d trailing bytes", ErrCorrupt, len(b)-pos)
	}
	return vals, nil
}
