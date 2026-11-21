// Command bench generates a large synthetic archive and exercises thousands of
// randomly composed, 6-9 condition queries, checking every result against a
// brute-force row scan and reporting per-query terms of latency.
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"time"

	"pianoindex/internal/dataset"
	"pianoindex/internal/idx"
	"pianoindex/internal/query"
)

func main() {
	n := flag.Int("n", 300000, "number of pianos")
	queries := flag.Int("q", 3000, "number of random queries")
	seed := flag.Int64("seed", 42, "random seed")
	verbose := flag.Bool("v", false, "print every query")
	flag.Parse()

	fmt.Printf("generating %d pianos...\n", *n)
	t0 := time.Now()
	ps, _ := dataset.Generate(*seed, *n)
	fmt.Printf("generated in %v\n", time.Since(t0))

	t0 = time.Now()
	ix := idx.Build(ps)
	buildTime := time.Since(t0)
	fmt.Printf("indexed in %v\n", buildTime)
	mem := ix.Memory()
	fmt.Printf("index memory %.1f MiB across %d bitmaps (rows %.1f MiB)\n",
		float64(mem.TotalByte)/(1<<20), mem.BitmapsCount, float64(mem.RowBytes)/(1<<20))
	for _, f := range []string{"brand", "model", "store", "status", "age", "tuned", "crack"} {
		fmt.Printf("  %-7s %8.1f KiB\n", f, float64(mem.Fields[f])/1024)
	}

	rng := rand.New(rand.NewSource(*seed + 1))
	type sample struct {
		src   string
		terms int
	}
	samples := make([]sample, 0, *queries)
	for len(samples) < *queries {
		src := randomQuery(rng, ix)
		samples = append(samples, sample{src: src, terms: countTerms(src)})
	}

	type timing struct {
		terms int
		dur   time.Duration
	}
	timings := make([]timing, 0, *queries)
	var total time.Duration
	mismatches := 0
	var maxDur time.Duration
	for i, s := range samples {
		node, err := query.Parse(s.src)
		if err != nil {
			fmt.Fprintf(os.Stderr, "parse error for %q: %v\n", s.src, err)
			os.Exit(1)
		}
		start := time.Now()
		got, _ := ix.Evaluate(node)
		d := time.Since(start)
		timings = append(timings, timing{s.terms, d})
		total += d
		if d > maxDur {
			maxDur = d
		}
		// brute-force reference
		cnt := 0
		for j := range ps {
			if bruteEval(node, ps[j]) {
				cnt++
			}
		}
		if got.Cardinality() != cnt {
			mismatches++
			fmt.Printf("MISMATCH %q: bitmap=%d brute=%d\n", s.src, got.Cardinality(), cnt)
		}
		if *verbose {
			fmt.Printf("%5d %8s %6d rows  %s\n", i, d.Round(time.Microsecond), got.Cardinality(), s.src)
		}
		if (i+1)%500 == 0 {
			fmt.Printf("  %d/%d queries checked, %.2f ms avg\n", i+1, *queries, float64(total.Microseconds())/float64(i+1)/1000)
		}
	}
	if mismatches > 0 {
		fmt.Printf("FAILED: %d mismatches\n", mismatches)
		os.Exit(1)
	}
	fmt.Printf("\nall %d queries match the brute-force reference\n", len(samples))
	sort.Slice(timings, func(i, j int) bool { return timings[i].dur < timings[j].dur })
	pct := func(p float64) time.Duration {
		i := int(float64(len(timings)-1) * p)
		return timings[i].dur
	}
	fmt.Printf("latency: p50=%v p90=%v p99=%v max=%v mean=%v\n",
		pct(0.50), pct(0.90), pct(0.99), maxDur, total/time.Duration(len(timings)))

	// raw scan cost for comparison
	start := time.Now()
	_ = ix.Rows(ix.All, 1<<30)
	fmt.Printf("materializing every matching row: %v\n", time.Since(start))
}

func countTerms(src string) int {
	n := 0
	for _, c := range src {
		if c == '&' || c == '|' {
			n++
		}
	}
	return n + 1
}

func randomQuery(rng *rand.Rand, ix *idx.Index) string {
	pick := func(vals []string) string { return vals[rng.Intn(len(vals))] }
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
				terms = append(terms, fmt.Sprintf("age between %d .. %d", a, a+rng.Intn(30)))
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
	expr := terms[0]
	for i := 1; i < len(terms); i++ {
		op := " & "
		if rng.Intn(4) == 0 {
			op = " | "
		}
		if rng.Intn(6) == 0 {
			terms[i] = "(" + terms[i] + " | all)"
		}
		expr = "(" + expr + op + terms[i] + ")"
	}
	if rng.Intn(3) == 0 {
		expr = "!(" + expr + ") & all"
	}
	return expr
}

// bruteEval is the row-by-row reference implementation.
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
		t := x.Term
		if t.Op == query.OpAll {
			return true
		}
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
			want := t.Num == 1
			if t.Op == query.OpNe {
				return p.HasCrack != want
			}
			return p.HasCrack == want
		}
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
