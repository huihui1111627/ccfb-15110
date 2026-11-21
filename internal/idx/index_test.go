package idx

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"pianoindex/internal/bitmap"
	"pianoindex/internal/dataset"
	"pianoindex/internal/query"
)

// bruteRow satisfies a term against a raw record.
func bruteTerm(p dataset.Piano, t query.Term) bool {
	switch t.Field {
	case "brand":
		return bruteStr(p.Brand, t)
	case "model":
		return bruteStr(p.Model, t)
	case "store":
		return bruteStr(p.Store, t)
	case "status":
		return bruteStr(p.Status, t)
	case "age":
		return bruteNum(int64(p.Age), t)
	case "tuned":
		if t.Str == "never" {
			if t.Op == query.OpNe {
				return p.TunedMonth >= 0
			}
			return p.TunedMonth < 0
		}
		if p.TunedMonth < 0 {
			return false
		}
		return bruteNum(int64(p.TunedMonth), t)
	case "crack":
		b := p.HasCrack
		if t.Op == query.OpNe {
			return b != (t.Num == 1)
		}
		return b == (t.Num == 1)
	}
	return false
}

func bruteStr(v string, t query.Term) bool {
	switch t.Op {
	case query.OpEq:
		return v == t.Str
	case query.OpNe:
		return v != t.Str
	case query.OpIn:
		for _, s := range t.StrIn {
			if v == s {
				return true
			}
		}
		return false
	}
	return false
}

func bruteNum(v int64, t query.Term) bool {
	switch t.Op {
	case query.OpEq:
		return v == t.Num
	case query.OpNe:
		return v != t.Num
	case query.OpGe:
		return v >= t.Num
	case query.OpLe:
		return v <= t.Num
	case query.OpGt:
		return v > t.Num
	case query.OpLt:
		return v < t.Num
	case query.OpBetween:
		return v >= t.Num && v <= t.Num2
	}
	return false
}

func bruteEval(n query.Node, p dataset.Piano) bool {
	switch x := n.(type) {
	case *query.AndNode:
		for _, k := range x.Kids {
			if !bruteEval(k, p) {
				return false
			}
		}
		return true
	case *query.OrNode:
		for _, k := range x.Kids {
			if bruteEval(k, p) {
				return true
			}
		}
		return false
	case *query.NotNode:
		return !bruteEval(x.Kid, p)
	case *query.TermNode:
		if x.Term.Op == query.OpAll {
			return true
		}
		return bruteTerm(p, x.Term)
	}
	panic("bad node")
}

// randomQuery builds a random expression six to nine conditions deep.
func randomQuery(rng *rand.Rand, ix *Index) string {
	pick := func(vals []string) string {
		return vals[rng.Intn(len(vals))]
	}
	var terms []string
	nTerms := 6 + rng.Intn(4)
	for i := 0; i < nTerms; i++ {
		switch rng.Intn(7) {
		case 0:
			terms = append(terms, fmt.Sprintf("brand = %q", pick(ix.Brand.Values)))
		case 1:
			terms = append(terms, fmt.Sprintf("model = %q", pick(ix.Model.Values)))
		case 2:
			terms = append(terms, fmt.Sprintf("store = %q", pick(ix.Store.Values)))
		case 3:
			terms = append(terms, fmt.Sprintf("status in (%q, %q)", pick(ix.Status.Values), pick(ix.Status.Values)))
		case 4:
			op := []string{">=", "<=", ">", "<", "between"}[rng.Intn(5)]
			if op == "between" {
				a := rng.Intn(80)
				b := a + rng.Intn(30)
				terms = append(terms, fmt.Sprintf("age between %d .. %d", a, b))
			} else {
				terms = append(terms, fmt.Sprintf("age %s %d", op, rng.Intn(80)))
			}
		case 5:
			op := []string{">=", "<=", ">", "<"}[rng.Intn(4)]
			terms = append(terms, fmt.Sprintf("tuned %s %d", op, rng.Intn(144)))
		default:
			terms = append(terms, fmt.Sprintf("crack = %v", rng.Intn(2) == 1))
		}
	}
	// combine with & and | and some negation/parentheses
	expr := terms[0]
	for i := 1; i < len(terms); i++ {
		op := " & "
		if rng.Intn(4) == 0 {
			op = " | "
		}
		if rng.Intn(6) == 0 {
			terms[i] = "(" + terms[i] + " | " + "all" + ")"
		}
		expr = "(" + expr + op + terms[i] + ")"
	}
	if rng.Intn(3) == 0 {
		expr = "!(" + expr + ") & all"
	}
	return expr
}

// TestIndexAgainstBrute runs thousands of random queries and compares bitmaps
// with row-by-row evaluation over the raw records.
func TestIndexAgainstBrute(t *testing.T) {
	ps, _ := dataset.Generate(99, 120000)
	ix := Build(ps)
	rng := rand.New(rand.NewSource(123))
	for i := 0; i < 4000; i++ {
		src := randomQuery(rng, ix)
		node, err := query.Parse(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		got, _ := ix.Evaluate(node)
		gotRows := got.ToSlice()
		var want []uint32
		for j := range ps {
			if bruteEval(node, ps[j]) {
				want = append(want, uint32(j))
			}
		}
		if len(gotRows) != len(want) {
			t.Fatalf("query %s: got %d rows want %d", src, len(gotRows), len(want))
		}
		for k := range gotRows {
			if gotRows[k] != want[k] {
				t.Fatalf("query %s: row %d mismatch got %d want %d", src, k, gotRows[k], want[k])
			}
		}
	}
}

// TestNumericRanges compares every range predicate against a row-by-row scan.
func TestNumericRanges(t *testing.T) {
	ps, _ := dataset.Generate(5, 50000)
	ix := Build(ps)
	for v := 0; v <= 150; v += 1 {
		for _, op := range []string{">=", "<=", ">", "<"} {
			src := fmt.Sprintf("tuned %s %d", op, v)
			node, err := query.Parse(src)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := ix.Evaluate(node)
			cnt := 0
			for i := range ps {
				if ps[i].TunedMonth >= 0 && bruteNum(int64(ps[i].TunedMonth), node.(*query.TermNode).Term) {
					cnt++
				}
			}
			if got.Cardinality() != cnt {
				t.Fatalf("%s: got %d want %d", src, got.Cardinality(), cnt)
			}
		}
		for v := 0; v <= 90; v++ {
			src := fmt.Sprintf("age >= %d", v)
			node, _ := query.Parse(src)
			got, _ := ix.Evaluate(node)
			cnt := 0
			for i := range ps {
				if int64(ps[i].Age) >= int64(v) {
					cnt++
				}
			}
			if got.Cardinality() != cnt {
				t.Fatalf("%s: got %d want %d", src, got.Cardinality(), cnt)
			}
		}
	}
}

// TestTunedBetween checks the between form and month literals.
func TestTunedBetween(t *testing.T) {
	ps, _ := dataset.Generate(6, 30000)
	ix := Build(ps)
	node, err := query.Parse("tuned between 2020-01 .. 2023-12")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := ix.Evaluate(node)
	cnt := 0
	for i := range ps {
		m := ps[i].TunedMonth
		if m >= 0 && m >= 72 && m <= 119 {
			cnt++
		}
	}
	if got.Cardinality() != cnt {
		t.Fatalf("got %d want %d", got.Cardinality(), cnt)
	}
}

// TestNotUsesLiveRows checks that after removals NOT complements the live set.
func TestNotUsesLiveRows(t *testing.T) {
	ps, _ := dataset.Generate(7, 5000)
	ix := Build(ps)
	for i := 0; i < 1000; i++ {
		ix.RemovePiano(uint32(i * 3))
	}
	live := map[uint32]bool{}
	for i := range ps {
		live[uint32(i)] = true
	}
	for i := 0; i < 1000; i++ {
		delete(live, uint32(i*3))
	}
	node, _ := query.Parse("!status = \"已退役\"")
	got, _ := ix.Evaluate(node)
	want := 0
	for r := range live {
		if ps[r].Status != "已退役" {
			want++
		}
	}
	if got.Cardinality() != want {
		t.Fatalf("got %d want %d", got.Cardinality(), want)
	}
	if got.Cardinality() == 0 || got.Contains(uint32(3)) {
		t.Fatalf("removed row leaked into NOT result")
	}
}

// TestIncrementalUpdates compares the index after adds/updates/removals with a
// freshly rebuilt index.
func TestIncrementalUpdates(t *testing.T) {
	ps, sch := dataset.Generate(8, 3000)
	ix := Build(ps)
	rng := rand.New(rand.NewSource(17))
	ops := 0
	for i := 0; i < 800; i++ {
		switch rng.Intn(3) {
		case 0:
			p := dataset.Piano{
				Brand:      sch.Brands[rng.Intn(len(sch.Brands))],
				Model:      sch.Models[sch.Brands[0]][0],
				Store:      sch.Stores[rng.Intn(len(sch.Stores))],
				Status:     sch.Statuses[rng.Intn(len(sch.Statuses))],
				TunedMonth: rng.Intn(140) - 1,
				HasCrack:   rng.Intn(2) == 1,
				Age:        rng.Intn(80),
			}
			ix.AddPiano(p)
			ops++
		case 1:
			row := uint32(rng.Intn(len(ix.Pianos)))
			p := ix.Pianos[row]
			p.Status = sch.Statuses[rng.Intn(len(sch.Statuses))]
			p.Age = rng.Intn(80)
			p.TunedMonth = rng.Intn(150) - 1
			p.HasCrack = rng.Intn(2) == 1
			p.Store = sch.Stores[rng.Intn(len(sch.Stores))]
			ix.UpdatePiano(row, p)
			ops++
		case 2:
			row := uint32(rng.Intn(len(ps)))
			ix.RemovePiano(row)
			ops++
		}
	}
	// Compare all field bitmaps against the current live records.
	for i, p := range ix.Pianos {
		row := uint32(i)
		if !ix.All.Contains(row) {
			continue
		}
		if br := ix.Brand.BitmapFor(p.Brand); !br.Contains(row) {
			t.Fatalf("row %d missing from brand bitmap", i)
		}
		if b := ix.Status.BitmapFor(p.Status); !b.Contains(row) {
			t.Fatalf("row %d missing from status bitmap", i)
		}
		if v, ok := ix.Age.ValueOf(row); !ok || int(v) != p.Age {
			t.Fatalf("row %d age %d, index %d ok=%v", i, p.Age, v, ok)
		}
		if p.TunedMonth >= 0 {
			if v, ok := ix.Tuned.ValueOf(row); !ok || int(v) != p.TunedMonth {
				t.Fatalf("row %d tuned %d, index %d ok=%v", i, p.TunedMonth, v, ok)
			}
			if ix.NeverTuned.Contains(row) {
				t.Fatalf("row %d in never-tuned but has a month", i)
			}
		} else if !ix.NeverTuned.Contains(row) {
			t.Fatalf("row %d not in never-tuned", i)
		}
		if got := ix.HasCrack.Contains(row); got != p.HasCrack {
			t.Fatalf("row %d crack mismatch", i)
		}
	}
	// randomly query and cross-check
	rng = rand.New(rand.NewSource(18))
	for i := 0; i < 300; i++ {
		src := randomQuery(rng, ix)
		node, err := query.Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := ix.Evaluate(node)
		gotRows := got.ToSlice()
		var want []uint32
		for j, p := range ix.Pianos {
			if !ix.All.Contains(uint32(j)) {
				continue
			}
			if bruteEval(node, p) {
				want = append(want, uint32(j))
			}
		}
		if len(gotRows) != len(want) {
			t.Fatalf("after %d ops, query %s: got %d want %d", ops, src, len(gotRows), len(want))
		}
		for k := range gotRows {
			if gotRows[k] != want[k] {
				t.Fatalf("after %d ops, query %s: row %d got %d want %d", ops, src, k, gotRows[k], want[k])
			}
		}
	}
}

// TestParser errors and canonical strings.
func TestParser(t *testing.T) {
	good := []string{
		`brand = "Yamaha" & age >= 10`,
		`(status = "已租出" | status = "维保中") & !crack = true`,
		`tuned < 2019-06 & store in ("北京朝阳旗舰店", "上海静安中心店")`,
		`brand != Kawai & all`,
		`age between 5 .. 20`,
		`tuned = never`,
		`!(brand = Yamaha | brand = Kawai)`,
		`status in ("在库可租","已租出","维保中","待检测") & age <= 5 & tuned >= 2024-01 & crack = false`,
	}
	for _, s := range good {
		n, err := query.Parse(s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		if _, err := query.Parse(query.String(n)); err != nil {
			t.Fatalf("reparse canonical %q: %v", query.String(n), err)
		}
	}
	bad := []string{
		`brand =`,
		`brand ~ "x"`,
		`age > "abc"`,
		`(brand = x`,
		`brand = x &`,
		`unknown = 1`,
		`brand between 1 .. 2`,
	}
	for _, s := range bad {
		if _, err := query.Parse(s); err == nil {
			t.Fatalf("expected parse error for %q", s)
		}
	}
}

func TestSerializedIndexRoundTrip(t *testing.T) {
	ps, _ := dataset.Generate(10, 20000)
	ix := Build(ps)
	dir := t.TempDir()
	path := dir + "/brand.pbm"
	if err := ix.Brand.Bitmaps[0].Serialize(path, uint64(ix.RowCount)); err != nil {
		t.Fatal(err)
	}
	m, err := bitmap.OpenMapped(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if m.Bitmap().Cardinality() != ix.Brand.Bitmaps[0].Cardinality() {
		t.Fatalf("mapped cardinality mismatch")
	}
	// query the mapped bitmap through the normal ops path
	got := m.Bitmap().And(ix.Status.BitmapFor(ix.Status.Values[0]))
	want := 0
	for i := range ps {
		if ps[i].Brand == ix.Brand.Values[0] && ps[i].Status == ix.Status.Values[0] {
			want++
		}
	}
	if got.Cardinality() != want {
		t.Fatalf("mapped AND got %d want %d", got.Cardinality(), want)
	}
}

func TestCompositionAndMemory(t *testing.T) {
	ps, _ := dataset.Generate(11, 30000)
	ix := Build(ps)
	c := ix.Composition("brand=Yamaha", 4096)
	if c == nil || c.Stats.Rows == 0 {
		t.Fatalf("composition missing")
	}
	names := ix.BitmapNames()
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatalf("no bitmap names")
	}
	m := ix.Memory()
	if m.TotalByte == 0 || m.BitmapsCount < len(ix.Brand.Values) {
		t.Fatalf("memory report incomplete")
	}
	// a store bitmap should be dense & mostly bitset containers
	st := ix.Store.Bitmaps[0]
	stats := st.Stats()
	if stats.Buckets == 0 {
		t.Fatalf("empty store bitmap")
	}
	_ = stats
}

func BenchmarkQueries(b *testing.B) {
	ps, _ := dataset.Generate(12, 200000)
	ix := Build(ps)
	exprs := []string{
		`brand = Yamaha & status = "已租出"`,
		`(brand = Yamaha | brand = Kawai) & age >= 10 & age <= 40 & tuned < 2020-01`,
		`store = "北京朝阳旗舰店" & status in ("在库可租","维保中") & crack = false & tuned >= 2023-01`,
		`!(brand = Yamaha) & age between 5 .. 30`,
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, s := range exprs {
			n, _ := query.Parse(s)
			ix.Evaluate(n)
		}
	}
}

var _ = strings.TrimSpace
