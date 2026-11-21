package bitmap

import (
	"math/rand"
	"sort"
	"testing"
)

// TestRangeTouchesOnlySpannedBuckets checks that SetRange/FlipRange leave
// buckets outside the range bit-for-bit untouched (pointer identity preserved).
func TestRangeTouchesOnlySpannedBuckets(t *testing.T) {
	rng := rand.New(rand.NewSource(101))
	for rep := 0; rep < 50; rep++ {
		b := New()
		for i := 0; i < 20000; i++ {
			b.Add(uint32(rng.Intn(1 << 21)))
		}
		lo := uint32(rng.Intn(1 << 21))
		hi := lo + uint32(rng.Intn(1<<17))
		first := lo >> 16
		last := hi >> 16
		before := map[uint16]Container{}
		for i, k := range b.Keys {
			if uint32(k) < first || uint32(k) > last {
				before[k] = b.Conts[i]
			}
		}
		b.FlipRange(lo, hi)
		for k, c := range before {
			i, ok := b.search(k)
			if !ok {
				t.Fatalf("bucket %d vanished", k)
			}
			if b.Conts[i] != c {
				t.Fatalf("bucket %d outside range was rewritten", k)
			}
		}
	}
}

// TestRangeExactnessAcrossBuckets verifies range set/clear/flip against brute
// force when the range spans many buckets, including full buckets.
func TestRangeExactnessAcrossBuckets(t *testing.T) {
	rng := rand.New(rand.NewSource(103))
	for rep := 0; rep < 40; rep++ {
		b := New()
		brute := map[uint32]bool{}
		for i := 0; i < 30000; i++ {
			r := uint32(rng.Intn(1 << 20))
			b.Add(r)
			brute[r] = true
		}
		lo := uint32(rng.Intn(1 << 19))
		hi := lo + uint32(rng.Intn(1<<19)) + 65536
		if hi > 1<<20 {
			hi = 1 << 20
		}
		for r := lo; r <= hi; r++ {
			brute[r] = true
		}
		b.SetRange(lo, hi, true)
		checkBrute(t, b, brute)
		for r := lo; r <= hi; r++ {
			delete(brute, r)
		}
		b.SetRange(lo, hi, false)
		checkBrute(t, b, brute)
		for r := lo; r <= hi; r++ {
			if brute[r] {
				delete(brute, r)
			} else {
				brute[r] = true
			}
		}
		b.FlipRange(lo, hi)
		checkBrute(t, b, brute)
	}
}

func checkBrute(t *testing.T, b *Bitmap, want map[uint32]bool) {
	t.Helper()
	rows := b.ToSlice()
	if len(rows) != len(want) {
		t.Fatalf("card %d != %d", len(rows), len(want))
	}
	for _, r := range rows {
		if !want[r] {
			t.Fatalf("stray %d", r)
		}
	}
}

// TestPopcountRankSelectNoExpansion checks rank/select/range-card agree with the
// materialized set on real-world-ish skewed data.
func TestPopcountRankSelectNoExpansion(t *testing.T) {
	rng := rand.New(rand.NewSource(107))
	b := New()
	for i := 0; i < 200000; i++ {
		// skew rows so some buckets are dense, some sparse, some runny
		var r uint32
		switch i % 3 {
		case 0:
			r = uint32(rng.Intn(1 << 16))
		case 1:
			r = uint32(50<<16) + uint32(rng.Intn(200)) + uint32(rng.Intn(40)*100)
		default:
			r = uint32(rng.Intn(1 << 20))
		}
		b.Add(r)
	}
	sorted := b.ToSlice()
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	if b.Cardinality() != len(sorted) {
		t.Fatalf("card mismatch")
	}
	for i := 0; i < 200; i++ {
		k := rng.Intn(len(sorted))
		if got := b.Select(k); got != sorted[k] {
			t.Fatalf("select %d = %d want %d", k, got, sorted[k])
		}
		probe := uint32(rng.Intn(1 << 20))
		want := sort.Search(len(sorted), func(i int) bool { return sorted[i] > probe })
		if got := b.Rank(probe); got != want {
			t.Fatalf("rank(%d) = %d want %d", probe, got, want)
		}
		lo := uint32(rng.Intn(1 << 20))
		hi := lo + uint32(rng.Intn(1<<19))
		cnt := 0
		for _, r := range sorted {
			if r >= lo && r <= hi {
				cnt++
			}
		}
		if got := b.RangeCard(lo, hi); got != cnt {
			t.Fatalf("rangeCard(%d,%d) = %d want %d", lo, hi, got, cnt)
		}
	}
}
