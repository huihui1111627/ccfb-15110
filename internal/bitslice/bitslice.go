package bitslice

import (
	"pianoindex/internal/bitmap"
)

// Slice is a group of bit-sliced bitmaps: bit i of every value is stored in
// Planes[i]. Range predicates are evaluated by comparing the query value with
// every stored value bit by bit, from the most significant bit down, using only
// bitmap operations — no per-row loop.
type Slice struct {
	Width  int
	Planes []*bitmap.Bitmap
	// NonNull marks rows that have a value at all (NULL handling).
	NonNull *bitmap.Bitmap
}

func New(width int) *Slice {
	s := &Slice{Width: width}
	for i := 0; i < width; i++ {
		s.Planes = append(s.Planes, bitmap.New())
	}
	s.NonNull = bitmap.New()
	return s
}

// Add sets row's value. val must fit in Width bits.
func (s *Slice) Add(row uint32, val uint64) {
	s.NonNull.Add(row)
	for i := 0; i < s.Width; i++ {
		if val&(1<<uint(i)) != 0 {
			s.Planes[i].Add(row)
		}
	}
}

// Remove clears row's value.
func (s *Slice) Remove(row uint32) {
	s.NonNull.Remove(row)
	for i := 0; i < s.Width; i++ {
		s.Planes[i].Remove(row)
	}
}

// Update replaces the value of an existing row.
func (s *Slice) Update(row uint32, oldVal, newVal uint64) {
	for i := 0; i < s.Width; i++ {
		ob := oldVal&(1<<uint(i)) != 0
		nb := newVal&(1<<uint(i)) != 0
		switch {
		case ob && !nb:
			s.Planes[i].Remove(row)
		case !ob && nb:
			s.Planes[i].Add(row)
		}
	}
}

// ValueOf reconstructs a row's value; ok is false when the row is null.
func (s *Slice) ValueOf(row uint32) (uint64, bool) {
	if !s.NonNull.Contains(row) {
		return 0, false
	}
	var v uint64
	for i := 0; i < s.Width; i++ {
		if s.Planes[i].Contains(row) {
			v |= 1 << uint(i)
		}
	}
	return v, true
}

// lesser returns the rows whose value is strictly less than c.
//
// It runs the MSB-first comparison in word form and compresses once at the end:
// doing so avoids re-choosing container representations for every intermediate
// AND/OR of a bit-sliced comparison.
func (s *Slice) lesser(c uint64) *bitmap.Bitmap {
	// Values outside the representable width: everything is smaller.
	if s.Width < 64 && c>>uint(s.Width) != 0 {
		return s.NonNull.Clone()
	}
	equal := bitmap.FromBitmap(s.NonNull)
	res := bitmap.NewWordBitmap()
	nonEmpty := true
	for i := s.Width - 1; i >= 0; i-- {
		plane := bitmap.FromBitmap(s.Planes[i])
		if c&(1<<uint(i)) != 0 {
			// Rows with 0 here are strictly smaller than c; keep the 1s as ties.
			res = res.Or(equal.AndNot(plane))
			equal = equal.And(plane)
		} else {
			equal = equal.AndNot(plane)
		}
		if nonEmpty && equal.Cardinality() == 0 {
			nonEmpty = false
			break
		}
	}
	return res.ToBitmap()
}

// LessThan returns rows with value < c.
func (s *Slice) LessThan(c uint64) *bitmap.Bitmap { return s.lesser(c) }

// LessEqual returns rows with value <= c.
func (s *Slice) LessEqual(c uint64) *bitmap.Bitmap {
	if c == ^uint64(0) {
		return s.NonNull.Clone()
	}
	return s.lesser(c + 1)
}

// Greater returns rows with value > c.
func (s *Slice) Greater(c uint64) *bitmap.Bitmap {
	if c == ^uint64(0) {
		return bitmap.New()
	}
	return s.NonNull.AndNot(s.lesser(c + 1))
}

// GreaterEqual returns rows with value >= c.
func (s *Slice) GreaterEqual(c uint64) *bitmap.Bitmap {
	return s.NonNull.AndNot(s.lesser(c))
}

// Equal returns rows whose value equals c.
func (s *Slice) Equal(c uint64) *bitmap.Bitmap {
	if s.Width < 64 && c>>uint(s.Width) != 0 {
		return bitmap.New() // no representable value can match
	}
	res := s.NonNull.Clone()
	for i := 0; i < s.Width; i++ {
		if c&(1<<uint(i)) != 0 {
			res = res.And(s.Planes[i])
		} else {
			res = res.AndNot(s.Planes[i])
		}
		if res.IsEmpty() {
			return res
		}
	}
	return res
}

// NotEqual returns rows whose value differs from c.
func (s *Slice) NotEqual(c uint64) *bitmap.Bitmap {
	return s.NonNull.AndNot(s.Equal(c))
}

// Stats reports the memory footprint of the slice.
func (s *Slice) Stats() int {
	n := 0
	for _, p := range s.Planes {
		n += p.Stats().SizeByte
	}
	n += s.NonNull.Stats().SizeByte
	return n
}

// Bitmaps returns all non-null planes plus the null bitmap, for serialization.
func (s *Slice) Bitmaps() []*bitmap.Bitmap {
	out := make([]*bitmap.Bitmap, 0, s.Width+1)
	out = append(out, s.Planes...)
	out = append(out, s.NonNull)
	return out
}
