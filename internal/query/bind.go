package query

import (
	"strings"

	"github.com/wilfex81/DB_Engine/internal/sql"
	"github.com/wilfex81/DB_Engine/internal/types"
)

const maxNameLen = 64 // what the catalog accepts

type binder struct{ cat Catalog }

// Bind checks a parsed statement against the catalog and resolves it.
func Bind(cat Catalog, s sql.Stmt) (Stmt, error) {
	b := &binder{cat: cat}
	switch s := s.(type) {
	case *sql.CreateTable:
		return b.createTable(s)
	case *sql.DropTable:
		return b.dropTable(s)
	case *sql.CreateIndex:
		return b.createIndex(s)
	case *sql.DropIndex:
		return b.dropIndex(s)
	case *sql.Insert:
		return b.insert(s)
	case *sql.Select:
		return b.selectStmt(s)
	case *sql.Update:
		return b.update(s)
	case *sql.Delete:
		return b.delete(s)
	}
	return nil, errorf(s.Start(), "this kind of statement cannot be run")
}

func (b *binder) table(name string, at sql.Pos) (TableInfo, error) {
	info, ok := b.cat.Table(name)
	if !ok {
		return TableInfo{}, errorf(at, "table %q does not exist", name)
	}
	return info, nil
}

func checkName(at sql.Pos, what, name string) error {
	if len(name) > maxNameLen {
		return errorf(at, "the %s name %q is longer than %d characters", what, name, maxNameLen)
	}
	return nil
}

// ---- DDL

func (b *binder) createTable(s *sql.CreateTable) (Stmt, error) {
	if err := checkName(s.NameAt, "table", s.Name); err != nil {
		return nil, err
	}
	if _, ok := b.cat.Table(s.Name); ok {
		return nil, errorf(s.NameAt, "table %q already exists", s.Name)
	}

	out := &CreateTable{Name: s.Name, Columns: make([]types.Column, len(s.Columns)), PrimaryKey: -1}
	seen := map[string]bool{}
	for i, c := range s.Columns {
		if err := checkName(c.At, "column", c.Name); err != nil {
			return nil, err
		}
		key := strings.ToLower(c.Name)
		if seen[key] {
			return nil, errorf(c.At, "column %q is defined twice", c.Name)
		}
		seen[key] = true

		col := types.Column{Name: c.Name, Type: columnType(c.Type), NotNull: c.NotNull}
		if c.PrimaryKey {
			if out.PrimaryKey >= 0 {
				return nil, errorf(c.At, "a table can have only one PRIMARY KEY")
			}
			if col.Type != types.Int {
				return nil, errorf(c.At, "PRIMARY KEY must be an INT column, %q is %s", c.Name, col.Type)
			}
			out.PrimaryKey = i
			col.NotNull = true
		}
		out.Columns[i] = col
	}
	return out, nil
}

func columnType(t sql.ColumnType) types.Type {
	switch t {
	case sql.TypeInt:
		return types.Int
	case sql.TypeFloat:
		return types.Float
	case sql.TypeBool:
		return types.Bool
	}
	return types.Text
}

func (b *binder) dropTable(s *sql.DropTable) (Stmt, error) {
	info, err := b.table(s.Name, s.NameAt)
	if err != nil {
		return nil, err
	}
	return &DropTable{Name: info.Name}, nil
}

func (b *binder) createIndex(s *sql.CreateIndex) (Stmt, error) {
	if err := checkName(s.NameAt, "index", s.Name); err != nil {
		return nil, err
	}
	info, err := b.table(s.Table, s.TableAt)
	if err != nil {
		return nil, err
	}
	if b.cat.HasIndex(s.Name) {
		return nil, errorf(s.NameAt, "index %q already exists", s.Name)
	}
	pos, ok := info.Schema.Index(s.Column)
	if !ok {
		return nil, errorf(s.ColumnAt, "column %q does not exist in table %q", s.Column, info.Name)
	}
	col := info.Schema.Column(pos)
	if col.Type != types.Int {
		return nil, errorf(s.ColumnAt, "only INT columns can be indexed, %q is %s", col.Name, col.Type)
	}
	return &CreateIndex{Name: s.Name, Table: info.Name, Column: col.Name}, nil
}

func (b *binder) dropIndex(s *sql.DropIndex) (Stmt, error) {
	if !b.cat.HasIndex(s.Name) {
		return nil, errorf(s.NameAt, "index %q does not exist", s.Name)
	}
	return &DropIndex{Name: s.Name}, nil
}

// ---- INSERT, UPDATE, DELETE

func (b *binder) insert(s *sql.Insert) (Stmt, error) {
	info, err := b.table(s.Table, s.TableAt)
	if err != nil {
		return nil, err
	}
	schema := info.Schema

	var targets []int
	if s.Columns == nil {
		for i := 0; i < schema.Len(); i++ {
			targets = append(targets, i)
		}
	} else {
		listed := map[int]bool{}
		for i, name := range s.Columns {
			pos, ok := schema.Index(name)
			if !ok {
				return nil, errorf(s.ColumnAts[i], "column %q does not exist in table %q", name, info.Name)
			}
			if listed[pos] {
				return nil, errorf(s.ColumnAts[i], "column %q is listed twice", name)
			}
			listed[pos] = true
			targets = append(targets, pos)
		}
		for i := 0; i < schema.Len(); i++ {
			if c := schema.Column(i); !listed[i] && c.NotNull {
				return nil, errorf(s.TableAt, "column %q is NOT NULL and needs a value", c.Name)
			}
		}
	}

	out := &Insert{Table: info.Name, Columns: targets}
	for _, pos := range targets {
		out.Names = append(out.Names, schema.Column(pos).Name)
	}

	sc := &scope{why: "VALUES cannot refer to columns"}
	for _, row := range s.Rows {
		if len(row.Values) != len(targets) {
			if s.Columns == nil {
				return nil, errorf(row.At, "the row has %d values but the table has %d columns", len(row.Values), len(targets))
			}
			return nil, errorf(row.At, "the row has %d values but %d columns were listed", len(row.Values), len(targets))
		}
		bound := make([]Expr, len(row.Values))
		for j, e := range row.Values {
			x, err := b.expr(e, sc)
			if err != nil {
				return nil, err
			}
			if bound[j], err = assign(x, e.Start(), schema.Column(targets[j])); err != nil {
				return nil, err
			}
		}
		out.Rows = append(out.Rows, bound)
	}
	return out, nil
}

// assign checks that a value can be stored in a column. Only two conversions
// exist: an untyped NULL takes the column's type, and an INT may go into a
// FLOAT column. Nothing is ever rounded, and text never turns into a number.
func assign(e Expr, at sql.Pos, col types.Column) (Expr, error) {
	if isUntypedNull(e) {
		if col.NotNull {
			return nil, errorf(at, "column %q cannot be NULL", col.Name)
		}
		return typedNull(col.Type), nil
	}
	switch {
	case e.Type() == col.Type:
		return e, nil
	case e.Type() == types.Int && col.Type == types.Float:
		return toFloat(e), nil
	}
	return nil, errorf(at, "cannot store %s in %s column %q", e.Type(), col.Type, col.Name)
}

func (b *binder) update(s *sql.Update) (Stmt, error) {
	info, err := b.table(s.Table, s.TableAt)
	if err != nil {
		return nil, err
	}
	sc := &scope{name: info.Name, schema: info.Schema}
	out := &Update{Table: info.Name}

	assigned := map[int]bool{}
	for _, a := range s.Set {
		pos, ok := info.Schema.Index(a.Column)
		if !ok {
			return nil, errorf(a.At, "column %q does not exist in table %q", a.Column, info.Name)
		}
		if assigned[pos] {
			return nil, errorf(a.At, "column %q is assigned twice", a.Column)
		}
		assigned[pos] = true

		x, err := b.expr(a.Value, sc)
		if err != nil {
			return nil, err
		}
		col := info.Schema.Column(pos)
		if x, err = assign(x, a.Value.Start(), col); err != nil {
			return nil, err
		}
		out.Set = append(out.Set, Assign{Col: pos, Name: col.Name, Expr: x})
	}
	if out.Where, err = b.cond(s.Where, sc, "WHERE"); err != nil {
		return nil, err
	}
	return out, nil
}

func (b *binder) delete(s *sql.Delete) (Stmt, error) {
	info, err := b.table(s.Table, s.TableAt)
	if err != nil {
		return nil, err
	}
	sc := &scope{name: info.Name, schema: info.Schema}
	out := &Delete{Table: info.Name}
	if out.Where, err = b.cond(s.Where, sc, "WHERE"); err != nil {
		return nil, err
	}
	return out, nil
}

// ---- SELECT

func (b *binder) selectStmt(s *sql.Select) (Stmt, error) {
	if len(s.Joins) > 0 {
		return nil, errorf(s.Joins[0].At, "JOIN is not supported yet")
	}
	if len(s.GroupBy) > 0 {
		return nil, errorf(s.GroupBy[0].Start(), "GROUP BY is not supported yet")
	}

	sc := &scope{why: "the query has no FROM clause"}
	out := &Select{Limit: s.Limit}
	if s.From != nil {
		info, err := b.table(s.From.Name, s.From.At)
		if err != nil {
			return nil, err
		}
		sc = &scope{name: info.Name, schema: info.Schema}
		if s.From.Alias != "" {
			sc.name = s.From.Alias // the alias hides the real name
		}
		out.Table = info.Name
	}

	for _, it := range s.Items {
		switch {
		case it.Star:
			if sc.schema == nil {
				return nil, errorf(it.At, "SELECT * needs a FROM clause")
			}
			out.Items = append(out.Items, sc.allColumns()...)
		case it.TableStar != "":
			if sc.schema == nil || !strings.EqualFold(it.TableStar, sc.name) {
				return nil, errorf(it.At, "missing FROM-clause entry for table %q", it.TableStar)
			}
			out.Items = append(out.Items, sc.allColumns()...)
		default:
			x, err := b.expr(it.Expr, sc)
			if err != nil {
				return nil, err
			}
			x = defaultNull(x)
			name := it.Alias
			if name == "" {
				name = "?column?"
				if ref, ok := x.(*ColRef); ok {
					name = ref.Name
				}
			}
			out.Items = append(out.Items, Item{Name: name, Expr: x})
		}
	}

	var err error
	if out.Where, err = b.cond(s.Where, sc, "WHERE"); err != nil {
		return nil, err
	}
	for _, o := range s.OrderBy {
		x, err := b.orderExpr(o.Expr, sc, out.Items)
		if err != nil {
			return nil, err
		}
		out.OrderBy = append(out.OrderBy, OrderItem{Expr: x, Desc: o.Desc})
	}
	return out, nil
}

// orderExpr binds one ORDER BY item. A whole number is a position in the
// select list, and a bare name that is the name of an output column means
// that column, as in PostgreSQL. Everything else is an ordinary expression.
func (b *binder) orderExpr(e sql.Expr, sc *scope, items []Item) (Expr, error) {
	switch e := e.(type) {
	case *sql.IntLiteral:
		if e.Value < 1 || e.Value > int64(len(items)) {
			return nil, errorf(e.Start(), "ORDER BY position %d is not in the select list", e.Value)
		}
		return items[e.Value-1].Expr, nil
	case *sql.ColumnRef:
		if e.Table == "" {
			var found []Item
			for _, it := range items {
				if it.Name != "?column?" && strings.EqualFold(it.Name, e.Name) {
					found = append(found, it)
				}
			}
			if len(found) > 0 {
				for _, it := range found[1:] {
					if it.Expr.String() != found[0].Expr.String() {
						return nil, errorf(e.Start(), "ORDER BY %q is ambiguous", e.Name)
					}
				}
				return found[0].Expr, nil
			}
		}
	}
	x, err := b.expr(e, sc)
	if err != nil {
		return nil, err
	}
	return defaultNull(x), nil
}

// cond binds a WHERE condition: it must be a BOOL, or NULL.
func (b *binder) cond(e sql.Expr, sc *scope, clause string) (Expr, error) {
	if e == nil {
		return nil, nil
	}
	x, err := b.expr(e, sc)
	if err != nil {
		return nil, err
	}
	if isUntypedNull(x) {
		return typedNull(types.Bool), nil
	}
	if x.Type() != types.Bool {
		return nil, errorf(e.Start(), "%s must be a BOOL expression, found %s", clause, x.Type())
	}
	return x, nil
}

// ---- scope

// scope is what a column name can refer to. It has one table, or none.
type scope struct {
	name   string        // how the table is called in this query: its alias, or its name
	schema *types.Schema // nil when there is no table
	why    string        // when there is no table, why columns are not allowed
}

func (sc *scope) allColumns() []Item {
	items := make([]Item, sc.schema.Len())
	for i := range items {
		c := sc.schema.Column(i)
		items[i] = Item{Name: c.Name, Expr: &ColRef{Pos: i, Name: c.Name, Typ: c.Type}}
	}
	return items
}

func (sc *scope) resolve(ref *sql.ColumnRef) (Expr, error) {
	if sc.schema == nil {
		return nil, errorf(ref.Start(), "column %q does not exist (%s)", ref.Name, sc.why)
	}
	if ref.Table != "" && !strings.EqualFold(ref.Table, sc.name) {
		return nil, errorf(ref.Start(), "missing FROM-clause entry for table %q", ref.Table)
	}
	i, ok := sc.schema.Index(ref.Name)
	if !ok {
		return nil, errorf(ref.Start(), "column %q does not exist", ref.Name)
	}
	c := sc.schema.Column(i)
	return &ColRef{Pos: i, Name: c.Name, Typ: c.Type}, nil
}

// ---- expressions

func (b *binder) expr(e sql.Expr, sc *scope) (Expr, error) {
	switch e := e.(type) {
	case *sql.IntLiteral:
		return &Const{Value: types.NewInt(e.Value)}, nil
	case *sql.FloatLiteral:
		return &Const{Value: types.NewFloat(e.Value)}, nil
	case *sql.StringLiteral:
		if len(e.Value) > types.MaxTextLen {
			return nil, errorf(e.Start(), "the string is longer than %d bytes", types.MaxTextLen)
		}
		return &Const{Value: types.NewText(e.Value)}, nil
	case *sql.BoolLiteral:
		return &Const{Value: types.NewBool(e.Value)}, nil
	case *sql.NullLiteral:
		return &Const{untyped: true}, nil
	case *sql.ColumnRef:
		return sc.resolve(e)
	case *sql.UnaryExpr:
		return b.unary(e, sc)
	case *sql.BinaryExpr:
		return b.binary(e, sc)
	case *sql.IsNullExpr:
		x, err := b.expr(e.X, sc)
		if err != nil {
			return nil, err
		}
		if isUntypedNull(x) {
			x = typedNull(types.Text)
		}
		return &IsNull{X: x, Negated: e.Negated}, nil
	case *sql.FuncCall:
		return nil, errorf(e.Start(), "functions are not supported yet")
	}
	return nil, errorf(e.Start(), "this kind of expression cannot be run")
}

func (b *binder) unary(e *sql.UnaryExpr, sc *scope) (Expr, error) {
	x, err := b.expr(e.X, sc)
	if err != nil {
		return nil, err
	}
	if e.Op == sql.OpNot {
		if x, err = boolArg(x, "NOT", e.X.Start()); err != nil {
			return nil, err
		}
		return &Unary{At: e.Start(), Op: sql.OpNot, X: x}, nil
	}
	if isUntypedNull(x) {
		x = typedNull(types.Int)
	}
	if !numeric(x.Type()) {
		return nil, errorf(e.Start(), "operator - is not defined for %s", x.Type())
	}
	return &Unary{At: e.Start(), Op: sql.OpNeg, X: x}, nil
}

func (b *binder) binary(e *sql.BinaryExpr, sc *scope) (Expr, error) {
	l, err := b.expr(e.L, sc)
	if err != nil {
		return nil, err
	}
	r, err := b.expr(e.R, sc)
	if err != nil {
		return nil, err
	}

	if e.Op == sql.OpAnd || e.Op == sql.OpOr {
		if l, err = boolArg(l, e.Op.String(), e.L.Start()); err != nil {
			return nil, err
		}
		if r, err = boolArg(r, e.Op.String(), e.R.Start()); err != nil {
			return nil, err
		}
		return &Binary{At: e.Start(), Op: e.Op, L: l, R: r, Typ: types.Bool}, nil
	}

	l, r = adaptNulls(l, r)
	lt, rt := l.Type(), r.Type()

	switch e.Op {
	case sql.OpEq, sql.OpNotEq, sql.OpLt, sql.OpLtEq, sql.OpGt, sql.OpGtEq:
		switch {
		case lt == rt:
		case numeric(lt) && numeric(rt):
			l, r = promote(l, r)
		default:
			return nil, errorf(e.Start(), "cannot compare %s with %s", lt, rt)
		}
		return &Binary{At: e.Start(), Op: e.Op, L: l, R: r, Typ: types.Bool}, nil
	}

	// arithmetic
	if !numeric(lt) || !numeric(rt) || (e.Op == sql.OpMod && (lt == types.Float || rt == types.Float)) {
		return nil, errorf(e.Start(), "operator %s is not defined for %s and %s", e.Op, lt, rt)
	}
	l, r = promote(l, r)
	return &Binary{At: e.Start(), Op: e.Op, L: l, R: r, Typ: l.Type()}, nil
}

func numeric(t types.Type) bool { return t == types.Int || t == types.Float }

func boolArg(x Expr, op string, at sql.Pos) (Expr, error) {
	if isUntypedNull(x) {
		return typedNull(types.Bool), nil
	}
	if x.Type() != types.Bool {
		return nil, errorf(at, "argument of %s must be BOOL, found %s", op, x.Type())
	}
	return x, nil
}

func isUntypedNull(e Expr) bool {
	c, ok := e.(*Const)
	return ok && c.untyped
}

func typedNull(t types.Type) Expr { return &Const{Value: types.Null(t)} }

// defaultNull gives an untyped NULL that nothing constrains the type TEXT.
func defaultNull(e Expr) Expr {
	if isUntypedNull(e) {
		return typedNull(types.Text)
	}
	return e
}

// adaptNulls gives an untyped NULL the type of the other side, or INT if both
// sides are untyped.
func adaptNulls(l, r Expr) (Expr, Expr) {
	lu, ru := isUntypedNull(l), isUntypedNull(r)
	switch {
	case lu && ru:
		return typedNull(types.Int), typedNull(types.Int)
	case lu:
		return typedNull(r.Type()), r
	case ru:
		return l, typedNull(l.Type())
	}
	return l, r
}

// promote makes both sides FLOAT when one is INT and the other FLOAT.
func promote(l, r Expr) (Expr, Expr) {
	lt, rt := l.Type(), r.Type()
	switch {
	case lt == types.Int && rt == types.Float:
		return toFloat(l), r
	case lt == types.Float && rt == types.Int:
		return l, toFloat(r)
	}
	return l, r
}

// toFloat converts an INT expression. A constant is converted right away.
func toFloat(e Expr) Expr {
	if c, ok := e.(*Const); ok {
		if c.Value.IsNull() {
			return typedNull(types.Float)
		}
		return &Const{Value: types.NewFloat(float64(c.Value.Int()))}
	}
	return &Cast{X: e, To: types.Float}
}