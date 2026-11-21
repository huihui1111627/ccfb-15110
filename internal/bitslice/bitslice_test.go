package bitslice

import (
	"math/rand"
	"sort"
	"testing"
)

func TestRangePredicatesMatchRowScan(t *testing.T) {
	rng := rand.New(rand.NewSource(201))
	for width := 1; width <= 12; width++ {
		maxv := uint64(1)<<uint(width) - 1
		s := New(width)
		vals := map[uint32]uint64{}
		for row := uint32(0); row < 3000; row++ {
			v := uint64(rng.Int63n(int64(maxv + 1)))
			s.Add(row, v)
			vals[row] = v
		}
		var probes []uint64
		for i := 0; i < 40; i++ {
			probes = append(probes, uint64(rng.Int63n(int64(maxv+2))))
		}
		probes = append(probes, 0, maxv, maxv+1)
		for _, c := range probes {
			check := func(name string, got map[uint32]bool, pred func(uint64) bool) {
				want := 0
				for _, v := range vals {
					if pred(v) {
						want++
					}
				}
				if len(got) != want {
					t.Fatalf("width %d %s(%d): got %d want %d", width, name, c, len(got), want)
				}
				for row, v := range vals {
					if pred(v) != got[row] {
						t.Fatalf("width %d %s(%d) row %d v=%d", width, name, c, row, v)
					}
				}
			}
			collect := func(b interface{ ToSlice() []uint32 }) map[uint32]bool {
				m := map[uint32]bool{}
				for _, r := range b.ToSlice() {
					m[r] = true
				}
				return m
			}
			check("lt", collect(s.LessThan(c)), func(v uint64) bool { return v < c })
			check("le", collect(s.LessEqual(c)), func(v uint64) bool { return v <= c })
			check("gt", collect(s.Greater(c)), func(v uint64) bool { return v > c })
			check("ge", collect(s.GreaterEqual(c)), func(v uint64) bool { return v >= c })
			check("eq", collect(s.Equal(c)), func(v uint64) bool { return v == c })
			check("ne", collect(s.NotEqual(c)), func(v uint64) bool { return v != c })
		}
	}
}

func TestValueOfAndUpdate(t *testing.T) {
	s := New(10)
	for row := uint32(0); row < 500; row++ {
		s.Add(row, uint64(row%1000))
	}
	for row := uint32(0); row < 500; row++ {
		v, ok := s.ValueOf(row)
		if !ok || v != uint64(row%1000) {
			t.Fatalf("row %d value %d ok=%v", row, v, ok)
		}
	}
	// flip every third row's bit planes and verify
	for row := uint32(0); row < 500; row += 3 {
		old := uint64(row % 1000)
		s.Update(row, old, 777)
		s.Update(row, 777, old)
	}
	rows := s.NonNull.ToSlice()
	sort.Slice(rows, func(i, j int) bool { return rows[i] < rows[j] })
	if len(rows) != 500 {
		t.Fatalf("non-null count %d", len(rows))
	}
	for row := uint32(0); row < 500; row++ {
		if v, ok := s.ValueOf(row); !ok || v != uint64(row%1000) {
			t.Fatalf("after update row %d value %d ok=%v", row, v, ok)
		}
	}
}
