package bitmap

import (
	"math/rand"
	"testing"
)

// TestMappedBitmapOpsMatchMemory checks that all four set operations, rank,
// select and range-card agree between the mmap'd and in-memory representations,
// and that a mapped bitmap can be combined with a compressed one.
func TestMappedBitmapOpsMatchMemory(t *testing.T) {
	rng := rand.New(rand.NewSource(401))
	a := New()
	b := New()
	for i := 0; i < 40000; i++ {
		a.Add(uint32(rng.Intn(1 << 20)))
		b.Add(uint32(rng.Intn(1 << 20)))
	}
	// give the file some run-heavy and array-heavy buckets too
	for r := uint32(100000); r < 103000; r++ {
		a.Add(r)
	}
	dir := t.TempDir()
	pa := dir + "/a.pbm"
	pb := dir + "/b.pbm"
	if err := a.Serialize(pa, 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := b.Serialize(pb, 1<<20); err != nil {
		t.Fatal(err)
	}
	ma, err := OpenMapped(pa)
	if err != nil {
		t.Fatal(err)
	}
	defer ma.Close()
	mb, err := OpenMapped(pb)
	if err != nil {
		t.Fatal(err)
	}
	defer mb.Close()

	check := func(name string, got, want *Bitmap) {
		t.Helper()
		gs, ws := got.ToSlice(), want.ToSlice()
		if len(gs) != len(ws) {
			t.Fatalf("%s: card %d != %d", name, len(gs), len(ws))
		}
		for i := range gs {
			if gs[i] != ws[i] {
				t.Fatalf("%s: row %d: %d != %d", name, i, gs[i], ws[i])
			}
		}
	}
	check("mapped and", ma.Bitmap().And(mb.Bitmap()), a.And(b))
	check("mapped or", ma.Bitmap().Or(mb.Bitmap()), a.Or(b))
	check("mapped xor", ma.Bitmap().Xor(mb.Bitmap()), a.Xor(b))
	check("mapped andnot", ma.Bitmap().AndNot(mb.Bitmap()), a.AndNot(b))
	check("mapped vs memory", ma.Bitmap().And(b), a.And(b))

	for i := 0; i < 100; i++ {
		row := uint32(rng.Intn(1 << 20))
		if ma.Bitmap().Rank(row) != a.Rank(row) {
			t.Fatalf("mapped rank %d mismatch", row)
		}
		lo := uint32(rng.Intn(1 << 20))
		hi := lo + uint32(rng.Intn(1<<19))
		if ma.Bitmap().RangeCard(lo, hi) != a.RangeCard(lo, hi) {
			t.Fatalf("mapped rangecard mismatch")
		}
	}
	for _, k := range []int{0, 1, 100, 1000, a.Cardinality() - 1} {
		if ma.Bitmap().Select(k) != a.Select(k) {
			t.Fatalf("mapped select %d mismatch", k)
		}
	}
	// mutation through a mapped container must materialize a writable copy
	mbm := ma.Bitmap()
	first := a.Min()
	mbm.Remove(first)
	if mbm.Contains(first) {
		t.Fatalf("remove on mapped bitmap had no effect")
	}
	if !a.Contains(first) {
		t.Fatalf("memory copy was mutated")
	}
}
