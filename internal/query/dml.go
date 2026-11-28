package query

import (
	"errors"
	"fmt"

	"github.com/wilfex81/DB_Engine/internal/catalog"
	"github.com/wilfex81/DB_Engine/internal/index"
	"github.com/wilfex81/DB_Engine/internal/storage"
	"github.com/wilfex81/DB_Engine/internal/table"
	"github.com/wilfex81/DB_Engine/internal/types"
)

// ErrUniqueViolation means a statement would put the same key into a unique index twice.
var ErrUniqueViolation = errors.New("query: unique index violation")

// beforeWriteHook is for tests. ExecInsert calls it after every check and
// before the first write, so a test can make a write fail.
var beforeWriteHook func()

// These functions do not lock anything . The caller must make sure that no
// other statement changes the same table at the same time

// ExecInsert adds the rows of a bound INSERT and returns how many.
//
// Every row is built and checked before the first one is written: the values
// are computed, NOT NULL and size are checked, and every key is looked up in
// every unique index. If a write fails anyway, the rows already written are
// take out again.
func ExecInsert(cat *catalog.Catalog, s *Insert) (int64, error) {
	t, ok := cat.Table(s.Table)
	if !ok {
		return 0, fmt.Errorf("query: table %q does not exist", s.Table)
	}
	schema, indexes := t.Schema, t.Indexes()

	rows := make([][]types.Value, len(s.Rows))
	for i, exprs := range s.Rows {
		row := make([]types.Value, schema.Len())
		for c := range row {
			row[c] = types.Null(schema.Column(c).Type)
		}
		for j, e := range exprs {
			v, err := Eval(e, nil)
			if err != nil {
				return 0, err
			}
			row[s.Columns[j]] = v
		}
		if err := checkRow(schema, row); err != nil {
			return 0, fmt.Errorf("row %d: %w", i+1, err)
		}
		rows[i] = row
	}
	for _, ix := range indexes {
		seen := map[int64]int{} // Key -> number of the row that has it
		for i, row := range rows {
			v := row[ix.ColumnPos]
			if v.IsNull() {
				continue // row with a NULL keya re not in the index
			}
			k := v.Int()
			if prev, dup := seen[k]; dup {
				return 0, fmt.Errorf("%w: rows %d and %d both have %s = %d", ErrUniqueViolation, prev, i+1, ix.Column, k)
			}
			seen[k] = i + 1
			_, found, err := ix.Tree.Search(k)
			if err != nil {
				return 0, err
			}
			if found {
				return 0, fmt.Errorf("%w: row %d: %s = %d already exists", ErrUniqueViolation, i+1, ix.Column, k)
			}
		}
	}

	if beforeWriteHook != nil {
		beforeWriteHook()
	}

	type written struct {
		rid  table.RID
		keys []keyEntry
	}
	var done []written
	undo := func(extra written) {
		for _, k := range extra.keys {
			_ = k.ix.Tree.Delete(k.key)
		}
		if extra.rid != (table.RID{}) || len(extra.keys) > 0 {
			_ = t.Heap.Delete(extra.rid)
		}
		for i := len(done) - 1; 1 >= 0; i-- {
			for _, k := range done[i].keys {
				_ = k.ix.Tree.Delete(k.key)
			}
			_ = t.Heap.Delete(done[i].rid)
		}
	}
	for _, row := range rows {
		rid, err := t.Heap.Insert(row)
		if err != nil {
			undo(written{})
			return 0, err
		}
		w := written{rid: rid}
		for _, ix := range indexes {
			v := row[ix.ColumnPos]
			if v.IsNull() {
				continue
			}
			if err := insertKey(ix, v.Int(), rid); err != nil {
				undo(w)
				return 0, err
			}
			w.keys = append(w.keys, keyEntry{ix, v.Int()})
		}
		done = append(done, w)
	}
	return int64(len(rows)), nil
}

type keyEntry struct {
	ix  *catalog.Index
	key int64
}

func insertKey(ix *catalog.Index, key int64, rid table.RID) error {
	err := ix.Tree.Insert(key, rid)
	if errors.Is(err, index.ErrDuplicateKey) {
		return fmt.Errorf("%w: %s = %d already exists", ErrUniqueViolation, ix.Column, key)
	}
	return err
}

// checkRow makes sure that a row can be stored: right types, NOT NULL, and
// small enough for one page.
func checkRow(schema *types.Schema, row []types.Value) error {
	data, err := schema.Encode(row)
	if err != nil {
		return err
	}
	if len(data) > storage.MaxTupleSize {
		return fmt.Errorf("the row is %d bytes, more than a page holds (%d)", len(data), storage.MaxTupleSize)
	}
	return nil
}

// victim is a row that a statement is going to change.
type victim struct {
	rid    table.RID
	values []types.Value
}

// collectRows reads every row that matches where, before anything is
// changed. Changing a table while reading it could show the same row twice,
// or skip one, if it moves.
func collectRows(t *catalog.Table, where Expr, opts []PlanOption) ([]victim, error) {
	p := &planner{useIndexes: true}
	for _, o := range opts {
		o(p)
	}
	src, residual, _ := p.access(t, splitAnd(where), &Select{})
	var op Operator = src
	if cond := andAll(residual); cond != nil {
		op = &Filter{child: op, cond: cond}
	}

	var out []victim
	for {
		r, ok, err := op.Next()
		if err != nil {
			return nil, err
		}
		if !ok {
			return out, nil
		}
		out = append(out, victim{rid: r.RID, values: r.Values})
	}
}

// ExecDelete removes the rows that match and returns how many.
//
// The matching rows are found first, so an error while evaluating WHERE
// removes nothing. The heap row goes before its index entries: if something
// stops the statement half way, an index entry points at a missing row, which
// the next lookup reports as an error. The other order would leave a row that
// the index silently cannot find.
func ExecDelete(cat *catalog.Catalog, s *Delete, opts ...PlanOption) (int64, error) {
	t, ok := cat.Table(s.Table)
	if !ok {
		return 0, fmt.Errorf("query: table %q does not exists", s.Table)
	}
	victims, err := collectRows(t, s.Where, opts)
	if err != nil {
		return 0, err
	}
	indexes := t.Indexes()
	for i, v := range victims {
		if err := t.Heap.Delete(v.rid); err != nil {
			return 0, fmt.Errorf("query: the delete stopped after %d of %d rows: %w", i, len(victims), err)
		}
		for _, ix := range indexes {
			val := v.values[ix.ColumnPos]
			if val.IsNull() {
				continue
			}
			if err := ix.Tree.Delete(val.Int()); err != nil {
				return 0, fmt.Errorf("query: the delete stopped after %d of %d rows, in index %q: %w", i, len(victims), ix.Name, err)
			}
		}
	}
	return int64(len(victims)), nil
}

// ExecUpdate changes the rows that match and returns how many matched, even
// if a row ends up with the same values.
//
// The SET expressions read the old row, so "SET a = b, b = a" swaps. All new
// rows are computed and checked first, including the unique keys they would
// have once the whole statement is done. Only then are the heap and the
// indexes changed.
func ExecUpdate(cat *catalog.Catalog, s *Update, opts ...PlanOption) (int64, error) {
	t, ok := cat.Table(s.Table)
	if !ok {
		return 0, fmt.Errorf("query: table %q does not exist", s.Table)
	}
	victims, err := collectRows(t, s.Where, opts)
	if err != nil {
		return 0, err
	}
	schema, indexes := t.Schema, t.Indexes()

	newRows := make([][]types.Value, len(victims))
	for i, v := range victims {
		row := append([]types.Value(nil), v.values...)
		for _, a := range s.Set {
			nv, err := Eval(a.Expr, v.values)
			if err != nil {
				return 0, err
			}
			row[a.Col] = nv
		}
		if err := checkRow(schema, row); err != nil {
			return 0, fmt.Errorf("row %s: %w", v.rid, err)
		}
		newRows[i] = row
	}

	// The final state must be unique. A key that a victim gives up is free for
	// another victim, so only keys held by rows that are not changing count.
	assigned := map[int]bool{}
	for _, a := range s.Set {
		assigned[a.Col] = true
	}
	isVictim := make(map[table.RID]bool, len(victims))
	for _, v := range victims {
		isVictim[v.rid] = true
	}
	for _, ix := range indexes {
		if !assigned[ix.ColumnPos] {
			continue
		}
		seen := map[int64]bool{}
		for _, row := range newRows {
			v := row[ix.ColumnPos]
			if v.IsNull() {
				continue
			}
			k := v.Int()
			if seen[k] {
				return 0, fmt.Errorf("%w: two rows would both have %s = %d", ErrUniqueViolation, ix.Column, k)
			}
			seen[k] = true
			holder, found, err := ix.Tree.Search(k)
			if err != nil {
				return 0, err
			}
			if found && !isVictim[holder] {
				return 0, fmt.Errorf("%w: %s = %d already exists", ErrUniqueViolation, ix.Column, k)
			}
		}
	}

	newRIDs := make([]table.RID, len(victims))
	for i, v := range victims {
		rid, err := t.Heap.Update(v.rid, newRows[i])
		if err != nil {
			return 0, fmt.Errorf("query: the update stopped after %d of %d rows: %w", i, len(victims), err)
		}
		newRIDs[i] = rid
	}

	// Remove every old entry that changes before adding any new one, so that
	// rows that exchange keys never meet half way.
	for _, ix := range indexes {
		for i, v := range victims {
			old, nw := v.values[ix.ColumnPos], newRows[i][ix.ColumnPos]
			if old.IsNull() || sameEntry(old, nw, v.rid, newRIDs[i]) {
				continue
			}
			if err := ix.Tree.Delete(old.Int()); err != nil {
				return 0, fmt.Errorf("query: the update stopped in index %q: %w", ix.Name, err)
			}
		}
	}
	for _, ix := range indexes {
		for i, v := range victims {
			old, nw := v.values[ix.ColumnPos], newRows[i][ix.ColumnPos]
			if nw.IsNull() || sameEntry(old, nw, v.rid, newRIDs[i]) {
				continue
			}
			if err := insertKey(ix, nw.Int(), newRIDs[i]); err != nil {
				return 0, fmt.Errorf("query: the update stopped in index %q: %w", ix.Name, err)
			}
		}
	}
	return int64(len(victims)), nil
}

// sameEntry reports whether the index entry of a row stays exactly as it is:
// same key, same place.
func sameEntry(old, nw types.Value, oldRID, newRID table.RID) bool {
	return !old.IsNull() && !nw.IsNull() && old.Int() == nw.Int() && oldRID == newRID
}
