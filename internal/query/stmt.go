package query

import (
	"strconv"
	"strings"

	"github.com/wilfex81/DB_Engine/internal/types"
)

// Stmt is a bound statement. Names are in the spelling the catalog has.
type Stmt interface {
	String() string
	boundStmt()
}

// CreateTable has every NOT NULL already applied, including the one that
// PRIMARY KEY implies. PrimaryKey is the position of that column, or -1.
type CreateTable struct {
	Name       string
	Columns    []types.Column
	PrimaryKey int
}

type DropTable struct{ Name string }

type CreateIndex struct{ Name, Table, Column string }

type DropIndex struct{ Name string }

// Insert has one expression per listed column in every row, already of the
// right type. Columns that are not listed are NULL
type Insert struct {
	Table   string
	Columns []int
	Names   []string
	Rows    [][]Expr
}

type Item struct {
	Name string
	Expr Expr
}

type OrderItem struct {
	Expr Expr
	Desc bool
}

// Select reads for one table, or from none when Table is empty. The
// expressions of Items, Where and OrderBy all read the row of that table.
type Select struct {
	Table   string
	Items   []Item
	Where   Expr
	OrderBy []OrderItem
	Limit   *int64
}

type Assign struct {
	Col  int
	Name string
	Expr Expr
}

type Update struct {
	Table string
	Set   []Assign
	Where Expr
}

type Delete struct {
	Table string
	Where Expr
}

func (*CreateTable) boundStmt() {}
func (*DropTable) boundStmt()   {}
func (*CreateIndex) boundStmt() {}
func (*DropIndex) boundStmt()   {}
func (*Insert) boundStmt()      {}
func (*Select) boundStmt()      {}
func (*Update) boundStmt()      {}
func (*Delete) boundStmt()      {}

func (s *CreateTable) String() string {
	defs := make([]string, len(s.Columns))
	for i, c := range s.Columns {
		defs[i] = c.Name + " " + c.Type.String()
		if c.NotNull {
			defs[i] += " NOT NULL"
		}
		if i == s.PrimaryKey {
			defs[i] += " PRIMARY KEY"
		}
	}
	return "CREATE TABLE " + s.Name + " (" + strings.Join(defs, ", ") + ")"
}

func (s *DropTable) String() string { return "DROP TABLE " + s.Name }
func (s *DropIndex) String() string { return "DROP INDEX " + s.Name }
func (s *CreateIndex) String() string {
	return "CREATE UNIQUE INDEX " + s.Name + " ON " + s.Table + " (" + s.Column + ")"
}

func (s *Insert) String() string {
	rows := make([]string, len(s.Rows))
	for i, row := range s.Rows {
		parts := make([]string, len(row))
		for j, e := range row {
			parts[j] = e.String()
		}
		rows[i] = "(" + strings.Join(parts, ", ") + ")"
	}
	return "INSERT INTO " + s.Table + " (" + strings.Join(s.Names, ", ") + ") VALUES " + strings.Join(rows, ", ")
}

func (s *Select) String() string {
	var b strings.Builder
	b.WriteString("SELECT ")
	for i, it := range s.Items {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(it.Expr.String() + " AS " + it.Name)
	}
	if s.Table != "" {
		b.WriteString(" FROM " + s.Table)
	}
	if s.Where != nil {
		b.WriteString(" WHERE " + s.Where.String())
	}
	if len(s.OrderBy) > 0 {
		b.WriteString(" ORDER BY ")
		for i, o := range s.OrderBy {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(o.Expr.String())
			if o.Desc {
				b.WriteString(" DESC")
			}
		}
	}
	if s.Limit != nil {
		b.WriteString(" LIMIT " + strconv.FormatInt(*s.Limit, 10))
	}
	return b.String()
}

func (s *Update) String() string {
	parts := make([]string, len(s.Set))
	for i, a := range s.Set {
		parts[i] = a.Name + " = " + a.Expr.String()
	}
	out := "UPDATE " + s.Table + " SET " + strings.Join(parts, ", ")
	if s.Where != nil {
		out += " WHERE " + s.Where.String()
	}
	return out
}

func (s *Delete) String() string {
	out := "DELETE FROM " + s.Table
	if s.Where != nil {
		out += " WHERE " + s.Where.String()
	}
	return out
}
