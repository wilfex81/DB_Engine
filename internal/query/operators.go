package query

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/wilfex81/DB_Engine/internal/catalog"
	"github.com/wilfex81/DB_Engine/internal/index"
	"github.com/wilfex81/DB_Engine/internal/table"
	"github.com/wilfex81/DB_Engine/internal/types"
)

// Dual produces exactly one row with no columns. It is the source of a
// SELECT that has no FROM.
type Dual struct{ done bool }

func (d *Dual) Next() (Row, bool, error) {
	if d.done {
		return Row{}, false, nil
	}
	d.done = true
	return Row{}, true, nil
}

func (d *Dual) describe() (string, []Operator) { return "Dual", nil }

// SeqScan reads every row of a table, in the order the heap holds them.
type SeqScan struct {
	tableName string
	it        *table.Iterator
}

func (s *SeqScan) Next() (Row, bool, error) {
	if !s.it.Next() {
		return Row{}, false, s.it.Err()
	}
	return Row{Values: s.it.Values(), RID: s.it.RID()}, true, nil
}

func (s *SeqScan) describe() (string, []Operator) { return "SeqScan " + s.tableName, nil }

// IndexScan walks the keys lo to hi of an index in ascending order, and reads
// the row of each key from the heap. lo == hi is a lookup of one key.
type IndexScan struct {
	tableName string
	ix        *catalog.Index
	heap      *table.Heap
	lo, hi    int64
	it        *index.Iterator
}

func (s *IndexScan) Next() (Row, bool, error) {
	if !s.it.Next() {
		return Row{}, false, s.it.Err()
	}
	vals, err := s.heap.Get(s.it.RID())
	if err != nil {
		return Row{}, false, fmt.Errorf("query: the index %s points at a missing row: %w", s.ix.Name, err)
	}
	return Row{Values: vals, RID: s.it.RID()}, true, nil
}

func (s *IndexScan) describe() (string, []Operator) {
	col := s.ix.Column
	lo, hi := strconv.FormatInt(s.lo, 10), strconv.FormatInt(s.hi, 10)
	var cond string
	switch {
	case s.lo == s.hi:
		cond = col + " = " + lo
	case s.lo == math.MinInt64 && s.hi == math.MaxInt64:
		cond = col + " ordered"
	case s.lo == math.MinInt64:
		cond = col + " <= " + hi
	case s.hi == math.MaxInt64:
		cond = col + " >= " + lo
	default:
		cond = col + " BETWEEN " + lo + " AND " + hi
	}
	return fmt.Sprintf("IndexScan %s using %s (%s)", s.tableName, s.ix.Name, cond), nil
}

// Filter passes the rows for which the condition is TRUE. FALSE and NULL both drop a row.
type Filter struct {
	child Operator
	cond  Expr
}

func (f *Filter) Next() (Row, bool, error) {
	for {
		r, ok, err := f.child.Next()
		if err != nil || !ok {
			return Row{}, false, err
		}
		keep, err := Matches(f.cond, r.Values)
		if err != nil {
			return Row{}, false, err
		}
		if keep {
			return r, true, nil
		}
	}
}

func (f *Filter) describe() (string, []Operator) {
	return "Filter " + f.cond.String(), []Operator{f.child}
}

// Sort reads every row of its child into memory on the first call, sorts
// them, and hands them out. The sort is stable, so rows with equal keys keep
// the order they came in. NULL counts as larger than any value, so it comes
// last in ascending order and first in descending order, as in PostgreSQL.
type Sort struct {
	child  Operator
	keys   []OrderItem
	rows   []sortRow
	next   int
	loaded bool
}

type sortRow struct {
	row  Row
	keys []types.Value
}

func compareForSort(a, b types.Value) int {
	switch {
	case a.IsNull() && b.IsNull():
		return 0
	case a.IsNull():
		return 1
	case b.IsNull():
		return -1
	}
	return compareNonNull(a, b)
}

func (s *Sort) load() error {
	for {
		r, ok, err := s.child.Next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		keys := make([]types.Value, len(s.keys))
		for i, k := range s.keys {
			if keys[i], err = Eval(k.Expr, r.Values); err != nil {
				return err
			}
		}
		s.rows = append(s.rows, sortRow{row: r, keys: keys})
	}
	sort.SliceStable(s.rows, func(i, j int) bool {
		for k, key := range s.keys {
			c := compareForSort(s.rows[i].keys[k], s.rows[j].keys[k])
			if key.Desc {
				c = -c
			}
			if c != 0 {
				return c < 0
			}
		}
		return false
	})
	return nil
}

func (s *Sort) Next() (Row, bool, error) {
	if !s.loaded {
		if err := s.load(); err != nil {
			return Row{}, false, err
		}
		s.loaded = true
	}
	if s.next >= len(s.rows) {
		return Row{}, false, nil
	}
	r := s.rows[s.next].row
	s.next++
	return r, true, nil
}

func (s *Sort) describe() (string, []Operator) {
	parts := make([]string, len(s.keys))
	for i, k := range s.keys {
		parts[i] = k.Expr.String()
		if k.Desc {
			parts[i] += " DESC"
		}
	}
	return "Sort " + strings.Join(parts, ", "), []Operator{s.child}
}

// Limit stops after n rows. With n = 0 it never asks its child for anything.
type Limit struct {
	child Operator
	n     int64
	seen  int64
}

func (l *Limit) Next() (Row, bool, error) {
	if l.seen >= l.n {
		return Row{}, false, nil
	}
	r, ok, err := l.child.Next()
	if err != nil || !ok {
		return Row{}, false, err
	}
	l.seen++
	return r, true, nil
}

func (l *Limit) describe() (string, []Operator) {
	return "Limit " + strconv.FormatInt(l.n, 10), []Operator{l.child}
}

// Project computes the select list for each row.
type Project struct {
	child Operator
	exprs []Expr
}

func (p *Project) Next() (Row, bool, error) {
	r, ok, err := p.child.Next()
	if err != nil || !ok {
		return Row{}, false, err
	}
	out := make([]types.Value, len(p.exprs))
	for i, e := range p.exprs {
		if out[i], err = Eval(e, r.Values); err != nil {
			return Row{}, false, err
		}
	}
	return Row{Values: out}, true, nil
}

func (p *Project) describe() (string, []Operator) {
	parts := make([]string, len(p.exprs))
	for i, e := range p.exprs {
		parts[i] = e.String()
	}
	return "Project " + strings.Join(parts, ", "), []Operator{p.child}
}
