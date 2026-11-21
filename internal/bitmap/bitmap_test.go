package bitmap

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

// TestChooseKindIsSizeOptimal pins the threshold behaviour: arrays win below
// 4096 values, bitsets above (8 KiB), and runs only when their 4 bytes/run is
// both smaller than the alternatives and below the scan-cost cap.
func TestChooseKindIsSizeOptimal(t *testing.T) {
	cases := []struct {
		card, runs int
		want       Kind
	}{
		{0, 0, Array}, {1, 1, Array}, {4096, 4096, Array}, {4096, 2048, Array},
		{4097, 4097, Bitset}, {4097, 1, Run}, {65536, 1, Run}, {65536, 1024, Run},
		{40000, 1025, Bitset}, {40000, 900, Run}, {5000, 2000, Bitset},
		{1000, 100, Run}, {1000, 400, Run}, {1000, 600, Array},
	}
	for _, c := range cases {
		if got := chooseKind(c.card, c.runs); got != c.want {
			t.Fatalf("chooseKind(%d,%d)=%v want %v", c.card, c.runs, got, c.want)
		}
	}
}

// brute is the reference model: a plain map/set of low-16 values.
type brute map[uint16]struct{}

func bruteOf(c Container) brute {
	b := brute{}
	c.ForEach(func(v uint16) bool { b[v] = struct{}{}; return true })
	return b
}

func (b brute) slice() []uint16 {
	out := make([]uint16, 0, len(b))
	for v := range b {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sameContainer(t *testing.T, got Container, want brute) {
	t.Helper()
	gotSlice := sortedValues(got)
	wantSlice := want.slice()
	if len(gotSlice) != len(wantSlice) {
		t.Fatalf("cardinality mismatch: got %d want %d", len(gotSlice), len(wantSlice))
	}
	for i := range gotSlice {
		if gotSlice[i] != wantSlice[i] {
			t.Fatalf("value mismatch at %d: got %d want %d", i, gotSlice[i], wantSlice[i])
		}
	}
	if err := got.DeepCopy(); err == nil { // silence unused warnings, no-op
	}
}

// randomVals builds a container with a controlled shape.
func randomVals(rng *rand.Rand, shape string) []uint16 {
	seen := map[uint16]struct{}{}
	add := func(v uint16) { seen[v] = struct{}{} }
	switch shape {
	case "sparse": // scattered singletons
		n := rng.Intn(200) + 1
		for i := 0; i < n; i++ {
			add(uint16(rng.Intn(65536)))
		}
		if len(seen) == 0 {
			add(0)
		}
	case "dense": // scattered, many values
		n := rng.Intn(40000) + 5000
		for i := 0; i < n; i++ {
			add(uint16(rng.Intn(65536)))
		}
	case "runs": // a few long runs
		n := rng.Intn(5) + 1
		for i := 0; i < n; i++ {
			start := rng.Intn(60000)
			length := rng.Intn(5000) + 1
			if start+length > 65535 {
				length = 65535 - start
			}
			for v := start; v <= start+length; v++ {
				add(uint16(v))
			}
		}
	case "boundary": // values around threshold boundaries
		card := []int{0, 1, 2, MixedThreshold - 1, MixedThreshold, MixedThreshold + 1, 65536}
		want := card[rng.Intn(len(card))]
		if want == 0 {
			return nil
		}
		for len(seen) < want {
			add(uint16(rng.Intn(65536)))
		}
	case "full":
		for v := 0; v < 65536; v++ {
			add(uint16(v))
		}
	case "single-run":
		start := rng.Intn(65536)
		length := rng.Intn(65536 - start)
		for v := start; v <= start+length; v++ {
			add(uint16(v))
		}
	}
	vals := make([]uint16, 0, len(seen))
	for v := range seen {
		vals = append(vals, v)
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	return vals
}

func containerOfShape(rng *rand.Rand, shape string) Container {
	return containerFromSorted(randomVals(rng, shape))
}

func forceKind(vals []uint16, k Kind) Container {
	switch k {
	case Array:
		return &ArrayContainer{data: append([]uint16{}, vals...)}
	case Run:
		return newRunContainerFromSorted(vals)
	case Bitset:
		return newBitsetFromSorted(vals)
	}
	panic("bad kind")
}

var shapes = []string{"sparse", "dense", "runs", "boundary", "full", "single-run"}

// TestOpsExhaustiveKinds tests all 3x3 container-type pairs for all four ops,
// forcing each representation regardless of what the canonicalizer would pick.
func TestOpsExhaustiveKinds(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	kinds := []Kind{Array, Bitset, Run}
	ops := []struct {
		name string
		fn   func(Container, Container) Container
		ref  func(a, b brute) brute
	}{
		{"and", opAnd, func(a, b brute) brute {
			out := brute{}
			for v := range a {
				if _, ok := b[v]; ok {
					out[v] = struct{}{}
				}
			}
			return out
		}},
		{"or", opOr, func(a, b brute) brute {
			out := brute{}
			for v := range a {
				out[v] = struct{}{}
			}
			for v := range b {
				out[v] = struct{}{}
			}
			return out
		}},
		{"xor", opXor, func(a, b brute) brute {
			out := brute{}
			for v := range a {
				out[v] = struct{}{}
			}
			for v := range b {
				if _, ok := out[v]; ok {
					delete(out, v)
				} else {
					out[v] = struct{}{}
				}
			}
			return out
		}},
		{"andnot", opAndNot, func(a, b brute) brute {
			out := brute{}
			for v := range a {
				if _, ok := b[v]; !ok {
					out[v] = struct{}{}
				}
			}
			return out
		}},
	}
	for _, op := range ops {
		for _, ka := range kinds {
			for _, kb := range kinds {
				shapeA := shapes[rng.Intn(len(shapes))]
				shapeB := shapes[rng.Intn(len(shapes))]
				// Many repetitions with varied shapes.
				for rep := 0; rep < 12; rep++ {
					valsA := randomVals(rng, shapeA)
					valsB := randomVals(rng, shapeB)
					// Keep runtime bounded for dense/full combos.
					if len(valsA) > 20000 && ka == Array {
						ka = Bitset
					}
					a := forceKind(valsA, ka)
					b := forceKind(valsB, kb)
					want := op.ref(bruteOf(a), bruteOf(b))
					got := op.fn(a, b)
					sameContainer(t, got, want)
					if got.Cardinality() != len(want) {
						t.Fatalf("%s %s/%s cardinality %d != %d", op.name, ka, kb, got.Cardinality(), len(want))
					}
				}
			}
		}
	}
}

// TestEmptyResultsAreArrays guards the canonical empty representation across all
// operands and both fast and slow paths.
func TestEmptyResultsAreArrays(t *testing.T) {
	empty := &ArrayContainer{}
	emptyBits := newBitsetContainer()
	emptyRun := &RunContainer{}
	some := containerOfShape(rand.New(rand.NewSource(9)), "dense")
	for _, a := range []Container{empty, emptyBits, emptyRun, some} {
		for _, b := range []Container{empty, emptyBits, emptyRun, some} {
			for _, op := range []func(Container, Container) Container{opAnd, opOr, opXor, opAndNot} {
				got := op(a, b)
				if got.Cardinality() == 0 {
					if _, ok := got.(*ArrayContainer); !ok {
						t.Fatalf("%T %T -> empty %T", a, b, got)
					}
				}
			}
		}
	}
}

// TestOpsResultTypeIsCanonical checks the result type matches chooseKind for its
// cardinality and run count.
func TestOpsResultTypeIsCanonical(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	for rep := 0; rep < 300; rep++ {
		a := containerOfShape(rng, shapes[rng.Intn(len(shapes))])
		b := containerOfShape(rng, shapes[rng.Intn(len(shapes))])
		for _, op := range []func(Container, Container) Container{opAnd, opOr, opXor, opAndNot} {
			got := op(a, b)
			card := got.Cardinality()
			if card == 0 {
				if _, ok := got.(*ArrayContainer); !ok {
					t.Fatalf("empty result is %T, want array", got)
				}
				continue
			}
			runs := countContainerRuns(got)
			wantKind := chooseKind(card, runs)
			if got.Kind() != wantKind {
				t.Fatalf("result kind %v for card %d runs %d, want %v", got.Kind(), card, runs, wantKind)
			}
		}
	}
}

func countContainerRuns(c Container) int {
	switch x := c.(type) {
	case *ArrayContainer:
		runs := 0
		for i, v := range x.data {
			if i == 0 || v != x.data[i-1]+1 {
				runs++
			}
		}
		return runs
	case *RunContainer:
		return len(x.runs)
	case *BitsetContainer:
		return x.countRuns()
	}
	return 0
}

// TestContainerThresholdTransitions focuses on the array<->bitset boundary and
// the run<->bitset boundary during incremental edits.
func TestContainerThresholdTransitions(t *testing.T) {
	// array grows to exactly 4096 (stays array), 4097 (becomes bitset)
	a := &ArrayContainer{}
	for v := 0; v < MixedThreshold; v++ {
		a2 := a.applyAdd(uint16(v * 3)).(*ArrayContainer)
		a = a2
	}
	if a.Kind() != Array {
		t.Fatalf("array with %d values should stay array", MixedThreshold)
	}
	// one more: crosses to bitset
	next := a.applyAdd(uint16(MixedThreshold*3 + 3))
	if _, ok := next.(*BitsetContainer); !ok {
		t.Fatalf("expected bitset after crossing threshold, got %T", next)
	}
	// removing from bitset back down to 4096 returns an array
	b := next.(*BitsetContainer)
	var cur Container = b
	for i := 0; i < len(a.data); i++ {
		cur = cur.applyRemove(a.data[i])
	}
	if _, ok := cur.(*ArrayContainer); !ok {
		t.Fatalf("expected array after shrinking below threshold, got %T", cur)
	}

	// A run container whose runs now cost more than a bitset converts on edit.
	var runs [][2]uint16
	for i := 0; i < 2100; i++ {
		runs = append(runs, [2]uint16{uint16(i * 30), uint16(i*30 + 7)})
	}
	rc := &RunContainer{runs: runs}
	if rc.Cardinality() <= MixedThreshold {
		t.Fatalf("test setup: cardinality %d should exceed threshold", rc.Cardinality())
	}
	got := rc.applyRemove(uint16(runs[0][0]))
	if _, ok := got.(*BitsetContainer); !ok {
		t.Fatalf("expected bitset after crossing run threshold, got %T", got)
	}
}

// TestRangeOpsMatchBrute tests whole-container and bitmap-level range set/clear
// and flip against the brute set.
func TestRangeOpsMatchBrute(t *testing.T) {
	rng := rand.New(rand.NewSource(23))
	for rep := 0; rep < 400; rep++ {
		vals := randomVals(rng, shapes[rng.Intn(len(shapes))])
		lo := uint16(rng.Intn(65536))
		hi := uint16(lo + uint16(rng.Intn(65536-int(lo))))
		for _, k := range []Kind{Array, Bitset, Run} {
			base := forceKind(vals, k)
			c := base.DeepCopy()
			want := bruteOf(c)
			for v := int(lo); v <= int(hi); v++ {
				want[uint16(v)] = struct{}{}
			}
			got := c.applyRange(lo, hi, true)
			sameContainer(t, got, want)

			c = base.DeepCopy()
			want = bruteOf(c)
			for v := int(lo); v <= int(hi); v++ {
				delete(want, uint16(v))
			}
			got = c.applyRange(lo, hi, false)
			sameContainer(t, got, want)

			c = base.DeepCopy()
			want = bruteOf(c)
			for v := int(lo); v <= int(hi); v++ {
				if _, ok := want[uint16(v)]; ok {
					delete(want, uint16(v))
				} else {
					want[uint16(v)] = struct{}{}
				}
			}
			got = flipRange(c, lo, hi)
			sameContainer(t, got, want)
		}
	}
}

func TestBitmapOperationsMatchBrute(t *testing.T) {
	rng := rand.New(rand.NewSource(31))
	for rep := 0; rep < 60; rep++ {
		n := rng.Intn(30000) + 100
		a := New()
		b := New()
		setA := map[uint32]struct{}{}
		setB := map[uint32]struct{}{}
		for i := 0; i < n; i++ {
			r := uint32(rng.Intn(1 << 20))
			a.Add(r)
			setA[r] = struct{}{}
		}
		for i := 0; i < n; i++ {
			r := uint32(rng.Intn(1 << 20))
			b.Add(r)
			setB[r] = struct{}{}
		}
		check := func(name string, got *Bitmap, want map[uint32]struct{}) {
			t.Helper()
			gs := got.ToSlice()
			if len(gs) != len(want) {
				t.Fatalf("%s card %d != %d", name, len(gs), len(want))
			}
			for _, r := range gs {
				if _, ok := want[r]; !ok {
					t.Fatalf("%s has stray %d", name, r)
				}
			}
			if err := got.Validate(); err != nil {
				t.Fatalf("%s invalid: %v", name, err)
			}
		}
		and := map[uint32]struct{}{}
		or := map[uint32]struct{}{}
		xor := map[uint32]struct{}{}
		sub := map[uint32]struct{}{}
		for r := range setA {
			if _, ok := setB[r]; ok {
				and[r] = struct{}{}
			}
			or[r] = struct{}{}
		}
		for r := range setB {
			or[r] = struct{}{}
		}
		for r := range or {
			_, inA := setA[r]
			_, inB := setB[r]
			if inA != inB {
				xor[r] = struct{}{}
			}
		}
		for r := range setA {
			if _, ok := setB[r]; !ok {
				sub[r] = struct{}{}
			}
		}
		check("and", a.And(b), and)
		check("or", a.Or(b), or)
		check("xor", a.Xor(b), xor)
		check("andnot", a.AndNot(b), sub)
	}
}

func TestBitmapRangeAndStatsMatchBrute(t *testing.T) {
	rng := rand.New(rand.NewSource(41))
	for rep := 0; rep < 30; rep++ {
		b := New()
		set := map[uint32]struct{}{}
		for i := 0; i < 5000; i++ {
			r := uint32(rng.Intn(1 << 20))
			b.Add(r)
			set[r] = struct{}{}
		}
		lo := uint32(rng.Intn(1 << 20))
		hi := lo + uint32(rng.Intn(1<<19))
		if hi > 1<<20 {
			hi = 1 << 20
		}
		want := map[uint32]struct{}{}
		for r := range set {
			want[r] = struct{}{}
		}
		for r := lo; r <= hi; r++ {
			want[r] = struct{}{}
		}
		b.SetRange(lo, hi, true)
		checkBitmapRows(t, b, want)

		for r := lo; r <= hi; r++ {
			delete(want, r)
		}
		b.SetRange(lo, hi, false)
		checkBitmapRows(t, b, want)

		for r := lo; r <= hi; r++ {
			if _, ok := want[r]; ok {
				delete(want, r)
			} else {
				want[r] = struct{}{}
			}
		}
		b.FlipRange(lo, hi)
		checkBitmapRows(t, b, want)

		// rank / select / rangecard
		var sorted []uint32
		for r := range want {
			sorted = append(sorted, r)
		}
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		if b.Cardinality() != len(sorted) {
			t.Fatalf("card mismatch")
		}
		for _, idx := range []int{0, len(sorted) / 2, len(sorted) - 1} {
			if got := b.Select(idx); got != sorted[idx] {
				t.Fatalf("select %d = %d want %d", idx, got, sorted[idx])
			}
		}
		for _, probe := range []uint32{lo, hi, (lo + hi) / 2} {
			wantRank := 0
			for _, r := range sorted {
				if r <= probe {
					wantRank++
				}
			}
			if got := b.Rank(probe); got != wantRank {
				t.Fatalf("rank(%d)=%d want %d", probe, got, wantRank)
			}
			wantRC := 0
			for _, r := range sorted {
				if r >= lo && r <= probe {
					wantRC++
				}
			}
			if got := b.RangeCard(lo, probe); got != wantRC {
				t.Fatalf("rangeCard(%d,%d)=%d want %d", lo, probe, got, wantRC)
			}
		}
	}
}

func checkBitmapRows(t *testing.T, b *Bitmap, want map[uint32]struct{}) {
	t.Helper()
	if !b.KeysSorted() {
		t.Fatalf("keys not sorted")
	}
	rows := b.ToSlice()
	if len(rows) != len(want) {
		t.Fatalf("card %d != %d", len(rows), len(want))
	}
	for _, r := range rows {
		if _, ok := want[r]; !ok {
			t.Fatalf("stray row %d", r)
		}
	}
	if err := b.Validate(); err != nil {
		t.Fatalf("invalid bitmap: %v", err)
	}
}

func TestIncrementalUpdatesMatchBrute(t *testing.T) {
	rng := rand.New(rand.NewSource(53))
	b := New()
	set := map[uint32]struct{}{}
	for i := 0; i < 200000; i++ {
		r := uint32(rng.Intn(1 << 18))
		if rng.Intn(3) == 0 && len(set) > 0 {
			// remove a random existing row
			for k := range set {
				b.Remove(k)
				delete(set, k)
				break
			}
		} else {
			b.Add(r)
			set[r] = struct{}{}
		}
		if i%50000 == 0 {
			checkBitmapRows(t, b, set)
		}
	}
	checkBitmapRows(t, b, set)
}

func TestSerializationRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(61))
	b := New()
	set := map[uint32]struct{}{}
	for i := 0; i < 60000; i++ {
		r := uint32(rng.Intn(1 << 20))
		b.Add(r)
		set[r] = struct{}{}
	}
	dir := t.TempDir()
	path := dir + "/test.pbm"
	if err := b.Serialize(path, 1<<20); err != nil {
		t.Fatal(err)
	}
	m, err := OpenMapped(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	mb := m.Bitmap()
	if mb.Cardinality() != b.Cardinality() {
		t.Fatalf("mapped card %d != %d", mb.Cardinality(), b.Cardinality())
	}
	checkBitmapRows(t, mb, set)
	// ops on mapped bitmaps
	c := mb.And(b)
	checkBitmapRows(t, c, set)
	for _, k := range []int{0, 10, 1000} {
		if mb.Select(k) != b.Select(k) {
			t.Fatalf("select mismatch at %d", k)
		}
	}
	var probe uint32 = 54321
	if mb.Rank(probe) != b.Rank(probe) {
		t.Fatalf("rank mismatch")
	}
}

func TestBitsetToRun(t *testing.T) {
	rng := rand.New(rand.NewSource(71))
	for rep := 0; rep < 200; rep++ {
		vals := randomVals(rng, shapes[rng.Intn(len(shapes))])
		b := newBitsetFromSorted(vals)
		rc := b.toRunContainer()
		if rc.Cardinality() != len(vals) {
			t.Fatalf("run card %d != %d", rc.Cardinality(), len(vals))
		}
		if !sameSlice(sortedValues(rc), vals) {
			t.Fatalf("run values mismatch")
		}
		if b.countRuns() != len(rc.runs) {
			t.Fatalf("countRuns %d != %d", b.countRuns(), len(rc.runs))
		}
	}
}

func sameSlice(a, b []uint16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func BenchmarkAndDense(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	x := containerOfShape(rng, "dense")
	y := containerOfShape(rng, "dense")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = opAnd(x, y)
	}
}

func ExampleStats() {
	rng := rand.New(rand.NewSource(1))
	bm := New()
	for i := 0; i < 1000; i++ {
		bm.Add(uint32(rng.Intn(1 << 20)))
	}
	s := bm.Stats()
	fmt.Printf("buckets=%d rows=%d\n", s.Buckets, s.Rows)
}
