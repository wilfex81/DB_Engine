package query

import (
	"fmt"

	"github.com/wilfex81/DB_Engine/internal/catalog"
	"github.com/wilfex81/DB_Engine/internal/table"
)

// CheckTable verifies that every index of a table agrees with its rows:
// every row with a non-NULL key is the index, at the right place , and the
// index hodls nothing else. It reads the whole table, so it is meant for
// tests and for a repair tool, not for every statement.
func CheckTable(t *catalog.Table) error {
	for _, ix := range t.Indexes() {
		want := map[int64]table.RID{}
		rows := t.Heap.Scan()
		for rows.Next() {
			v := rows.Values()[ix.ColumnPos]
			if v.IsNull() {
				continue
			}
			if prev, dup := want[v.Int()]; dup {
				return fmt.Errorf("query: table %q: rows %s and %s both have %s = %d so the index %q cannot be right",
					t.Name, prev, rows.RID(), ix.Column, v.Int(), ix.Name)
			}
			want[v.Int()] = rows.RID()
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if _, err := ix.Tree.Verify(); err != nil {
			return fmt.Errorf("query: index %q: %w", ix.Name, err)
		}

		seen := 0
		entries := ix.Tree.Scan()
		for entries.Next() {
			rid, ok := want[entries.Key()]
			switch {
			case !ok:
				return fmt.Errorf("query: index %q has the key %d but no row does", ix.Name, entries.Key())
			case rid != entries.RID():
				return fmt.Errorf("query: index %q maps key %d to %s but the row is at %s", ix.Name, entries.Key(), entries.RID(), rid)
			}
			seen++
		}
		if err := entries.Err(); err != nil {
			return err
		}
		if seen != len(want) {
			return fmt.Errorf("query: index %q is missing %d of the keys of table %q", ix.Name, len(want)-seen, t.Name)
		}
	}
	return nil
}
