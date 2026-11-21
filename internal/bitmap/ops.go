package bitmap

import (
	"math/bits"
	"sort"
)

// This file holds the pairwise container operations. Every ordered pair of
// representations gets its own implementation for AND, OR, XOR and ANDNOT.
// The result representation is picked from the result's cardinality and run
// count (see containerFromSorted / canonicalRun / finishBitset).

// finishBitset picks the representation for a freshly computed bitset.
func finishBitset(b *BitsetContainer) Container {
	if b.card == 0 {
		return &ArrayContainer{}
	}
	switch chooseKind(b.card, b.countRuns()) {
	case Run:
		return b.toRunContainer()
	case Array:
		return &ArrayContainer{data: bitsetValues(b)}
	default:
		return b
	}
}

func bitsetValues(b *BitsetContainer) []uint16 {
	out := make([]uint16, 0, b.card)
	b.ForEach(func(v uint16) bool { out = append(out, v); return true })
	return out
}

// ---------- AND ----------

// canonResult reduces an empty result to the canonical empty container and
// otherwise leaves the operation's own choice (which is based on cardinality
// and run count) untouched.
func canonResult(c Container) Container {
	if c.Cardinality() == 0 {
		return &ArrayContainer{}
	}
	return c
}

func opAnd(a, b Container) Container {
	switch x := a.(type) {
	case *ArrayContainer:
		switch y := b.(type) {
		case *ArrayContainer:
			return andArrayArray(x, y)
		case *BitsetContainer:
			return andArrayBitset(x, y)
		case *RunContainer:
			return andArrayRun(x, y)
		case *MMapContainer:
			return opAnd(x, y.materialize())
		}
	case *BitsetContainer:
		switch y := b.(type) {
		case *ArrayContainer:
			return andArrayBitset(y, x)
		case *BitsetContainer:
			return andBitsetBitset(x, y)
		case *RunContainer:
			return andBitsetRun(x, y)
		case *MMapContainer:
			return opAnd(x, y.materialize())
		}
	case *RunContainer:
		switch y := b.(type) {
		case *ArrayContainer:
			return andArrayRun(y, x)
		case *BitsetContainer:
			return andBitsetRun(y, x)
		case *RunContainer:
			return canonResult(canonicalRun(andRunRunRaw(x, y)))
		case *MMapContainer:
			return opAnd(x, y.materialize())
		}
	case *MMapContainer:
		return opAnd(x.materialize(), b)
	}
	panic("opAnd: unreachable")
}

func andArrayArray(a, b *ArrayContainer) Container {
	out := make([]uint16, 0, minInt(len(a.data), len(b.data)))
	i, j := 0, 0
	for i < len(a.data) && j < len(b.data) {
		switch {
		case a.data[i] < b.data[j]:
			i++
		case a.data[i] > b.data[j]:
			j++
		default:
			out = append(out, a.data[i])
			i++
			j++
		}
	}
	return finishSorted(out)
}

func andArrayBitset(a *ArrayContainer, b *BitsetContainer) Container {
	out := make([]uint16, 0, len(a.data))
	for _, v := range a.data {
		if b.Contains(v) {
			out = append(out, v)
		}
	}
	return finishSorted(out)
}

func andArrayRun(a *ArrayContainer, b *RunContainer) Container {
	out := make([]uint16, 0, len(a.data))
	ri := 0
	for _, v := range a.data {
		for ri < len(b.runs) && b.runs[ri][1] < v {
			ri++
		}
		if ri == len(b.runs) {
			break
		}
		if b.runs[ri][0] <= v {
			out = append(out, v)
		}
	}
	return finishSorted(out)
}

func andBitsetBitset(a, b *BitsetContainer) Container {
	res := newBitsetContainer()
	card := 0
	for i := range res.data {
		w := a.data[i] & b.data[i]
		res.data[i] = w
		card += bits.OnesCount64(w)
	}
	res.card = card
	return finishBitset(res)
}

// runMaskWord returns the bits of [lo,hi] that fall into the 64-bit word whose
// first bit is base.
func runMaskWord(lo, hi uint16, base int) uint64 {
	mask := ^uint64(0)
	if int(lo) > base {
		mask &= ^uint64(0) << uint(int(lo)-base)
	}
	if int(hi) < base+63 {
		mask &= ^uint64(0) >> uint(63-(int(hi)-base))
	}
	return mask
}

func andBitsetRun(a *BitsetContainer, b *RunContainer) Container {
	res := newBitsetContainer()
	card := 0
	ri := 0
	for i := range res.data {
		w := a.data[i]
		if w == 0 {
			continue
		}
		base := i * 64
		var keep uint64
		for ri < len(b.runs) && int(b.runs[ri][1]) < base {
			ri++
		}
		for k := ri; k < len(b.runs) && int(b.runs[k][0]) <= base+63; k++ {
			keep |= runMaskWord(b.runs[k][0], b.runs[k][1], base)
		}
		w &= keep
		res.data[i] = w
		card += bits.OnesCount64(w)
	}
	res.card = card
	return finishBitset(res)
}

func andRunRunRaw(a, b *RunContainer) *RunContainer {
	out := &RunContainer{}
	i, j := 0, 0
	for i < len(a.runs) && j < len(b.runs) {
		lo := maxU16(a.runs[i][0], b.runs[j][0])
		hi := minU16(a.runs[i][1], b.runs[j][1])
		if lo <= hi {
			out.runs = append(out.runs, [2]uint16{lo, hi})
		}
		if a.runs[i][1] < b.runs[j][1] {
			i++
		} else {
			j++
		}
	}
	return out
}

// ---------- OR ----------

func opOr(a, b Container) Container {
	switch x := a.(type) {
	case *ArrayContainer:
		switch y := b.(type) {
		case *ArrayContainer:
			return orArrayArray(x, y)
		case *BitsetContainer:
			return orArrayBitset(x, y)
		case *RunContainer:
			return orArrayRun(x, y)
		case *MMapContainer:
			return opOr(x, y.materialize())
		}
	case *BitsetContainer:
		switch y := b.(type) {
		case *ArrayContainer:
			return orArrayBitset(y, x)
		case *BitsetContainer:
			return orBitsetBitset(x, y)
		case *RunContainer:
			return orBitsetRun(x, y)
		case *MMapContainer:
			return opOr(x, y.materialize())
		}
	case *RunContainer:
		switch y := b.(type) {
		case *ArrayContainer:
			return orArrayRun(y, x)
		case *BitsetContainer:
			return orBitsetRun(y, x)
		case *RunContainer:
			return canonResult(canonicalRun(orRunRunRaw(x, y)))
		case *MMapContainer:
			return opOr(x, y.materialize())
		}
	case *MMapContainer:
		return opOr(x.materialize(), b)
	}
	panic("opOr: unreachable")
}

// mergeSortedUnion merges two sorted slices, emitting duplicates once.
func mergeSortedUnion(a, b []uint16) []uint16 {
	out := make([]uint16, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			out = append(out, a[i])
			i++
		case a[i] > b[j]:
			out = append(out, b[j])
			j++
		default:
			out = append(out, a[i])
			i++
			j++
		}
	}
	out = append(out, a[i:]...)
	out = append(out, b[j:]...)
	return out
}

// mergeSortedXor merges two sorted slices, dropping values present in both.
func mergeSortedXor(a, b []uint16) []uint16 {
	out := make([]uint16, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			out = append(out, a[i])
			i++
		case a[i] > b[j]:
			out = append(out, b[j])
			j++
		default:
			i++
			j++
		}
	}
	out = append(out, a[i:]...)
	out = append(out, b[j:]...)
	return out
}

func orArrayArray(a, b *ArrayContainer) Container {
	return finishSorted(mergeSortedUnion(a.data, b.data))
}

func orArrayBitset(a *ArrayContainer, b *BitsetContainer) Container {
	res := b.clone().(*BitsetContainer)
	bitsetAddAll(res, a.data)
	return finishBitset(res)
}

func orArrayRun(a *ArrayContainer, b *RunContainer) Container {
	return canonResult(canonicalRun(orRunRunRaw(arrayToRuns(a), b)))
}

// arrayToRuns converts a sorted array to runs.
func arrayToRuns(a *ArrayContainer) *RunContainer {
	r := &RunContainer{}
	for i := 0; i < len(a.data); i++ {
		j := i
		for j+1 < len(a.data) && a.data[j+1] == a.data[j]+1 {
			j++
		}
		r.runs = append(r.runs, [2]uint16{a.data[i], a.data[j]})
		i = j
	}
	return r
}

func orBitsetBitset(a, b *BitsetContainer) Container {
	res := newBitsetContainer()
	card := 0
	for i := range res.data {
		w := a.data[i] | b.data[i]
		res.data[i] = w
		card += bits.OnesCount64(w)
	}
	res.card = card
	return finishBitset(res)
}

func orBitsetRun(a *BitsetContainer, b *RunContainer) Container {
	res := a.clone().(*BitsetContainer)
	bitsetAddAllRuns(res, b)
	return finishBitset(res)
}

func orRunRunRaw(a, b *RunContainer) *RunContainer {
	out := &RunContainer{runs: make([][2]uint16, 0, len(a.runs)+len(b.runs))}
	i, j := 0, 0
	for i < len(a.runs) || j < len(b.runs) {
		var run [2]uint16
		switch {
		case j == len(b.runs) || (i < len(a.runs) && a.runs[i][0] <= b.runs[j][0]):
			run = a.runs[i]
			i++
		default:
			run = b.runs[j]
			j++
		}
		if n := len(out.runs); n > 0 && int(run[0]) <= int(out.runs[n-1][1])+1 {
			if run[1] > out.runs[n-1][1] {
				out.runs[n-1][1] = run[1]
			}
		} else {
			out.runs = append(out.runs, run)
		}
	}
	return out
}

// ---------- XOR ----------

func opXor(a, b Container) Container {
	switch x := a.(type) {
	case *ArrayContainer:
		switch y := b.(type) {
		case *ArrayContainer:
			return xorArrayArray(x, y)
		case *BitsetContainer:
			return xorArrayBitset(x, y)
		case *RunContainer:
			return xorArrayRun(x, y)
		case *MMapContainer:
			return opXor(x, y.materialize())
		}
	case *BitsetContainer:
		switch y := b.(type) {
		case *ArrayContainer:
			return xorArrayBitset(y, x)
		case *BitsetContainer:
			return xorBitsetBitset(x, y)
		case *RunContainer:
			return xorBitsetRun(x, y)
		case *MMapContainer:
			return opXor(x, y.materialize())
		}
	case *RunContainer:
		switch y := b.(type) {
		case *ArrayContainer:
			return xorArrayRun(y, x)
		case *BitsetContainer:
			return xorBitsetRun(y, x)
		case *RunContainer:
			return canonResult(canonicalRun(xorRunRunRaw(x, y)))
		case *MMapContainer:
			return opXor(x, y.materialize())
		}
	case *MMapContainer:
		return opXor(x.materialize(), b)
	}
	panic("opXor: unreachable")
}

func xorArrayArray(a, b *ArrayContainer) Container {
	return finishSorted(mergeSortedXor(a.data, b.data))
}

func xorArrayBitset(a *ArrayContainer, b *BitsetContainer) Container {
	res := b.clone().(*BitsetContainer)
	for _, v := range a.data {
		w := &res.data[v>>6]
		m := uint64(1) << (v & 63)
		if *w&m != 0 {
			*w &^= m
			res.card--
		} else {
			*w |= m
			res.card++
		}
	}
	return finishBitset(res)
}

func xorArrayRun(a *ArrayContainer, b *RunContainer) Container {
	return canonResult(canonicalRun(xorRunRunRaw(arrayToRuns(a), b)))
}

func xorBitsetBitset(a, b *BitsetContainer) Container {
	res := newBitsetContainer()
	card := 0
	for i := range res.data {
		w := a.data[i] ^ b.data[i]
		res.data[i] = w
		card += bits.OnesCount64(w)
	}
	res.card = card
	return finishBitset(res)
}

func xorBitsetRun(a *BitsetContainer, b *RunContainer) Container {
	res := a.clone().(*BitsetContainer)
	for _, run := range b.runs {
		res.card += rangeOpBitsetXor(res.data, run[0], run[1])
	}
	return finishBitset(res)
}

func xorRunRunRaw(a, b *RunContainer) *RunContainer {
	// XOR = (A\B) U (B\A); both parts stay run containers.
	d1 := andNotRunRunRaw(a, b)
	d2 := andNotRunRunRaw(b, a)
	return orRunRunRaw(d1, d2)
}

// ---------- ANDNOT (a minus b) ----------

func opAndNot(a, b Container) Container {
	switch x := a.(type) {
	case *ArrayContainer:
		switch y := b.(type) {
		case *ArrayContainer:
			return andNotArrayArray(x, y)
		case *BitsetContainer:
			return andNotArrayBitset(x, y)
		case *RunContainer:
			return andNotArrayRun(x, y)
		case *MMapContainer:
			return opAndNot(x, y.materialize())
		}
	case *BitsetContainer:
		switch y := b.(type) {
		case *ArrayContainer:
			return andNotBitsetArray(x, y)
		case *BitsetContainer:
			return andNotBitsetBitset(x, y)
		case *RunContainer:
			return andNotBitsetRun(x, y)
		case *MMapContainer:
			return opAndNot(x, y.materialize())
		}
	case *RunContainer:
		switch y := b.(type) {
		case *ArrayContainer:
			return andNotRunArray(x, y)
		case *BitsetContainer:
			return andNotRunBitset(x, y)
		case *RunContainer:
			return canonResult(canonicalRun(andNotRunRunRaw(x, y)))
		case *MMapContainer:
			return opAndNot(x, y.materialize())
		}
	case *MMapContainer:
		return opAndNot(x.materialize(), b)
	}
	panic("opAndNot: unreachable")
}

func andNotArrayArray(a, b *ArrayContainer) Container {
	out := make([]uint16, 0, len(a.data))
	i, j := 0, 0
	for i < len(a.data) {
		for j < len(b.data) && b.data[j] < a.data[i] {
			j++
		}
		if j == len(b.data) || b.data[j] != a.data[i] {
			out = append(out, a.data[i])
		}
		i++
	}
	return finishSorted(out)
}

func andNotArrayBitset(a *ArrayContainer, b *BitsetContainer) Container {
	out := make([]uint16, 0, len(a.data))
	for _, v := range a.data {
		if !b.Contains(v) {
			out = append(out, v)
		}
	}
	return finishSorted(out)
}

func andNotArrayRun(a *ArrayContainer, b *RunContainer) Container {
	out := make([]uint16, 0, len(a.data))
	ri := 0
	for _, v := range a.data {
		for ri < len(b.runs) && b.runs[ri][1] < v {
			ri++
		}
		if ri == len(b.runs) || b.runs[ri][0] > v {
			out = append(out, v)
		}
	}
	return finishSorted(out)
}

func andNotBitsetArray(a *BitsetContainer, b *ArrayContainer) Container {
	res := a.clone().(*BitsetContainer)
	for _, v := range b.data {
		w := &res.data[v>>6]
		m := uint64(1) << (v & 63)
		if *w&m != 0 {
			*w &^= m
			res.card--
		}
	}
	return finishBitset(res)
}

func andNotBitsetBitset(a, b *BitsetContainer) Container {
	res := newBitsetContainer()
	card := 0
	for i := range res.data {
		w := a.data[i] &^ b.data[i]
		res.data[i] = w
		card += bits.OnesCount64(w)
	}
	res.card = card
	return finishBitset(res)
}

func andNotBitsetRun(a *BitsetContainer, b *RunContainer) Container {
	res := a.clone().(*BitsetContainer)
	for _, run := range b.runs {
		res.card += rangeOpBitset(res.data, run[0], run[1], false)
	}
	return finishBitset(res)
}

func andNotRunArray(a *RunContainer, b *ArrayContainer) Container {
	return canonResult(canonicalRun(andNotRunRunRaw(a, arrayToRuns(b))))
}

func andNotRunBitset(a *RunContainer, b *BitsetContainer) Container {
	src := a.toBitset()
	res := newBitsetContainer()
	for i := range res.data {
		w := src.data[i] &^ b.data[i]
		res.data[i] = w
		res.card += bits.OnesCount64(w)
	}
	return finishBitset(res)
}

// andNotRunRun returns a minus b as raw runs.
func andNotRunRunRaw(a, b *RunContainer) *RunContainer {
	out := &RunContainer{}
	j := 0
	for _, ra := range a.runs {
		lo, hi := ra[0], ra[1]
		for j < len(b.runs) && b.runs[j][1] < lo {
			j++
		}
		if j == len(b.runs) {
			out.runs = append(out.runs, [2]uint16{lo, hi})
			continue
		}
		k := j
		done := false
		for k < len(b.runs) && b.runs[k][0] <= hi {
			rb := b.runs[k]
			if rb[0] > lo {
				out.runs = append(out.runs, [2]uint16{lo, rb[0] - 1})
			}
			if rb[1] >= hi {
				done = true
				break
			}
			lo = rb[1] + 1
			k++
		}
		if !done && lo <= hi {
			out.runs = append(out.runs, [2]uint16{lo, hi})
		}
	}
	return out
}

// ---------- range helpers ----------

// rangeOpBitsetXor flips bits in [lo,hi] and returns the change in cardinality.
func rangeOpBitsetXor(data []uint64, lo, hi uint16) int {
	delta := 0
	startW, endW := lo>>6, hi>>6
	startMask := ^uint64(0) << (lo & 63)
	endMask := ^uint64(0) >> (63 - (hi & 63))
	apply := func(w *uint64, mask uint64) {
		old := bits.OnesCount64(*w & mask)
		*w ^= mask
		delta += bits.OnesCount64(mask) - 2*old
	}
	if startW == endW {
		apply(&data[startW], startMask&endMask)
		return delta
	}
	apply(&data[startW], startMask)
	for w := int(startW) + 1; w < int(endW); w++ {
		apply(&data[w], ^uint64(0))
	}
	apply(&data[endW], endMask)
	return delta
}

// flipRange returns a new container with [lo,hi] complemented.
func flipRange(c Container, lo, hi uint16) Container {
	if lo > hi {
		return c
	}
	switch x := c.(type) {
	case *ArrayContainer:
		return flipRangeArray(x, lo, hi)
	case *BitsetContainer:
		res := x.clone().(*BitsetContainer)
		res.card += rangeOpBitsetXor(res.data, lo, hi)
		return finishBitset(res)
	case *RunContainer:
		return flipRangeRun(x, lo, hi)
	case *MMapContainer:
		return flipRange(x.materialize(), lo, hi)
	}
	panic("flipRange: unreachable")
}

func flipRangeArray(a *ArrayContainer, lo, hi uint16) Container {
	i := sort.Search(len(a.data), func(k int) bool { return a.data[k] >= lo })
	j := sort.Search(len(a.data), func(k int) bool { return a.data[k] > hi })
	out := make([]uint16, 0, len(a.data)+(j-i)+int(hi-lo)+1)
	out = append(out, a.data[:i]...)
	k := i
	for v := int(lo); v <= int(hi); v++ {
		if k < j && int(a.data[k]) == v {
			k++
			continue
		}
		out = append(out, uint16(v))
	}
	out = append(out, a.data[j:]...)
	return finishSorted(out)
}

func flipRangeRun(r *RunContainer, lo, hi uint16) Container {
	out := &RunContainer{runs: make([][2]uint16, 0, len(r.runs)+2)}
	cur := int(lo)
	stop := false
	for _, run := range r.runs {
		if stop {
			out.runs = append(out.runs, run)
			continue
		}
		if run[1] < lo {
			out.runs = append(out.runs, run)
			continue
		}
		if run[0] > hi {
			if cur <= int(hi) {
				out.runs = append(out.runs, [2]uint16{uint16(cur), hi})
				cur = int(hi) + 1
			}
			out.runs = append(out.runs, run)
			stop = true
			continue
		}
		if run[0] < lo {
			out.runs = append(out.runs, [2]uint16{run[0], lo - 1})
		} else if int(run[0]) > cur {
			out.runs = append(out.runs, [2]uint16{uint16(cur), run[0] - 1})
		}
		if run[1] > hi {
			out.runs = append(out.runs, [2]uint16{hi + 1, run[1]})
			cur = int(hi) + 1
			stop = true
			continue
		}
		cur = int(run[1]) + 1
	}
	if cur <= int(hi) {
		out.runs = append(out.runs, [2]uint16{uint16(cur), hi})
	}
	return out.normalizeRun()
}

// ---------- helpers used by ops ----------

func bitsetAddAll(b *BitsetContainer, vals []uint16) {
	for _, v := range vals {
		w := &b.data[v>>6]
		m := uint64(1) << (v & 63)
		if *w&m == 0 {
			*w |= m
			b.card++
		}
	}
}

func bitsetAddAllRuns(b *BitsetContainer, r *RunContainer) {
	for _, run := range r.runs {
		b.card += rangeOpBitset(b.data, run[0], run[1], true)
	}
}

// ---------- small helpers ----------

func minU16(a, b uint16) uint16 {
	if a < b {
		return a
	}
	return b
}
func maxU16(a, b uint16) uint16 {
	if a > b {
		return a
	}
	return b
}
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
