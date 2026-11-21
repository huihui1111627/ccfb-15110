package bitmap

import (
	"math"
	"sort"
)

// Bitmap is a compressed bitmap of 32-bit row ids. Rows are bucketed by their
// high 16 bits; each non-empty bucket holds one container for the low 16 bits.
type Bitmap struct {
	Keys  []uint16
	Conts []Container
}

func New() *Bitmap { return &Bitmap{} }

func (b *Bitmap) Len() int { return len(b.Keys) }

// search returns the index of key and whether it exists.
func (b *Bitmap) search(key uint16) (int, bool) {
	i := sort.Search(len(b.Keys), func(i int) bool { return b.Keys[i] >= key })
	return i, i < len(b.Keys) && b.Keys[i] == key
}

// KeysSorted reports whether the bucket keys are in ascending order.
func (b *Bitmap) KeysSorted() bool {
	return sort.SliceIsSorted(b.Keys, func(i, j int) bool { return b.Keys[i] < b.Keys[j] })
}

func (b *Bitmap) container(key uint16) (Container, bool) {
	i, ok := b.search(key)
	if !ok {
		return nil, false
	}
	return b.Conts[i], true
}

func (b *Bitmap) insertAt(i int, key uint16, c Container) {
	b.Keys = append(b.Keys, 0)
	copy(b.Keys[i+1:], b.Keys[i:])
	b.Keys[i] = key
	b.Conts = append(b.Conts, nil)
	copy(b.Conts[i+1:], b.Conts[i:])
	b.Conts[i] = c
}

func (b *Bitmap) removeAt(i int) {
	b.Keys = append(b.Keys[:i], b.Keys[i+1:]...)
	b.Conts = append(b.Conts[:i], b.Conts[i+1:]...)
}

func (b *Bitmap) Cardinality() int {
	n := 0
	for _, c := range b.Conts {
		n += c.Cardinality()
	}
	return n
}

func (b *Bitmap) IsEmpty() bool { return len(b.Keys) == 0 }

func (b *Bitmap) Contains(row uint32) bool {
	c, ok := b.container(uint16(row >> 16))
	if !ok {
		return false
	}
	return c.Contains(uint16(row))
}

// Add inserts a row. The container type may change as a result.
func (b *Bitmap) Add(row uint32) {
	key := uint16(row >> 16)
	low := uint16(row)
	i, ok := b.search(key)
	if !ok {
		a := &ArrayContainer{data: []uint16{low}}
		b.insertAt(i, key, a)
		return
	}
	b.Conts[i] = b.Conts[i].applyAdd(low)
}

// Remove deletes a row. Empty buckets are dropped.
func (b *Bitmap) Remove(row uint32) {
	key := uint16(row >> 16)
	low := uint16(row)
	i, ok := b.search(key)
	if !ok {
		return
	}
	c := b.Conts[i].applyRemove(low)
	if c.Cardinality() == 0 {
		b.removeAt(i)
		return
	}
	b.Conts[i] = c
}

// SetRange sets or clears [lo,hi] (inclusive) in one pass, touching only the
// buckets the range spans.
func (b *Bitmap) SetRange(lo, hi uint32, set bool) {
	if lo > hi {
		return
	}
	first := uint32(lo >> 16)
	last := uint32(hi >> 16)
	for k := first; ; k++ {
		key := uint16(k)
		blo := uint16(0)
		bhi := uint16(0xffff)
		if k == first {
			blo = uint16(lo)
		}
		if k == last {
			bhi = uint16(hi)
		}
		i, ok := b.search(key)
		if !ok {
			if set {
				b.insertAt(i, key, containerFromRange(blo, bhi))
			}
		} else {
			c := b.Conts[i].applyRange(blo, bhi, set)
			if c.Cardinality() == 0 {
				b.removeAt(i)
			} else {
				b.Conts[i] = c
			}
		}
		if k == last {
			break
		}
		if k == math.MaxUint16 {
			break
		}
	}
}

// FlipRange complements [lo,hi] in one pass, touching only the buckets the
// range spans.
func (b *Bitmap) FlipRange(lo, hi uint32) {
	if lo > hi {
		return
	}
	first := uint32(lo >> 16)
	last := uint32(hi >> 16)
	for k := first; ; k++ {
		key := uint16(k)
		blo := uint16(0)
		bhi := uint16(0xffff)
		if k == first {
			blo = uint16(lo)
		}
		if k == last {
			bhi = uint16(hi)
		}
		i, ok := b.search(key)
		if !ok {
			b.insertAt(i, key, containerFromRange(blo, bhi))
		} else {
			b.Conts[i] = flipRange(b.Conts[i], blo, bhi)
		}
		if k == last {
			break
		}
		if k == math.MaxUint16 {
			break
		}
	}
}

func containerFromRange(lo, hi uint16) Container {
	return canonicalRun(&RunContainer{runs: [][2]uint16{{lo, hi}}})
}

// RowCount returns the number of rows <= row (i.e. the rank of row), or the
// total cardinality when row is math.MaxUint32.
func (b *Bitmap) Rank(row uint32) int {
	key := uint16(row >> 16)
	low := uint16(row)
	n := 0
	for i, k := range b.Keys {
		if k < key {
			n += b.Conts[i].Cardinality()
			continue
		}
		if k == key {
			n += b.Conts[i].Rank(low)
		}
		break
	}
	return n
}

// Select returns the k-th smallest row (0-based).
func (b *Bitmap) Select(k int) uint32 {
	for i, c := range b.Conts {
		card := c.Cardinality()
		if k < card {
			return uint32(b.Keys[i])<<16 | uint32(c.Select(k))
		}
		k -= card
	}
	panic("bitmap.Select: index out of range")
}

// RangeCard counts rows in [lo,hi].
func (b *Bitmap) RangeCard(lo, hi uint32) int {
	if lo > hi {
		return 0
	}
	first := uint32(lo >> 16)
	last := uint32(hi >> 16)
	n := 0
	i := sort.Search(len(b.Keys), func(i int) bool { return uint32(b.Keys[i]) >= first })
	for ; i < len(b.Keys); i++ {
		key := uint32(b.Keys[i])
		if key > last {
			break
		}
		blo := uint16(0)
		bhi := uint16(0xffff)
		if key == first {
			blo = uint16(lo)
		}
		if key == last {
			bhi = uint16(hi)
		}
		n += b.Conts[i].RangeCard(blo, bhi)
	}
	return n
}

// ForEach visits rows in increasing order.
func (b *Bitmap) ForEach(fn func(row uint32) bool) {
	for i, c := range b.Conts {
		base := uint32(b.Keys[i]) << 16
		ok := true
		c.ForEach(func(v uint16) bool {
			if !fn(base | uint32(v)) {
				ok = false
				return false
			}
			return true
		})
		if !ok {
			return
		}
	}
}

// ToSlice materializes all rows. Used by tests and the API's sample output.
func (b *Bitmap) ToSlice() []uint32 {
	out := make([]uint32, 0, b.Cardinality())
	b.ForEach(func(row uint32) bool { out = append(out, row); return true })
	return out
}

// Min returns the smallest row (requires a non-empty bitmap).
func (b *Bitmap) Min() uint32 {
	if len(b.Keys) == 0 {
		panic("Min on empty bitmap")
	}
	return uint32(b.Keys[0])<<16 | uint32(b.Conts[0].Min())
}

// Max returns the largest row (requires a non-empty bitmap).
func (b *Bitmap) Max() uint32 {
	n := len(b.Keys) - 1
	if n < 0 {
		panic("Max on empty bitmap")
	}
	return uint32(b.Keys[n])<<16 | uint32(b.Conts[n].Max())
}

// Clone deep-copies the bitmap.
func (b *Bitmap) Clone() *Bitmap {
	n := &Bitmap{Keys: make([]uint16, len(b.Keys)), Conts: make([]Container, len(b.Conts))}
	copy(n.Keys, b.Keys)
	for i, c := range b.Conts {
		n.Conts[i] = c.DeepCopy()
	}
	return n
}

// ---------- set operations between bitmaps ----------

// ensureMemory converts read-only mmap containers into in-memory ones so the
// regular op implementations can be used.
func ensureMemory(c Container) Container {
	if m, ok := c.(*MMapContainer); ok {
		return m.materialize()
	}
	return c
}

// opBitmaps merges two bitmaps bucket by bucket. oneSided says what to do with
// buckets present in only one operand: 0 = drop, 1 = keep a's, 2 = keep b's,
// 3 = keep both (union/xor semantics).
func opBitmaps(a, b *Bitmap, op func(Container, Container) Container, oneSided int) *Bitmap {
	res := &Bitmap{}
	i, j := 0, 0
	for i < len(a.Keys) || j < len(b.Keys) {
		switch {
		case j >= len(b.Keys) || (i < len(a.Keys) && a.Keys[i] < b.Keys[j]):
			if oneSided&1 != 0 {
				res.Keys = append(res.Keys, a.Keys[i])
				res.Conts = append(res.Conts, a.Conts[i].DeepCopy())
			}
			i++
		case i >= len(a.Keys) || b.Keys[j] < a.Keys[i]:
			if oneSided&2 != 0 {
				res.Keys = append(res.Keys, b.Keys[j])
				res.Conts = append(res.Conts, b.Conts[j].DeepCopy())
			}
			j++
		default:
			c := op(a.Conts[i], b.Conts[j])
			if c.Cardinality() > 0 {
				res.Keys = append(res.Keys, a.Keys[i])
				res.Conts = append(res.Conts, c)
			}
			i++
			j++
		}
	}
	return res
}

// And returns the intersection.
func (b *Bitmap) And(other *Bitmap) *Bitmap {
	return opBitmaps(b, other, opAnd, 0)
}

// Or returns the union.
func (b *Bitmap) Or(other *Bitmap) *Bitmap {
	return opBitmaps(b, other, opOr, 3)
}

// Xor returns the symmetric difference.
func (b *Bitmap) Xor(other *Bitmap) *Bitmap {
	return opBitmaps(b, other, opXor, 3)
}

// AndNot returns b minus other.
func (b *Bitmap) AndNot(other *Bitmap) *Bitmap {
	return opBitmaps(b, other, opAndNot, 1)
}

// ---------- introspection ----------

// Stats summarizes the bucket/container composition of a bitmap.
type Stats struct {
	Buckets  int `json:"buckets"`
	Arrays   int `json:"arrays"`
	Bitsets  int `json:"bitsets"`
	Runs     int `json:"runs"`
	Rows     int `json:"rows"`
	SizeByte int `json:"sizeByte"`
}

func (b *Bitmap) Stats() Stats {
	// keys: 2B, container interface: 16B (on 64-bit), plus the container itself
	s := Stats{Buckets: len(b.Keys), SizeByte: 24 + 18*len(b.Keys)}
	for _, c := range b.Conts {
		switch c.Kind() {
		case Array:
			s.Arrays++
		case Bitset:
			s.Bitsets++
		case Run:
			s.Runs++
		}
		s.Rows += c.Cardinality()
		s.SizeByte += ContainerBytes(c)
	}
	return s
}

// ContainerBytes returns the exact payload bytes of a container as it would be
// written to disk (arrays 2B/value, bitsets 8KB, runs 4B/run).
func ContainerBytes(c Container) int {
	switch x := c.(type) {
	case *ArrayContainer:
		return 2 * len(x.data)
	case *BitsetContainer:
		return 8 * BitmapWords
	case *RunContainer:
		return 4 * len(x.runs)
	case *MMapContainer:
		return x.SizeBytes()
	}
	return 0
}

// ContainerSize is the approximate in-memory footprint of a container.
func ContainerSize(c Container) int {
	switch x := c.(type) {
	case *ArrayContainer:
		return 24 + 2*len(x.data)
	case *BitsetContainer:
		return 8 + BitmapWords*8
	case *RunContainer:
		return 24 + 4*len(x.runs)
	case *MMapContainer:
		return x.SizeBytes()
	}
	return 0
}

// Validate checks the structural invariants (sorted keys, no empty containers,
// sorted unique container contents).
func (b *Bitmap) Validate() error {
	for i, k := range b.Keys {
		if i > 0 && b.Keys[i-1] >= k {
			return errf("keys not strictly increasing at %d", i)
		}
		c := b.Conts[i]
		if c == nil {
			return errf("nil container at key %d", k)
		}
		if c.Cardinality() == 0 {
			return errf("empty container at key %d", k)
		}
		switch x := c.(type) {
		case *ArrayContainer:
			for j := 1; j < len(x.data); j++ {
				if x.data[j-1] >= x.data[j] {
					return errf("array not sorted at key %d", k)
				}
			}
		case *RunContainer:
			if len(x.runs) == 0 {
				return errf("empty run at key %d", k)
			}
			for j, run := range x.runs {
				if run[0] > run[1] {
					return errf("inverted run at key %d", k)
				}
				if j > 0 && int(x.runs[j-1][1])+1 >= int(run[0]) {
					return errf("overlapping runs at key %d", k)
				}
			}
			if n := x.Cardinality(); (n > MixedThreshold) != (len(x.runs) <= runWordThreshold) {
				// Only a soft invariant: canonicalRun keeps large+few-runs as run.
				_ = n
			}
		}
	}
	return nil
}
