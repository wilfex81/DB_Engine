package query

import (
	"fmt"
	"math"

	"github.com/wilfex81/DB_Engine/internal/catalog"
	"github.com/wilfex81/DB_Engine/internal/sql"
	"github.com/wilfex81/DB_Engine/internal/types"
)

// NewCatalog lets the binder look at the real catalog.
func NewCatalog(c *catalog.Catalog) Catalog { return catalogView{c} }

type catalogView struct{ c *catalog.Catalog }

func (v catalogView) Table(name string) (TableInfo, bool) {
	t, ok := v.c.Table(name)
	if !ok {
		return TableInfo{}, false
	}
	return TableInfo{Name: t.Name, Schema: t.Schema}, true
}

func (v catalogView) HasIndex(name string) bool {
	_, ok := v.c.Index(name)
	return ok
}

// Plan is a SELECT ready to run: read rows from Root until it says there are no more.
type Plan struct {
	Root    Operator
	Columns []Column
}

// Rows runs the plan to the end.
func (p *Plan) Rows() ([][]types.Value, error) { return Drain(p.Root) }

// PlanOption changes how a plan is made.
type PlanOption func(*planner)

// WithoutIndexes makes every plan read whole tables. The results must not
// change, so tests use it to check the plans that do use indexes.
func WithoutIndexes() PlanOption { return func(p *planner) { p.useIndexes = false } }

type planner struct{ useIndexes bool }

// PlanSelect turns a bound SELECT into operators. The rules are fixed and
// need no statistics:
//
//   - Split WHERE at the top level ANDs. A part of the form column OP number,
//     on an indexed INT column, with OP one of = < <= > >=, can be answered by
//     the index. Several such parts on one column become one range.
//   - A lookup of one key beats a range, and a range beats a full scan. Among
//     equals, the index that comes first by name wins.
//   - Parts the index answers are dropped. The rest stay in a Filter.
//   - An index scan already returns rows in key order, so an ORDER BY on that
//     column, ascending, needs no Sort.
//   - With a LIMIT, ORDER BY on an indexed NOT NULL column can use a full
//     index scan with no Sort, which stops after LIMIT rows.
func PlanSelect(cat *catalog.Catalog, sel *Select, opts ...PlanOption) (*Plan, error) {
	p := &planner{useIndexes: true}
	for _, o := range opts {
		o(p)
	}

	conjuncts := splitAnd(sel.Where)
	var root Operator
	ordered := -1 // the column the source returns rows sorted by, or -1
	if sel.Table == "" {
		root = &Dual{}
	} else {
		t, ok := cat.Table(sel.Table)
		if !ok {
			return nil, fmt.Errorf("query: table %q does not exist", sel.Table)
		}
		root, conjuncts, ordered = p.access(t, conjuncts, sel)
	}

	if cond := andAll(conjuncts); cond != nil {
		root = &Filter{child: root, cond: cond}
	}
	if len(sel.OrderBy) > 0 && !sortedBy(sel.OrderBy, ordered) {
		root = &Sort{child: root, keys: sel.OrderBy}
	}
	if sel.Limit != nil {
		root = &Limit{child: root, n: *sel.Limit}
	}

	exprs := make([]Expr, len(sel.Items))
	cols := make([]Column, len(sel.Items))
	for i, it := range sel.Items {
		exprs[i] = it.Expr
		cols[i] = Column{Name: it.Name, Type: it.Expr.Type()}
	}
	return &Plan{Root: &Project{child: root, exprs: exprs}, Columns: cols}, nil
}

// sortedBy reports whether rows that come sorted ascending by column pos
// already satisfy the ORDER BY.
func sortedBy(order []OrderItem, pos int) bool {
	if pos < 0 || len(order) != 1 || order[0].Desc {
		return false
	}
	ref, ok := order[0].Expr.(*ColRef)
	return ok && ref.Pos == pos
}

// access chooses how to read the table. It returns the source, the parts of
// WHERE that are still left to check, and the column the source is sorted by.
func (p *planner) access(t *catalog.Table, conjuncts []Expr, sel *Select) (Operator, []Expr, int) {
	if !p.useIndexes {
		return &SeqScan{tableName: t.Name, it: t.Heap.Scan()}, conjuncts, -1
	}

	var best *candidate
	for _, ix := range t.Indexes() { // sorted by name
		if c := newCandidate(ix, conjuncts); c != nil && (best == nil || c.score() > best.score()) {
			best = c
		}
	}
	if best != nil {
		residual := make([]Expr, 0, len(conjuncts))
		for i, e := range conjuncts {
			if !best.used[i] {
				residual = append(residual, e)
			}
		}
		return newIndexScan(t, best.ix, best.lo, best.hi), residual, best.ix.ColumnPos
	}

	// No part of WHERE helps. An ordered scan can still replace Sort and Limit,
	// but only on a NOT NULL column: rows with a NULL key are not in the index,
	// and a full scan of it would lose them.
	if sel.Limit != nil && len(sel.OrderBy) == 1 && !sel.OrderBy[0].Desc {
		if ref, ok := sel.OrderBy[0].Expr.(*ColRef); ok && t.Schema.Column(ref.Pos).NotNull {
			for _, ix := range t.Indexes() {
				if ix.ColumnPos == ref.Pos {
					return newIndexScan(t, ix, math.MinInt64, math.MaxInt64), conjuncts, ref.Pos
				}
			}
		}
	}
	return &SeqScan{tableName: t.Name, it: t.Heap.Scan()}, conjuncts, -1
}

func newIndexScan(t *catalog.Table, ix *catalog.Index, lo, hi int64) *IndexScan {
	return &IndexScan{tableName: t.Name, ix: ix, heap: t.Heap, lo: lo, hi: hi, it: ix.Tree.Range(lo, hi)}
}

// candidate is one index and the key range that the parts of WHERE give it.
type candidate struct {
	ix     *catalog.Index
	lo, hi int64
	point  bool         // there is an equality, so at most one key
	used   map[int]bool // which parts of WHERE the index answers
}

func (c *candidate) score() int {
	if c.point {
		return 2
	}
	return 1
}

func newCandidate(ix *catalog.Index, conjuncts []Expr) *candidate {
	c := &candidate{ix: ix, lo: math.MinInt64, hi: math.MaxInt64, used: map[int]bool{}}
	empty := false
	for i, e := range conjuncts {
		b, ok := indexable(e, ix.ColumnPos)
		if !ok {
			continue
		}
		c.used[i] = true
		switch b.op {
		case sql.OpEq:
			c.point = true
			c.lo, c.hi = max(c.lo, b.k), min(c.hi, b.k)
		case sql.OpLt:
			if b.k == math.MinInt64 {
				empty = true // nothing is smaller
			} else {
				c.hi = min(c.hi, b.k-1)
			}
		case sql.OpLtEq:
			c.hi = min(c.hi, b.k)
		case sql.OpGt:
			if b.k == math.MaxInt64 {
				empty = true // nothing is larger
			} else {
				c.lo = max(c.lo, b.k+1)
			}
		case sql.OpGtEq:
			c.lo = max(c.lo, b.k)
		}
	}
	if len(c.used) == 0 {
		return nil
	}
	if empty {
		c.lo, c.hi = 1, 0
	}
	return c
}

// bound is "column op k".
type bound struct {
	op sql.BinaryOp
	k  int64
}

// indexable recognizes column OP number and number OP column, for the INT
// column at position pos. Anything else, such as a comparison through a CAST
// or with NULL, is left to the Filter.
func indexable(e Expr, pos int) (bound, bool) {
	b, ok := e.(*Binary)
	if !ok {
		return bound{}, false
	}
	switch b.Op {
	case sql.OpEq, sql.OpLt, sql.OpLtEq, sql.OpGt, sql.OpGtEq:
	default:
		return bound{}, false
	}
	if col, ok := b.L.(*ColRef); ok && col.Pos == pos && col.Typ == types.Int {
		if k, ok := intConst(b.R); ok {
			return bound{b.Op, k}, true
		}
	}
	if col, ok := b.R.(*ColRef); ok && col.Pos == pos && col.Typ == types.Int {
		if k, ok := intConst(b.L); ok {
			return bound{flip(b.Op), k}, true
		}
	}
	return bound{}, false
}

func intConst(e Expr) (int64, bool) {
	c, ok := e.(*Const)
	if !ok || c.untyped || c.Value.IsNull() || c.Value.Type() != types.Int {
		return 0, false
	}
	return c.Value.Int(), true
}

// flip turns "k op column" into "column op' k".
func flip(op sql.BinaryOp) sql.BinaryOp {
	switch op {
	case sql.OpLt:
		return sql.OpGt
	case sql.OpLtEq:
		return sql.OpGtEq
	case sql.OpGt:
		return sql.OpLt
	case sql.OpGtEq:
		return sql.OpLtEq
	}
	return op
}

// splitAnd lists the parts of a condition that are joined by AND at the top.
func splitAnd(e Expr) []Expr {
	if e == nil {
		return nil
	}
	if b, ok := e.(*Binary); ok && b.Op == sql.OpAnd {
		return append(splitAnd(b.L), splitAnd(b.R)...)
	}
	return []Expr{e}
}

// andAll joins conditions with AND again. With none, it returns nil.
func andAll(es []Expr) Expr {
	if len(es) == 0 {
		return nil
	}
	out := es[0]
	for _, e := range es[1:] {
		out = &Binary{Op: sql.OpAnd, L: out, R: e, Typ: types.Bool}
	}
	return out
}