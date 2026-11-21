// Package query implements the filter expression language:
//
//	expr    := or
//	or      := and ( '|' and )*
//	and     := unary ( '&' unary )*
//	unary   := '!' unary | '(' expr ')' | term
//	term    := field '=' value
//	         | field '!=' value
//	         | field 'in' '(' value (',' value)* ')'
//	         | field ('>='|'<='|'>'|'<') number
//	         | field 'between' number '..' number
//	         | 'all'
//
// Field names: brand, model, store, status, age, tuned, crack.
// String values may be quoted with single or double quotes. `tuned` compares
// months since 2014-01 (accepts "2023-05" or a plain number). `crack` accepts
// true/false. `!` is complement against all currently live rows.
package query

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Op int

const (
	OpEq Op = iota
	OpNe
	OpIn
	OpGe
	OpLe
	OpGt
	OpLt
	OpBetween
	OpAll
)

type Term struct {
	Field string
	Op    Op
	Str   string
	Str2  string // second value for between (strings)
	Num   int64
	Num2  int64
	StrIn []string
}

type Node interface{}

type AndNode struct{ Kids []Node }
type OrNode struct{ Kids []Node }
type NotNode struct{ Kid Node }
type TermNode struct{ Term Term }

// Parse parses a query expression into an AST.
func Parse(src string) (Node, error) {
	p := &parser{src: src}
	n, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.pos != len(p.src) {
		return nil, fmt.Errorf("unexpected trailing input at %d: %q", p.pos, p.src[p.pos:])
	}
	return n, nil
}

type parser struct {
	src string
	pos int
}

func (p *parser) skipSpace() {
	for p.pos < len(p.src) {
		r, w := utf8.DecodeRuneInString(p.src[p.pos:])
		if !unicode.IsSpace(r) {
			break
		}
		p.pos += w
	}
}

func (p *parser) peek() byte {
	p.skipSpace()
	if p.pos >= len(p.src) {
		return 0
	}
	return p.src[p.pos]
}

func (p *parser) accept(s string) bool {
	p.skipSpace()
	if strings.HasPrefix(p.src[p.pos:], s) {
		p.pos += len(s)
		return true
	}
	return false
}

func (p *parser) acceptWord(w string) bool {
	p.skipSpace()
	if !strings.HasPrefix(p.src[p.pos:], w) {
		return false
	}
	end := p.pos + len(w)
	if end < len(p.src) && isIdentByte(p.src[end]) {
		return false
	}
	p.pos = end
	return true
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '-' || unicode.IsLetter(rune(c)) || unicode.IsDigit(rune(c))
}

func (p *parser) parseOr() (Node, error) {
	first, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	kids := []Node{first}
	for p.accept("|") {
		n, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		kids = append(kids, n)
	}
	if len(kids) == 1 {
		return kids[0], nil
	}
	return &OrNode{Kids: kids}, nil
}

func (p *parser) parseAnd() (Node, error) {
	first, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	kids := []Node{first}
	for p.accept("&") {
		n, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		kids = append(kids, n)
	}
	if len(kids) == 1 {
		return kids[0], nil
	}
	return &AndNode{Kids: kids}, nil
}

func (p *parser) parseUnary() (Node, error) {
	if p.accept("!") {
		n, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &NotNode{Kid: n}, nil
	}
	if p.accept("(") {
		n, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if !p.accept(")") {
			return nil, fmt.Errorf("expected ')' at %d", p.pos)
		}
		return n, nil
	}
	return p.parseTerm()
}

func (p *parser) parseIdent() (string, error) {
	p.skipSpace()
	start := p.pos
	for p.pos < len(p.src) && (isIdentByte(p.src[p.pos]) || p.src[p.pos] == ':') {
		p.pos++
	}
	if start == p.pos {
		return "", fmt.Errorf("expected field name at %d", p.pos)
	}
	return p.src[start:p.pos], nil
}

func (p *parser) parseString() (string, error) {
	p.skipSpace()
	if p.pos >= len(p.src) {
		return "", fmt.Errorf("expected value at end of input")
	}
	if c := p.src[p.pos]; c == '\'' || c == '"' {
		p.pos++
		start := p.pos
		for p.pos < len(p.src) && p.src[p.pos] != c {
			p.pos++
		}
		if p.pos >= len(p.src) {
			return "", fmt.Errorf("unterminated string")
		}
		s := p.src[start:p.pos]
		p.pos++
		return s, nil
	}
	start := p.pos
	for p.pos < len(p.src) {
		r := rune(p.src[p.pos])
		if r < utf8.RuneSelf {
			c := p.src[p.pos]
			if c == ',' || c == ')' || c == '&' || c == '|' || c == '!' || unicode.IsSpace(r) {
				break
			}
			p.pos++
			continue
		}
		_, width := utf8.DecodeRuneInString(p.src[p.pos:])
		p.pos += width
	}
	if start == p.pos {
		return "", fmt.Errorf("expected value at %d", p.pos)
	}
	return p.src[start:p.pos], nil
}

func (p *parser) parseTerm() (Node, error) {
	if p.acceptWord("all") {
		return &TermNode{Term: Term{Op: OpAll}}, nil
	}
	field, err := p.parseIdent()
	if err != nil {
		return nil, err
	}
	field = strings.ToLower(field)
	switch field {
	case "brand", "model", "store", "status", "age", "tuned", "crack":
	default:
		return nil, fmt.Errorf("unknown field %q", field)
	}
	t := Term{Field: field}
	switch {
	case p.accept("!="):
		t.Op = OpNe
	case p.accept(">="):
		t.Op = OpGe
	case p.accept("<="):
		t.Op = OpLe
	case p.accept(">"):
		t.Op = OpGt
	case p.accept("<"):
		t.Op = OpLt
	case p.accept("="):
		t.Op = OpEq
	case p.acceptWord("in"):
		t.Op = OpIn
		if !p.accept("(") {
			return nil, fmt.Errorf("expected '(' after in")
		}
		for {
			v, err := p.parseString()
			if err != nil {
				return nil, err
			}
			t.StrIn = append(t.StrIn, v)
			if p.accept(")") {
				break
			}
			if !p.accept(",") {
				return nil, fmt.Errorf("expected ',' or ')' in list at %d", p.pos)
			}
		}
		return &TermNode{Term: t}, nil
	case p.acceptWord("between"):
		t.Op = OpBetween
		v1, err := p.parseString()
		if err != nil {
			return nil, err
		}
		if !p.accept("..") {
			return nil, fmt.Errorf("expected '..' in between")
		}
		v2, err := p.parseString()
		if err != nil {
			return nil, err
		}
		return p.finishTerm(t, v1, v2)
	default:
		return nil, fmt.Errorf("expected operator after %q", field)
	}
	v, err := p.parseString()
	if err != nil {
		return nil, err
	}
	return p.finishTerm(t, v, "")
}

func (p *parser) finishTerm(t Term, v1, v2 string) (Node, error) {
	switch t.Field {
	case "tuned":
		if strings.EqualFold(v1, "never") {
			if t.Op != OpEq && t.Op != OpNe {
				return nil, fmt.Errorf("tuned=never only supports = and !=")
			}
			t.Str = "never"
			return &TermNode{Term: t}, nil
		}
		fallthrough
	case "age":
		n1, err := parseNumField(t.Field, v1)
		if err != nil {
			return nil, err
		}
		t.Num = n1
		if t.Op == OpBetween {
			n2, err := parseNumField(t.Field, v2)
			if err != nil {
				return nil, err
			}
			t.Num2 = n2
		}
	case "crack":
		b1, err := strconv.ParseBool(strings.ToLower(v1))
		if err != nil {
			if v1 == "1" || v1 == "0" {
				b1 = v1 == "1"
			} else {
				return nil, fmt.Errorf("crack expects true/false, got %q", v1)
			}
		}
		if b1 {
			t.Num = 1
		}
		if t.Op == OpBetween {
			return nil, fmt.Errorf("between is not supported on crack")
		}
	default:
		// categorical string fields
		switch t.Op {
		case OpGe, OpLe, OpGt, OpLt, OpBetween:
			return nil, fmt.Errorf("%s does not support range comparison", t.Field)
		}
		t.Str = v1
		t.Str2 = v2
	}
	return &TermNode{Term: t}, nil
}

func parseNumField(field, v string) (int64, error) {
	if field == "tuned" {
		if strings.Count(v, "-") == 1 && len(v) >= 7 {
			parts := strings.SplitN(v, "-", 2)
			y, err1 := strconv.Atoi(parts[0])
			m, err2 := strconv.Atoi(parts[1])
			if err1 != nil || err2 != nil || m < 1 || m > 12 {
				return 0, fmt.Errorf("bad month %q", v)
			}
			return int64((y-2014)*12 + m - 1), nil
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s expects a number, got %q", field, v)
	}
	return n, nil
}

// String renders the AST back to the canonical query syntax.
func String(n Node) string {
	switch x := n.(type) {
	case *AndNode:
		parts := make([]string, len(x.Kids))
		for i, k := range x.Kids {
			parts[i] = String(k)
		}
		return "(" + strings.Join(parts, " & ") + ")"
	case *OrNode:
		parts := make([]string, len(x.Kids))
		for i, k := range x.Kids {
			parts[i] = String(k)
		}
		return "(" + strings.Join(parts, " | ") + ")"
	case *NotNode:
		return "!" + String(x.Kid)
	case *TermNode:
		t := x.Term
		if t.Op == OpAll {
			return "all"
		}
		if t.Field == "crack" {
			v := "false"
			if t.Num == 1 {
				v = "true"
			}
			return fmt.Sprintf("%s = %s", t.Field, v)
		}
		if t.Field == "tuned" && t.Str == "never" {
			return "tuned = never"
		}
		switch t.Op {
		case OpIn:
			return fmt.Sprintf("%s in (%s)", t.Field, strings.Join(t.StrIn, ", "))
		case OpBetween:
			return fmt.Sprintf("%s between %d .. %d", t.Field, t.Num, t.Num2)
		case OpGe:
			return fmt.Sprintf("%s >= %d", t.Field, t.Num)
		case OpLe:
			return fmt.Sprintf("%s <= %d", t.Field, t.Num)
		case OpGt:
			return fmt.Sprintf("%s > %d", t.Field, t.Num)
		case OpLt:
			return fmt.Sprintf("%s < %d", t.Field, t.Num)
		case OpNe:
			return fmt.Sprintf("%s != %s", t.Field, quoteIfNeeded(t.Str))
		default:
			return fmt.Sprintf("%s = %s", t.Field, quoteIfNeeded(t.Str))
		}
	}
	return "?"
}

func quoteIfNeeded(s string) string {
	if s == "" {
		return `""`
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_' && r != ':' {
			return strconv.Quote(s)
		}
	}
	return s
}
