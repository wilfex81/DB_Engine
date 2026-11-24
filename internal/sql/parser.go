package sql

import (
	"fmt"
	"strconv"
	"strings"
)

// maxNesting limits how deep parentheses, NOT and unary minus may go, so that
// a hostile input cannot overflow the stack.
const maxNesting = 200

type parser struct {
	toks  []Token
	i     int
	depth int
}

func newParser(src string) (*parser, error) {
	toks, err := Lex(src)
	if err != nil {
		return nil, err
	}
	return &parser{toks: toks}, nil
}

// ParseExpr parses src as one expression and nothing else.
func ParseExpr(src string) (Expr, error) {
	p, err := newParser(src)
	if err != nil {
		return nil, err
	}
	e, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.Kind != EOF {
		return nil, syntaxErrorf(t.Pos, "unexpected %s after the expression", describe(t))
	}
	return e, nil
}

func (p *parser) peek() Token { return p.toks[p.i] }

// advance returns the current token and moves on. It never moves past EOF.
func (p *parser) advance() Token {
	t := p.toks[p.i]
	if p.i < len(p.toks)-1 {
		p.i++
	}
	return t
}

func (p *parser) expect(k Kind) (Token, error) {
	t := p.peek()
	if t.Kind != k {
		return Token{}, syntaxErrorf(t.Pos, "expected %s, found %s", describeKind(k), describe(t))
	}
	return p.advance(), nil
}

// enter counts one more level of nesting. A caller that gets nil must call leave.
func (p *parser) enter(t Token) error {
	p.depth++
	if p.depth > maxNesting {
		return syntaxErrorf(t.Pos, "the expression is nested too deeply")
	}
	return nil
}

func (p *parser) leave() { p.depth-- }

// describe names a token for an error message.
func describe(t Token) string {
	switch t.Kind {
	case EOF:
		return "end of input"
	case Ident:
		return fmt.Sprintf("name %q", t.Text)
	case IntLit, FloatLit:
		return "number " + t.Text
	case StringLit:
		text := []rune(t.Text)
		suffix := ""
		if len(text) > 30 {
			text, suffix = text[:30], "..."
		}
		return "string '" + strings.ReplaceAll(string(text), "'", "''") + suffix + "'"
	}
	return describeKind(t.Kind)
}

// describeKind names a kind of token for an error message.
func describeKind(k Kind) string {
	switch {
	case k == EOF:
		return "end of input"
	case k == Ident:
		return "a name"
	case k == IntLit || k == FloatLit:
		return "a number"
	case k == StringLit:
		return "a string"
	case k.IsKeyword():
		return k.String()
	}
	return `"` + k.String() + `"`
}

func (p *parser) parseExpr() (Expr, error) { return p.parseOr() }

func (p *parser) parseOr() (Expr, error) {
	if err := p.enter(p.peek()); err != nil {
		return nil, err
	}
	defer p.leave()

	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.peek().Kind == KwOr {
		op := p.advance()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{node: node{op.Pos}, Op: OpOr, L: left, R: right}
	}
	return left, nil
}

func (p *parser) parseAnd() (Expr, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.peek().Kind == KwAnd {
		op := p.advance()
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{node: node{op.Pos}, Op: OpAnd, L: left, R: right}
	}
	return left, nil
}

func (p *parser) parseNot() (Expr, error) {
	t := p.peek()
	if t.Kind != KwNot {
		return p.parseIs()
	}
	if err := p.enter(t); err != nil {
		return nil, err
	}
	defer p.leave()
	p.advance()
	x, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	return &UnaryExpr{node: node{t.Pos}, Op: OpNot, X: x}, nil
}

func (p *parser) parseIs() (Expr, error) {
	x, err := p.parseComparison()
	if err != nil {
		return nil, err
	}
	for p.peek().Kind == KwIs {
		is := p.advance()
		negated := false
		if p.peek().Kind == KwNot {
			p.advance()
			negated = true
		}
		if t := p.peek(); t.Kind != KwNull {
			if negated {
				return nil, syntaxErrorf(t.Pos, "expected NULL after IS NOT, found %s", describe(t))
			}
			return nil, syntaxErrorf(t.Pos, "expected NULL or NOT NULL after IS, found %s", describe(t))
		}
		p.advance()
		x = &IsNullExpr{node: node{is.Pos}, X: x, Negated: negated}
	}
	return x, nil
}

var comparisons = map[Kind]BinaryOp{
	Eq: OpEq, NotEq: OpNotEq, Lt: OpLt, LtEq: OpLtEq, Gt: OpGt, GtEq: OpGtEq,
}

// parseComparison allows one comparison and no more: a < b < c would compare
// a true or false value with c, which is never what was meant.
func (p *parser) parseComparison() (Expr, error) {
	left, err := p.parseAdditive()
	if err != nil {
		return nil, err
	}
	op, ok := comparisons[p.peek().Kind]
	if !ok {
		return left, nil
	}
	t := p.advance()
	right, err := p.parseAdditive()
	if err != nil {
		return nil, err
	}
	if next := p.peek(); comparisons[next.Kind] != 0 {
		return nil, syntaxErrorf(next.Pos, "comparison operators cannot be chained: combine them with AND")
	}
	return &BinaryExpr{node: node{t.Pos}, Op: op, L: left, R: right}, nil
}

func (p *parser) parseAdditive() (Expr, error) {
	left, err := p.parseMultiplicative()
	if err != nil {
		return nil, err
	}
	for {
		var op BinaryOp
		switch p.peek().Kind {
		case Plus:
			op = OpAdd
		case Minus:
			op = OpSub
		default:
			return left, nil
		}
		t := p.advance()
		right, err := p.parseMultiplicative()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{node: node{t.Pos}, Op: op, L: left, R: right}
	}
}

func (p *parser) parseMultiplicative() (Expr, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		var op BinaryOp
		switch p.peek().Kind {
		case Star:
			op = OpMul
		case Slash:
			op = OpDiv
		case Percent:
			op = OpMod
		default:
			return left, nil
		}
		t := p.advance()
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{node: node{t.Pos}, Op: op, L: left, R: right}
	}
}

func (p *parser) parseUnary() (Expr, error) {
	t := p.peek()
	if t.Kind != Minus {
		return p.parsePrimary()
	}
	if err := p.enter(t); err != nil {
		return nil, err
	}
	defer p.leave()
	p.advance()

	// A minus directly in front of a number belongs to the number. That is how
	// the smallest int64 can be written, and it lets the planner see
	// "id = -5" as a constant.
	if n := p.peek(); n.Kind == IntLit || n.Kind == FloatLit {
		p.advance()
		return numberLiteral(n, true, t.Pos)
	}
	x, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	return &UnaryExpr{node: node{t.Pos}, Op: OpNeg, X: x}, nil
}

func (p *parser) parsePrimary() (Expr, error) {
	t := p.peek()
	switch t.Kind {
	case IntLit, FloatLit:
		p.advance()
		return numberLiteral(t, false, t.Pos)
	case StringLit:
		p.advance()
		return &StringLiteral{node: node{t.Pos}, Value: t.Text}, nil
	case KwTrue, KwFalse:
		p.advance()
		return &BoolLiteral{node: node{t.Pos}, Value: t.Kind == KwTrue}, nil
	case KwNull:
		p.advance()
		return &NullLiteral{node{t.Pos}}, nil
	case LParen:
		p.advance()
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(RParen); err != nil {
			return nil, err
		}
		return e, nil
	case Ident:
		p.advance()
		switch p.peek().Kind {
		case LParen:
			return p.parseCall(t)
		case Dot:
			p.advance()
			c := p.peek()
			if c.Kind != Ident {
				return nil, syntaxErrorf(c.Pos, `expected a column name after ".", found %s`, describe(c))
			}
			p.advance()
			return &ColumnRef{node: node{t.Pos}, Table: t.Text, Name: c.Text}, nil
		}
		return &ColumnRef{node: node{t.Pos}, Name: t.Text}, nil
	}
	return nil, syntaxErrorf(t.Pos, "expected an expression, found %s", describe(t))
}

// parseCall parses the part of name(args) from the opening parenthesis on.
func (p *parser) parseCall(name Token) (Expr, error) {
	p.advance() // (
	call := &FuncCall{node: node{name.Pos}, Name: name.Text}
	switch p.peek().Kind {
	case Star:
		p.advance()
		call.Star = true
	case RParen:
		// no arguments
	default:
		for {
			arg, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			call.Args = append(call.Args, arg)
			if p.peek().Kind != Comma {
				break
			}
			p.advance()
		}
	}
	if _, err := p.expect(RParen); err != nil {
		return nil, err
	}
	return call, nil
}

// numberLiteral converts a number token. With neg set, a minus sign came
// before it, at position at.
func numberLiteral(t Token, neg bool, at Pos) (Expr, error) {
	if t.Kind == IntLit {
		text := t.Text
		if neg {
			text = "-" + text
		}
		v, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, syntaxErrorf(t.Pos, "the integer %s is out of range", text)
		}
		return &IntLiteral{node: node{at}, Value: v}, nil
	}
	v, err := strconv.ParseFloat(t.Text, 64)
	if err != nil {
		return nil, syntaxErrorf(t.Pos, "the number %s is out of range", t.Text)
	}
	if neg {
		v = -v
	}
	return &FloatLiteral{node: node{at}, Value: v}, nil
}
