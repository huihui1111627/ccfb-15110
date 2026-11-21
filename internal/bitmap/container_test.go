package bitmap

import (
	"math/rand"
	"sort"
	"testing"
)

// TestApplyAddRemoveAgainstSet hammers each container type through random
// single-element edits and compares against the reference set after each edit.
func TestApplyAddRemoveAgainstSet(t *testing.T) {
	rng := rand.New(rand.NewSource(301))
	for _, k := range []Kind{Array, Bitset, Run} {
		vals := randomVals(rng, shapes[rng.Intn(len(shapes))])
		var c Container = forceKind(vals, k)
		want := bruteOf(c)
		for step := 0; step < 4000; step++ {
			v := uint16(rng.Intn(65536))
			if rng.Intn(2) == 0 {
				c = c.applyAdd(v)
				want[v] = struct{}{}
			} else {
				c = c.applyRemove(v)
				delete(want, v)
			}
			got := bruteOf(c)
			if len(got) != len(want) {
				t.Fatalf("%v step %d: card %d != %d", k, step, len(got), len(want))
			}
			for x := range want {
				if _, ok := got[x]; !ok {
					t.Fatalf("%v step %d: missing %d", k, step, x)
				}
			}
			// container invariant: type must follow chooseKind
			if c.Cardinality() > 0 {
				switch x := c.(type) {
				case *ArrayContainer:
					if len(x.data) > MixedThreshold {
						t.Fatalf("array over threshold")
					}
					for i := 1; i < len(x.data); i++ {
						if x.data[i-1] >= x.data[i] {
							t.Fatalf("array unsorted")
						}
					}
				case *RunContainer:
					for i, r := range x.runs {
						if r[0] > r[1] {
							t.Fatalf("inverted run")
						}
						if i > 0 && int(x.runs[i-1][1])+1 >= int(r[0]) {
							t.Fatalf("overlapping runs %v %v", x.runs[i-1], r)
						}
					}
				}
			}
		}
	}
}

// TestQuickStats checks rank/select/range-card on every container type directly.
func TestQuickStats(t *testing.T) {
	rng := rand.New(rand.NewSource(303))
	for _, k := range []Kind{Array, Bitset, Run} {
		for rep := 0; rep < 20; rep++ {
			vals := randomVals(rng, shapes[rng.Intn(len(shapes))])
			if len(vals) == 0 {
				continue
			}
			c := forceKind(vals, k)
			sorted := append([]uint16{}, vals...)
			sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
			if c.Min() != sorted[0] || c.Max() != sorted[len(sorted)-1] {
				t.Fatalf("%v min/max wrong", k)
			}
			for i := 0; i < 20; i++ {
				v := uint16(rng.Intn(65536))
				want := sort.Search(len(sorted), func(j int) bool { return sorted[j] > v })
				if c.Rank(v) != want {
					t.Fatalf("%v rank(%d)=%d want %d", k, v, c.Rank(v), want)
				}
				kidx := rng.Intn(len(sorted))
				if c.Select(kidx) != sorted[kidx] {
					t.Fatalf("%v select(%d) wrong", k, kidx)
				}
				lo := uint16(rng.Intn(65536))
				hi := uint16(lo + uint16(rng.Intn(65536-int(lo))))
				cnt := 0
				for _, x := range sorted {
					if x >= lo && x <= hi {
						cnt++
					}
				}
				if c.RangeCard(lo, hi) != cnt {
					t.Fatalf("%v rangeCard(%d,%d)=%d want %d", k, lo, hi, c.RangeCard(lo, hi), cnt)
				}
				if c.Contains(v) != (sort.Search(len(sorted), func(j int) bool { return sorted[j] >= v }) < len(sorted) && sorted[sort.Search(len(sorted), func(j int) bool { return sorted[j] >= v })] == v) {
					t.Fatalf("%v contains(%d) wrong", k, v)
				}
			}
		}
	}
}
