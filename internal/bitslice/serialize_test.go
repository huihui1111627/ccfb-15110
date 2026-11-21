package bitslice

import (
	"testing"
)

func TestMappedColumnQueriesInPlace(t *testing.T) {
	s := New(9)
	for row := uint32(0); row < 50000; row++ {
		if row%10 == 0 {
			s.Add(row, uint64(row%300))
		}
	}
	dir := t.TempDir()
	if err := SaveColumn(s, dir, "tuned", 50000); err != nil {
		t.Fatal(err)
	}
	mc, err := OpenColumn(dir, "tuned")
	if err != nil {
		t.Fatal(err)
	}
	defer mc.Close()
	ms := mc.Slice()
	for _, c := range []uint64{0, 1, 50, 150, 299, 300, 400} {
		if got, want := ms.LessThan(c).Cardinality(), s.LessThan(c).Cardinality(); got != want {
			t.Fatalf("mapped lt(%d)=%d want %d", c, got, want)
		}
		if got, want := ms.GreaterEqual(c).Cardinality(), s.GreaterEqual(c).Cardinality(); got != want {
			t.Fatalf("mapped ge(%d)=%d want %d", c, got, want)
		}
		if got, want := ms.Equal(c).Cardinality(), s.Equal(c).Cardinality(); got != want {
			t.Fatalf("mapped eq(%d)=%d want %d", c, got, want)
		}
	}
}
