// Package query turns parsed SQL into something that can run. This part is
// the binder: names because column positions, every expression gets a type,
// and the type rules are checked. The planner and executor join it later.
package query

import (
	"fmt"

	"github.com/wilfex81/DB_Engine/internal/sql"
	"github.com/wilfex81/DB_Engine/internal/types"
)

// BindError  is a mistake in a statement that is well formed SQL: a name that
// does not exist, or a type that does not fit.
type BindError struct {
	Pos sql.Pos
	Msg string
}

func (e *BindError) Error() string { return fmt.Sprintf("error at %s: %s", e.Pos, e.Msg) }

func errorf(pos sql.Pos, format string, args ...any) error {
	return &BindError{Pos: pos, Msg: fmt.Sprintf(format, args...)}
}

// TableInfo is what the binder needs to know about a table
type TableInfo struct {
	Name   string // the spelling the table was created with
	Schema *types.Schema
}

// Catalog is what the binder asks of the database. Both lookups ignore case.
type Catalog interface {
	Table(name string) (TableInfo, bool)
	HasIndex(name string) bool
}
