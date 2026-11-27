package query

import (
	"strings"

	"github.com/wilfex81/DB_Engine/internal/table"
	"github.com/wilfex81/DB_Engine/internal/types"
)

// Row is what operators hand to each other. RID is where the row lives in its
// table, which UPDATE and DELETE will need. It is set for rows that come from
// a table scan, even after Filter and Sort, and is zero after Project.
type Row struct {
	Values []types.Value
	RID    table.RID
}

// Operator is one step of a plan. Next retursn the next row, or false when
// there are no more. No operator keeps a page pinned between calls, so a plan
// that is abandoned half way leaes nothing behind and needs no Close.
type Operator interface {
	Next() (Row, bool, error)
	// describe is one line about this step, and the steps below it.
	describe() (line string, children []Operator)
}

// Column describes one output column of a query.
type Column struct {
	Name string
	Type types.Type
}

// Drain runs an operator to the end and returns all of its rows.
func Drain(op Operator) ([][]types.Value, error) {
	var out [][]types.Value
	for {
		r, ok, err := op.Next()
		if err != nil {
			return nil, err
		}
		if !ok {
			return out, nil
		}
		out = append(out, r.Values)
	}
}

// Explain draws a plan as indented text, the topmost step first.
func Explain(op Operator) string {
	var b strings.Builder
	var walk func(o Operator, depth int)
	walk = func(o Operator, depth int) {
		line, kids := o.describe()
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(strings.Repeat(" ", depth) + line)
		for _, k := range kids {
			walk(k, depth+1)
		}
	}
	walk(op, 0)
	return b.String()
}
