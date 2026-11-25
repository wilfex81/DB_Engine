package sql

import (
	"strconv"
	"strings"
)

// ParseScript parses any number of statements separated by semicolons. Empty
// statements are skipped, and the last semicolon is optional.
func ParseScript(src string) ([]Stmt, error) {
	p, err := newParser(src)
	if err != nil {
		return nil, err
	}
	return p.parseScript()
}

// ParseStatement parses exactly one statement, with an optional semicolon.
func ParseStatement(src string) (Stmt, error) {
	p, err := newParser(src)
	if err != nil {
		return nil, err
	}
	stmts, err := p.parseScript()
	if err != nil {
		return nil, err
	}
	switch {
	case len(stmts) == 0:
		return nil, syntaxErrorf(p.peek().Pos, "expected a statement, found end of input")
	case len(stmts) > 1:
		return nil, syntaxErrorf(stmts[1].Start(), "expected one statement, found %d", len(stmts))
	}
	return stmts[0], nil
}

func (p *parser) parseScript() ([]Stmt, error) {
	var out []Stmt
	for {
		for p.peek().Kind == Semicolon {
			p.advance()
		}
		if p.peek().Kind == EOF {
			return out, nil
		}
		s, err := p.parseStmt()
		if err != nil {
			return nil, err
		}
		out = append(out, s)
		if t := p.peek(); t.Kind != Semicolon && t.Kind != EOF {
			return nil, syntaxErrorf(t.Pos, "unexpected %s after the statement", describe(t))
		}
	}
}

func (p *parser) parseStmt() (Stmt, error) {
	t := p.peek()
	switch t.Kind {
	case KwSelect:
		return p.parseSelect()
	case KwInsert:
		return p.parseInsert()
	case KwUpdate:
		return p.parseUpdate()
	case KwDelete:
		return p.parseDelete()
	case KwCreate:
		return p.parseCreate()
	case KwDrop:
		return p.parseDrop()
	}
	return nil, syntaxErrorf(t.Pos, "expected a statement (SELECT, INSERT, UPDATE, DELETE, CREATE or DROP), found %s", describe(t))
}

// name reads a table, column, index or alias name.
func (p *parser) name(what string) (Token, error) {
	t := p.peek()
	if t.Kind != Ident {
		return Token{}, syntaxErrorf(t.Pos, "expected %s, found %s", what, describe(t))
	}
	return p.advance(), nil
}

// peekAt looks n tokens ahead. Past the end it gives EOF.
func (p *parser) peekAt(n int) Token {
	if i := p.i + n; i < len(p.toks) {
		return p.toks[i]
	}
	return p.toks[len(p.toks)-1]
}

func (p *parser) parseWhere() (Expr, error) {
	if p.peek().Kind != KwWhere {
		return nil, nil
	}
	p.advance()
	return p.parseExpr()
}

// ---- CREATE and DROP

func (p *parser) parseCreate() (Stmt, error) {
	create := p.advance()
	switch t := p.peek(); t.Kind {
	case KwTable:
		p.advance()
		return p.parseCreateTable(create)
	case KwUnique:
		p.advance()
		if _, err := p.expect(KwIndex); err != nil {
			return nil, err
		}
		return p.parseCreateIndex(create)
	case KwIndex:
		return nil, syntaxErrorf(t.Pos, "only UNIQUE indexes are supported: write CREATE UNIQUE INDEX")
	default:
		return nil, syntaxErrorf(t.Pos, "expected TABLE or UNIQUE INDEX after CREATE, found %s", describe(t))
	}
}

func (p *parser) parseCreateTable(create Token) (Stmt, error) {
	name, err := p.name("a table name")
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(LParen); err != nil {
		return nil, err
	}
	s := &CreateTable{stmt: stmt{create.Pos}, Name: name.Text}
	for {
		col, err := p.parseColumnDef()
		if err != nil {
			return nil, err
		}
		s.Columns = append(s.Columns, col)
		if p.peek().Kind != Comma {
			break
		}
		p.advance()
	}
	if _, err := p.expect(RParen); err != nil {
		return nil, err
	}
	return s, nil
}

var typeNames = map[string]ColumnType{
	"INT": TypeInt, "INTEGER": TypeInt,
	"FLOAT": TypeFloat, "REAL": TypeFloat, "DOUBLE": TypeFloat,
	"BOOL": TypeBool, "BOOLEAN": TypeBool,
	"TEXT": TypeText,
}

// Type names are ordinary names that mean something only here, so a column
// may still be called text or int.
func (p *parser) parseColumnType() (ColumnType, error) {
	t := p.peek()
	if t.Kind != Ident {
		return 0, syntaxErrorf(t.Pos, "expected a type (INT, FLOAT, BOOL or TEXT), found %s", describe(t))
	}
	ct, ok := typeNames[strings.ToUpper(t.Text)]
	if !ok {
		return 0, syntaxErrorf(t.Pos, "unknown type %q: use INT, FLOAT, BOOL or TEXT", t.Text)
	}
	p.advance()
	return ct, nil
}

func (p *parser) parseColumnDef() (ColumnDef, error) {
	name, err := p.name("a column name")
	if err != nil {
		return ColumnDef{}, err
	}
	typ, err := p.parseColumnType()
	if err != nil {
		return ColumnDef{}, err
	}
	def := ColumnDef{At: name.Pos, Name: name.Text, Type: typ}
	for {
		t := p.peek()
		switch t.Kind {
		case KwNot:
			p.advance()
			if n := p.peek(); n.Kind != KwNull {
				return ColumnDef{}, syntaxErrorf(n.Pos, "expected NULL after NOT, found %s", describe(n))
			}
			p.advance()
			if def.NotNull {
				return ColumnDef{}, syntaxErrorf(t.Pos, "NOT NULL given twice")
			}
			def.NotNull = true
		case KwPrimary:
			p.advance()
			if n := p.peek(); n.Kind != KwKey {
				return ColumnDef{}, syntaxErrorf(n.Pos, "expected KEY after PRIMARY, found %s", describe(n))
			}
			p.advance()
			if def.PrimaryKey {
				return ColumnDef{}, syntaxErrorf(t.Pos, "PRIMARY KEY given twice")
			}
			def.PrimaryKey = true
		default:
			return def, nil
		}
	}
}

func (p *parser) parseCreateIndex(create Token) (Stmt, error) {
	name, err := p.name("an index name")
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(KwOn); err != nil {
		return nil, err
	}
	table, err := p.name("a table name")
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(LParen); err != nil {
		return nil, err
	}
	col, err := p.name("a column name")
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.Kind == Comma {
		return nil, syntaxErrorf(t.Pos, "an index covers exactly one column")
	}
	if _, err := p.expect(RParen); err != nil {
		return nil, err
	}
	return &CreateIndex{stmt: stmt{create.Pos}, Name: name.Text, Table: table.Text, Column: col.Text}, nil
}

func (p *parser) parseDrop() (Stmt, error) {
	drop := p.advance()
	switch t := p.peek(); t.Kind {
	case KwTable:
		p.advance()
		name, err := p.name("a table name")
		if err != nil {
			return nil, err
		}
		return &DropTable{stmt: stmt{drop.Pos}, Name: name.Text}, nil
	case KwIndex:
		p.advance()
		name, err := p.name("an index name")
		if err != nil {
			return nil, err
		}
		return &DropIndex{stmt: stmt{drop.Pos}, Name: name.Text}, nil
	default:
		return nil, syntaxErrorf(t.Pos, "expected TABLE or INDEX after DROP, found %s", describe(t))
	}
}

// ---- INSERT, UPDATE, DELETE

func (p *parser) parseInsert() (Stmt, error) {
	kw := p.advance()
	if _, err := p.expect(KwInto); err != nil {
		return nil, err
	}
	table, err := p.name("a table name")
	if err != nil {
		return nil, err
	}
	s := &Insert{stmt: stmt{kw.Pos}, Table: table.Text}

	if p.peek().Kind == LParen {
		p.advance()
		for {
			c, err := p.name("a column name")
			if err != nil {
				return nil, err
			}
			s.Columns = append(s.Columns, c.Text)
			if p.peek().Kind != Comma {
				break
			}
			p.advance()
		}
		if _, err := p.expect(RParen); err != nil {
			return nil, err
		}
	}

	if _, err := p.expect(KwValues); err != nil {
		return nil, err
	}
	for {
		open, err := p.expect(LParen)
		if err != nil {
			return nil, err
		}
		row := InsertRow{At: open.Pos}
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			row.Values = append(row.Values, e)
			if p.peek().Kind != Comma {
				break
			}
			p.advance()
		}
		if _, err := p.expect(RParen); err != nil {
			return nil, err
		}
		s.Rows = append(s.Rows, row)
		if p.peek().Kind != Comma {
			break
		}
		p.advance()
	}
	return s, nil
}

func (p *parser) parseUpdate() (Stmt, error) {
	kw := p.advance()
	table, err := p.name("a table name")
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(KwSet); err != nil {
		return nil, err
	}
	s := &Update{stmt: stmt{kw.Pos}, Table: table.Text}
	for {
		col, err := p.name("a column name")
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(Eq); err != nil {
			return nil, err
		}
		val, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		s.Set = append(s.Set, Assignment{At: col.Pos, Column: col.Text, Value: val})
		if p.peek().Kind != Comma {
			break
		}
		p.advance()
	}
	if s.Where, err = p.parseWhere(); err != nil {
		return nil, err
	}
	return s, nil
}

func (p *parser) parseDelete() (Stmt, error) {
	kw := p.advance()
	if _, err := p.expect(KwFrom); err != nil {
		return nil, err
	}
	table, err := p.name("a table name")
	if err != nil {
		return nil, err
	}
	s := &Delete{stmt: stmt{kw.Pos}, Table: table.Text}
	if s.Where, err = p.parseWhere(); err != nil {
		return nil, err
	}
	return s, nil
}

// ---- SELECT

func (p *parser) parseSelect() (Stmt, error) {
	kw := p.advance()
	s := &Select{stmt: stmt{kw.Pos}}

	for {
		item, err := p.parseSelectItem()
		if err != nil {
			return nil, err
		}
		s.Items = append(s.Items, item)
		if p.peek().Kind != Comma {
			break
		}
		p.advance()
	}

	if p.peek().Kind == KwFrom {
		p.advance()
		ref, err := p.parseTableRef()
		if err != nil {
			return nil, err
		}
		s.From = &ref
		for {
			j, ok, err := p.parseJoin()
			if err != nil {
				return nil, err
			}
			if !ok {
				break
			}
			s.Joins = append(s.Joins, j)
		}
	}

	var err error
	if s.Where, err = p.parseWhere(); err != nil {
		return nil, err
	}

	if p.peek().Kind == KwGroup {
		p.advance()
		if _, err := p.expect(KwBy); err != nil {
			return nil, err
		}
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			s.GroupBy = append(s.GroupBy, e)
			if p.peek().Kind != Comma {
				break
			}
			p.advance()
		}
	}

	if p.peek().Kind == KwOrder {
		p.advance()
		if _, err := p.expect(KwBy); err != nil {
			return nil, err
		}
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			item := OrderItem{Expr: e}
			switch p.peek().Kind {
			case KwAsc:
				p.advance()
			case KwDesc:
				p.advance()
				item.Desc = true
			}
			s.OrderBy = append(s.OrderBy, item)
			if p.peek().Kind != Comma {
				break
			}
			p.advance()
		}
	}

	if p.peek().Kind == KwLimit {
		p.advance()
		t := p.peek()
		if t.Kind != IntLit {
			return nil, syntaxErrorf(t.Pos, "expected a whole number after LIMIT, found %s", describe(t))
		}
		p.advance()
		n, err := strconv.ParseInt(t.Text, 10, 64)
		if err != nil {
			return nil, syntaxErrorf(t.Pos, "the integer %s is out of range", t.Text)
		}
		s.Limit = &n
	}
	return s, nil
}

// parseSelectItem reads *, t.*, or an expression with an optional alias. An
// alias may be written without AS, because every keyword is reserved and a
// plain name after an expression can only be an alias.
func (p *parser) parseSelectItem() (SelectItem, error) {
	t := p.peek()
	if t.Kind == Star {
		p.advance()
		return SelectItem{At: t.Pos, Star: true}, nil
	}
	if t.Kind == Ident && p.peekAt(1).Kind == Dot && p.peekAt(2).Kind == Star {
		p.advance()
		p.advance()
		p.advance()
		return SelectItem{At: t.Pos, TableStar: t.Text}, nil
	}

	e, err := p.parseExpr()
	if err != nil {
		return SelectItem{}, err
	}
	item := SelectItem{At: t.Pos, Expr: e}
	switch p.peek().Kind {
	case KwAs:
		p.advance()
		a, err := p.name("an alias")
		if err != nil {
			return SelectItem{}, err
		}
		item.Alias = a.Text
	case Ident:
		item.Alias = p.advance().Text
	}
	return item, nil
}

func (p *parser) parseTableRef() (TableRef, error) {
	name, err := p.name("a table name")
	if err != nil {
		return TableRef{}, err
	}
	ref := TableRef{At: name.Pos, Name: name.Text}
	switch p.peek().Kind {
	case KwAs:
		p.advance()
		a, err := p.name("an alias")
		if err != nil {
			return TableRef{}, err
		}
		ref.Alias = a.Text
	case Ident:
		ref.Alias = p.advance().Text
	}
	return ref, nil
}

// parseJoin reads [INNER] JOIN table ON expr. It returns false, with no
// error, when the next token does not start a join.
func (p *parser) parseJoin() (Join, bool, error) {
	switch t := p.peek(); t.Kind {
	case Comma:
		return Join{}, false, syntaxErrorf(t.Pos, "use JOIN ... ON instead of a comma between tables")
	case KwInner:
		p.advance()
		if j := p.peek(); j.Kind != KwJoin {
			return Join{}, false, syntaxErrorf(j.Pos, "expected JOIN after INNER, found %s", describe(j))
		}
	case KwJoin:
	default:
		return Join{}, false, nil
	}

	join := p.advance()
	ref, err := p.parseTableRef()
	if err != nil {
		return Join{}, false, err
	}
	if _, err := p.expect(KwOn); err != nil {
		return Join{}, false, err
	}
	on, err := p.parseExpr()
	if err != nil {
		return Join{}, false, err
	}
	return Join{At: join.Pos, Table: ref, On: on}, true, nil
}