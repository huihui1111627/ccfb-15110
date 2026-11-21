package bitmap

import (
	"math/bits"
	"sort"
)

// WordBitmap is the same two-level scheme as Bitmap (bucket by high 16 bits),
// but every bucket is kept as 1024 dense uint64 words. It exists for boolean
// trees over bit-sliced planes, where the same buckets are touched again and
// again and re-choosing a container representation for every intermediate
// result would dominate the cost. Converting back to a compressed Bitmap
// happens once, at the end of the expression.
type WordBitmap struct {
	Keys []uint16
	Data [][]uint64
}

func NewWordBitmap() *WordBitmap { return &WordBitmap{} }

func (w *WordBitmap) search(key uint16) (int, bool) {
	i := sort.Search(len(w.Keys), func(i int) bool { return w.Keys[i] >= key })
	return i, i < len(w.Keys) && w.Keys[i] == key
}

func (w *WordBitmap) bucket(key uint16, create bool) ([]uint64, bool) {
	i, ok := w.search(key)
	if ok {
		return w.Data[i], true
	}
	if !create {
		return nil, false
	}
	w.Keys = append(w.Keys, 0)
	copy(w.Keys[i+1:], w.Keys[i:])
	w.Keys[i] = key
	w.Data = append(w.Data, nil)
	copy(w.Data[i+1:], w.Data[i:])
	w.Data[i] = make([]uint64, BitmapWords)
	return w.Data[i], true
}

func (w *WordBitmap) Add(row uint32) {
	words, _ := w.bucket(uint16(row>>16), true)
	words[(row&0xffff)>>6] |= uint64(1) << (row & 63)
}

func (w *WordBitmap) Remove(row uint32) {
	words, ok := w.bucket(uint16(row>>16), false)
	if !ok {
		return
	}
	words[(row&0xffff)>>6] &^= uint64(1) << (row & 63)
}

func (w *WordBitmap) Contains(row uint32) bool {
	words, ok := w.bucket(uint16(row>>16), false)
	if !ok {
		return false
	}
	return words[(row&0xffff)>>6]&(uint64(1)<<(row&63)) != 0
}

func (w *WordBitmap) Cardinality() int {
	n := 0
	for _, words := range w.Data {
		for _, x := range words {
			n += bits.OnesCount64(x)
		}
	}
	return n
}

func (w *WordBitmap) IsEmpty() bool { return w.Cardinality() == 0 }

func (w *WordBitmap) Clone() *WordBitmap {
	n := &WordBitmap{Keys: make([]uint16, len(w.Keys)), Data: make([][]uint64, len(w.Data))}
	copy(n.Keys, w.Keys)
	for i := range w.Data {
		n.Data[i] = make([]uint64, BitmapWords)
		copy(n.Data[i], w.Data[i])
	}
	return n
}

func (w *WordBitmap) ForEach(fn func(row uint32) bool) {
	for i, key := range w.Keys {
		base := uint32(key) << 16
		for wi, x := range w.Data[i] {
			rowBase := base | uint32(wi)<<6
			for x != 0 {
				t := bits.TrailingZeros64(x)
				if !fn(rowBase + uint32(t)) {
					return
				}
				x &= x - 1
			}
		}
	}
}

func (w *WordBitmap) ToSlice() []uint32 {
	out := make([]uint32, 0, w.Cardinality())
	w.ForEach(func(row uint32) bool { out = append(out, row); return true })
	return out
}

// FromBitmap converts a compressed bitmap into word form.
func FromBitmap(b *Bitmap) *WordBitmap {
	w := &WordBitmap{Keys: make([]uint16, len(b.Keys)), Data: make([][]uint64, len(b.Conts))}
	copy(w.Keys, b.Keys)
	for i, c := range b.Conts {
		words := make([]uint64, BitmapWords)
		switch x := c.(type) {
		case *BitsetContainer:
			copy(words, x.data)
		case *ArrayContainer:
			for _, v := range x.data {
				words[v>>6] |= uint64(1) << (v & 63)
			}
		case *RunContainer:
			for _, run := range x.runs {
				rangeOpBitset(words, run[0], run[1], true)
			}
		case *MMapContainer:
			src := FromBitmap(&Bitmap{Keys: []uint16{b.Keys[i]}, Conts: []Container{x.materialize()}})
			copy(words, src.Data[0])
		}
		w.Data[i] = words
	}
	return w
}

// ToBitmap compresses the word bitmap, choosing a container type per bucket
// from the bucket's cardinality and run count.
func (w *WordBitmap) ToBitmap() *Bitmap {
	b := &Bitmap{}
	for i, key := range w.Keys {
		card := 0
		for _, x := range w.Data[i] {
			card += bits.OnesCount64(x)
		}
		if card == 0 {
			continue
		}
		c := wordContainer(w.Data[i], card)
		b.Keys = append(b.Keys, key)
		b.Conts = append(b.Conts, c)
	}
	return b
}

// wordContainer builds the canonical container for a word bucket.
func wordContainer(words []uint64, card int) Container {
	if card <= MixedThreshold {
		vals := make([]uint16, 0, card)
		for wi, x := range words {
			base := uint16(wi * 64)
			for x != 0 {
				t := bits.TrailingZeros64(x)
				vals = append(vals, base+uint16(t))
				x &= x - 1
			}
		}
		return containerFromSorted(vals)
	}
	b := newBitsetContainer()
	copy(b.data, words)
	b.card = card
	// reuse the same selection rule as compressed op results
	if b.countRuns() <= runWordThreshold {
		return b.toRunContainer()
	}
	return b
}

// wordOp merges two word bitmaps bucket-wise. op receives the a-words (or nil)
// and b-words (or nil); it returns the result words and a per-word "has bits"
// mask so callers can avoid counting (and scanning) empty results.
type wordOp func(a, b []uint64) ([]uint64, []bool)

func hasAny(mask []bool) bool {
	for _, ok := range mask {
		if ok {
			return true
		}
	}
	return false
}

func countWords(words []uint64) int {
	n := 0
	for _, x := range words {
		n += bits.OnesCount64(x)
	}
	return n
}

var zeroWords = make([]uint64, BitmapWords)

func (w *WordBitmap) combine(other *WordBitmap, op wordOp, oneSided int) *WordBitmap {
	res := &WordBitmap{}
	i, j := 0, 0
	for i < len(w.Keys) || j < len(other.Keys) {
		switch {
		case j >= len(other.Keys) || (i < len(w.Keys) && w.Keys[i] < other.Keys[j]):
			if oneSided&1 != 0 {
				words, nonempty := op(w.Data[i], nil)
				if hasAny(nonempty) {
					res.Keys = append(res.Keys, w.Keys[i])
					res.Data = append(res.Data, words)
				}
			}
			i++
		case i >= len(w.Keys) || other.Keys[j] < w.Keys[i]:
			if oneSided&2 != 0 {
				words, nonempty := op(nil, other.Data[j])
				if hasAny(nonempty) {
					res.Keys = append(res.Keys, other.Keys[j])
					res.Data = append(res.Data, words)
				}
			}
			j++
		default:
			words, nonempty := op(w.Data[i], other.Data[j])
			if hasAny(nonempty) {
				res.Keys = append(res.Keys, w.Keys[i])
				res.Data = append(res.Data, words)
			}
			i++
			j++
		}
	}
	return res
}

func opWords(a, b []uint64, kind byte) ([]uint64, []bool) {
	out := make([]uint64, BitmapWords)
	nonempty := make([]bool, BitmapWords)
	if a == nil {
		a = zeroWords
	}
	if b == nil {
		b = zeroWords
	}
	switch kind {
	case '&':
		for k := range out {
			x := a[k] & b[k]
			out[k] = x
			nonempty[k] = x != 0
		}
	case '|':
		for k := range out {
			x := a[k] | b[k]
			out[k] = x
			nonempty[k] = x != 0
		}
	case '^':
		for k := range out {
			x := a[k] ^ b[k]
			out[k] = x
			nonempty[k] = x != 0
		}
	case '-':
		for k := range out {
			x := a[k] &^ b[k]
			out[k] = x
			nonempty[k] = x != 0
		}
	}
	return out, nonempty
}

// And returns the intersection.
func (w *WordBitmap) And(other *WordBitmap) *WordBitmap {
	return w.combine(other, func(a, b []uint64) ([]uint64, []bool) { return opWords(a, b, '&') }, 0)
}

// Or returns the union.
func (w *WordBitmap) Or(other *WordBitmap) *WordBitmap {
	return w.combine(other, func(a, b []uint64) ([]uint64, []bool) { return opWords(a, b, '|') }, 3)
}

// Xor returns the symmetric difference.
func (w *WordBitmap) Xor(other *WordBitmap) *WordBitmap {
	return w.combine(other, func(a, b []uint64) ([]uint64, []bool) { return opWords(a, b, '^') }, 3)
}

// AndNot returns w minus other.
func (w *WordBitmap) AndNot(other *WordBitmap) *WordBitmap {
	return w.combine(other, func(a, b []uint64) ([]uint64, []bool) { return opWords(a, b, '-') }, 1)
}

// SizeBytes reports the word footprint.
func (w *WordBitmap) SizeBytes() int {
	return 24*len(w.Keys) + len(w.Data)*8*BitmapWords
}

// LessThanWord evaluates "value < c" over the given planes using MSB-first
// word-level comparisons. planes[0] is the least significant bit.
func LessThanWord(planes []*WordBitmap, nonNull *WordBitmap, c uint64) *WordBitmap {
	width := len(planes)
	if width < 64 && c>>uint(width) != 0 {
		return nonNull.Clone()
	}
	equal := nonNull.Clone()
	res := NewWordBitmap()
	for i := width - 1; i >= 0 && !equal.IsEmpty(); i-- {
		plane := planes[i]
		if c&(1<<uint(i)) != 0 {
			res = res.Or(equal.AndNot(plane))
			equal = equal.And(plane)
		} else {
			equal = equal.AndNot(plane)
		}
	}
	return res
}
