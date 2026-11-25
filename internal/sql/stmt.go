package sql

import (
	"strconv"
	"strings"
)

// Stmt is a parsed statement.
type Stmt interface {
	Start() Pos
	// String prints the statement in one line, with upper case keywords and
	// fully parenthesized expressions. Parsing the printed form gives the same
	// printed form back.
	String() string
	stmtNode()
}

type stmt struct{ At Pos }

func (s stmt) Start() Pos { return s.At }
func (stmt) stmtNode()    {}

// ColumnType is the type written in CREATE TABLE.
type ColumnType uint8

const (
	TypeInt ColumnType = iota + 1
	TypeFloat
	TypeBool
	TypeText
)

func (t ColumnType) String() string {
	switch t {
	case TypeInt:
		return "INT"
	case TypeFloat:
		return "FLOAT"
	case TypeBool:
		return "BOOL"
	case TypeText:
		return "TEXT"
	}
	return "TYPE?"
}

// ColumnDef is one column of CREATE TABLE. The flags say what was written:
// PRIMARY KEY does not turn on NotNull here, the binder does that.
type ColumnDef struct {
	At         Pos
	Name       string
	Type       ColumnType
	NotNull    bool
	PrimaryKey bool
}

type CreateTable struct {
	stmt
	Name    string
	Columns []ColumnDef
}

type DropTable struct {
	stmt
	Name string
}

// CreateIndex is CREATE UNIQUE INDEX. Every index is unique, so there is no flag.
type CreateIndex struct {
	stmt
	Name, Table, Column string
}

type DropIndex struct {
	stmt
	Name string
}

// InsertRow is one parenthesized row of VALUES.
type InsertRow struct {
	At     Pos
	Values []Expr
}

type Insert struct {
	stmt
	Table   string
	Columns []string // nil when the statement has no column list
	Rows    []InsertRow
}

// SelectItem is one output of SELECT: *, t.*, or an expression with an optional alias.
type SelectItem struct {
	At        Pos
	Star      bool
	TableStar string
	Expr      Expr
	Alias     string
}

type TableRef struct {
	At          Pos
	Name, Alias string
}

type Join struct {
	At    Pos
	Table TableRef
	On    Expr
}

type OrderItem struct {
	Expr Expr
	Desc bool
}

// Select has no FROM when it only computes values, as in SELECT 1 + 2.
type Select struct {
	stmt
	Items   []SelectItem
	From    *TableRef
	Joins   []Join
	Where   Expr
	GroupBy []Expr
	OrderBy []OrderItem
	Limit   *int64
}

type Assignment struct {
	At     Pos
	Column string
	Value  Expr
}

type Update struct {
	stmt
	Table string
	Set   []Assignment
	Where Expr
}

type Delete struct {
	stmt
	Table string
	Where Expr
}

func (d ColumnDef) String() string {
	s := d.Name + " " + d.Type.String()
	if d.NotNull {
		s += " NOT NULL"
	}
	if d.PrimaryKey {
		s += " PRIMARY KEY"
	}
	return s
}

func (s *CreateTable) String() string {
	defs := make([]string, len(s.Columns))
	for i, c := range s.Columns {
		defs[i] = c.String()
	}
	return "CREATE TABLE " + s.Name + " (" + strings.Join(defs, ", ") + ")"
}

func (s *DropTable) String() string   { return "DROP TABLE " + s.Name }
func (s *DropIndex) String() string   { return "DROP INDEX " + s.Name }
func (s *CreateIndex) String() string {
	return "CREATE UNIQUE INDEX " + s.Name + " ON " + s.Table + " (" + s.Column + ")"
}

func (s *Insert) String() string {
	var b strings.Builder
	b.WriteString("INSERT INTO " + s.Table)
	if s.Columns != nil {
		b.WriteString(" (" + strings.Join(s.Columns, ", ") + ")")
	}
	b.WriteString(" VALUES ")
	for i, row := range s.Rows {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("(" + joinExprs(row.Values) + ")")
	}
	return b.String()
}

func (it SelectItem) String() string {
	switch {
	case it.Star:
		return "*"
	case it.TableStar != "":
		return it.TableStar + ".*"
	case it.Alias != "":
		return it.Expr.String() + " AS " + it.Alias
	}
	return it.Expr.String()
}

func (r TableRef) String() string {
	if r.Alias != "" {
		return r.Name + " AS " + r.Alias
	}
	return r.Name
}

func (s *Select) String() string {
	var b strings.Builder
	b.WriteString("SELECT ")
	for i, it := range s.Items {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(it.String())
	}
	if s.From != nil {
		b.WriteString(" FROM " + s.From.String())
		for _, j := range s.Joins {
			b.WriteString(" JOIN " + j.Table.String() + " ON " + j.On.String())
		}
	}
	if s.Where != nil {
		b.WriteString(" WHERE " + s.Where.String())
	}
	if len(s.GroupBy) > 0 {
		b.WriteString(" GROUP BY " + joinExprs(s.GroupBy))
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
	var b strings.Builder
	b.WriteString("UPDATE " + s.Table + " SET ")
	for i, a := range s.Set {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(a.Column + " = " + a.Value.String())
	}
	if s.Where != nil {
		b.WriteString(" WHERE " + s.Where.String())
	}
	return b.String()
}

func (s *Delete) String() string {
	out := "DELETE FROM " + s.Table
	if s.Where != nil {
		out += " WHERE " + s.Where.String()
	}
	return out
}

func joinExprs(es []Expr) string {
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = e.String()
	}
	return strings.Join(parts, ", ")
}