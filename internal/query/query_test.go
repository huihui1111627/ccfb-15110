package query

import "testing"

func TestParsePrecedence(t *testing.T) {
	// & binds tighter than |
	n, err := Parse(`brand = A | brand = B & age >= 3`)
	if err != nil {
		t.Fatal(err)
	}
	or, ok := n.(*OrNode)
	if !ok || len(or.Kids) != 2 {
		t.Fatalf("expected top-level OR with 2 kids, got %#v", n)
	}
	if _, ok := or.Kids[1].(*AndNode); !ok {
		t.Fatalf("& should bind tighter than |")
	}
	// parentheses override
	n, err = Parse(`(brand = A | brand = B) & age >= 3`)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := n.(*AndNode); !ok {
		t.Fatalf("expected top-level AND")
	}
	// ! binds to the next unary
	n, err = Parse(`!brand = A & brand = B`)
	if err != nil {
		t.Fatal(err)
	}
	and, ok := n.(*AndNode)
	if !ok {
		t.Fatalf("expected AND, got %T", n)
	}
	if _, ok := and.Kids[0].(*NotNode); !ok {
		t.Fatalf("expected ! to bind to term only")
	}
}

func TestMonthLiteral(t *testing.T) {
	n, err := Parse(`tuned >= 2020-03`)
	if err != nil {
		t.Fatal(err)
	}
	term := n.(*TermNode).Term
	if term.Num != (2020-2014)*12+2 {
		t.Fatalf("got %d", term.Num)
	}
	if _, err := Parse(`tuned >= 2020-13`); err == nil {
		t.Fatalf("expected bad month error")
	}
}

func TestStringRoundTrip(t *testing.T) {
	inputs := []string{
		`brand = "Yamaha" & age >= 10`,
		`(status = 已租出 | status = 维保中) & !crack = true`,
		`tuned < 2020-01 & store in (北京朝阳旗舰店, 上海静安中心店)`,
		`age between 5 .. 20`,
		`tuned = never`,
		`!(brand = Yamaha | brand = Kawai)`,
		`all`,
		`crack = false`,
	}
	for _, s := range inputs {
		n, err := Parse(s)
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		s2 := String(n)
		n2, err := Parse(s2)
		if err != nil {
			t.Fatalf("canonical %q: %v", s2, err)
		}
		if String(n2) != s2 {
			t.Fatalf("unstable canonical form: %q -> %q", s2, String(n2))
		}
	}
}

func TestUnicodeValues(t *testing.T) {
	n, err := Parse(`store = "北京朝阳旗舰店" & status in ("在库可租", "待检测")`)
	if err != nil {
		t.Fatal(err)
	}
	and := n.(*AndNode)
	store := and.Kids[0].(*TermNode).Term
	if store.Str != "北京朝阳旗舰店" {
		t.Fatalf("got %q", store.Str)
	}
	in := and.Kids[1].(*TermNode).Term
	if len(in.StrIn) != 2 || in.StrIn[1] != "待检测" {
		t.Fatalf("got %v", in.StrIn)
	}
}
