package bitmap

import (
	"fmt"
	"math/bits"
	"sort"
)

// Kind identifies a container representation.
type Kind uint8

const (
	Array Kind = iota + 1
	Bitset
	Run
)

func (k Kind) String() string {
	switch k {
	case Array:
		return "array"
	case Bitset:
		return "bitset"
	case Run:
		return "run"
	}
	return fmt.Sprintf("kind(%d)", k)
}

const (
	// BitmapWords is the number of uint64 words in a full container (65536 bits).
	BitmapWords = 1024
	// MixedThreshold is the cardinality above which an array container becomes a bitmap.
	MixedThreshold = 4096
	// runWordThreshold is the run count above which a bitset is smaller than a run container.
	runWordThreshold = BitmapWords
)

// Container is a set of low-16-bit values.
type Container interface {
	Kind() Kind
	Cardinality() int
	Contains(v uint16) bool
	Min() uint16 // requires Cardinality() > 0
	Max() uint16 // requires Cardinality() > 0
	// Rank returns the number of values <= v.
	Rank(v uint16) int
	// Select returns the k-th smallest value (k is 0-based).
	Select(k int) uint16
	// RangeCard counts values in [lo, hi].
	RangeCard(lo, hi uint16) int
	// ForEach calls fn on every value until fn returns false.
	ForEach(fn func(v uint16) bool)
	DeepCopy() Container

	// apply* methods mutate the receiver when it is safe and return the possibly
	// re-typed container that should replace it.
	applyAdd(v uint16) Container
	applyRemove(v uint16) Container
	applyRange(lo, hi uint16, set bool) Container
}

func rangeCardRank(c Container, lo, hi uint16) int {
	if lo > hi {
		return 0
	}
	if lo == 0 {
		return c.Rank(hi)
	}
	return c.Rank(hi) - c.Rank(lo-1)
}

// chooseKind picks the representation with the smallest in-memory footprint for
// a container with the given cardinality and run count. Ties prefer array, then
// run, then bitset so results are deterministic.
func chooseKind(card, runs int) Kind {
	// A run pays off only when its 4 bytes/run beat the alternatives -- and a
	// bitset's uniform 8 KiB is also much cheaper to scan, so runs are capped at
	// runWordThreshold even when 4*runs would nominally be smaller.
	if runs <= runWordThreshold {
		runSize := 4 * runs
		arraySize := 1 << 30
		if card <= MixedThreshold {
			arraySize = 2 * card
		}
		bitsetSize := 1 << 30
		if card > MixedThreshold {
			bitsetSize = BitmapWords * 8
		}
		if runSize < arraySize && runSize*2 <= bitsetSize {
			return Run
		}
		if arraySize <= bitsetSize {
			return Array
		}
		return Bitset
	}
	if card <= MixedThreshold {
		return Array
	}
	return Bitset
}

// containerFromSorted builds the smallest reasonable container from a sorted,
// deduplicated slice.
func containerFromSorted(vals []uint16) Container {
	n := len(vals)
	if n == 0 {
		return &ArrayContainer{}
	}
	runs := 1
	for i := 1; i < n; i++ {
		if vals[i] != vals[i-1]+1 {
			runs++
		}
	}
	switch chooseKind(n, runs) {
	case Run:
		return newRunContainerFromSorted(vals)
	case Array:
		cp := make([]uint16, n)
		copy(cp, vals)
		return &ArrayContainer{data: cp}
	default:
		return newBitsetFromSorted(vals)
	}
}

func newBitsetFromSorted(vals []uint16) *BitsetContainer {
	b := newBitsetContainer()
	b.card = len(vals)
	for _, v := range vals {
		b.data[v>>6] |= uint64(1) << (v & 63)
	}
	return b
}

func sortedValues(c Container) []uint16 {
	vals := make([]uint16, 0, c.Cardinality())
	c.ForEach(func(v uint16) bool { vals = append(vals, v); return true })
	return vals
}

// ---------- Array container ----------

// ArrayContainer stores sorted values as flat uint16s.
type ArrayContainer struct {
	data []uint16
}

func (a *ArrayContainer) Kind() Kind          { return Array }
func (a *ArrayContainer) Cardinality() int    { return len(a.data) }
func (a *ArrayContainer) Min() uint16         { return a.data[0] }
func (a *ArrayContainer) Max() uint16         { return a.data[len(a.data)-1] }
func (a *ArrayContainer) DeepCopy() Container { return a.clone() }
func (a *ArrayContainer) clone() Container {
	d := make([]uint16, len(a.data))
	copy(d, a.data)
	return &ArrayContainer{data: d}
}
func (a *ArrayContainer) index(v uint16) int {
	return sort.Search(len(a.data), func(i int) bool { return a.data[i] >= v })
}
func (a *ArrayContainer) Contains(v uint16) bool {
	i := a.index(v)
	return i < len(a.data) && a.data[i] == v
}
func (a *ArrayContainer) Rank(v uint16) int {
	return sort.Search(len(a.data), func(i int) bool { return a.data[i] > v })
}
func (a *ArrayContainer) Select(k int) uint16 { return a.data[k] }
func (a *ArrayContainer) RangeCard(lo, hi uint16) int {
	return rangeCardRank(a, lo, hi)
}
func (a *ArrayContainer) ForEach(fn func(uint16) bool) {
	for _, v := range a.data {
		if !fn(v) {
			return
		}
	}
}

func (a *ArrayContainer) applyAdd(v uint16) Container {
	i := a.index(v)
	if i < len(a.data) && a.data[i] == v {
		return a
	}
	if len(a.data)+1 <= MixedThreshold {
		a.data = append(a.data, 0)
		copy(a.data[i+1:], a.data[i:])
		a.data[i] = v
		return a
	}
	b := newBitsetFromSorted(a.data)
	b.applyAdd(v)
	return b
}

func (a *ArrayContainer) applyRemove(v uint16) Container {
	i := a.index(v)
	if i >= len(a.data) || a.data[i] != v {
		return a
	}
	a.data = append(a.data[:i], a.data[i+1:]...)
	return a
}

func (a *ArrayContainer) applyRange(lo, hi uint16, set bool) Container {
	if lo > hi {
		return a
	}
	start := sort.Search(len(a.data), func(i int) bool { return a.data[i] >= lo })
	end := sort.Search(len(a.data), func(i int) bool { return a.data[i] > hi })
	if set {
		missing := int(hi) - int(lo) + 1 - (end - start)
		if len(a.data)+missing <= MixedThreshold {
			merged := make([]uint16, 0, len(a.data)+missing)
			merged = append(merged, a.data[:start]...)
			for v := int(lo); v <= int(hi); v++ {
				merged = append(merged, uint16(v))
			}
			merged = append(merged, a.data[end:]...)
			a.data = merged
			return a
		}
		b := newBitsetFromSorted(a.data)
		return b.applyRange(lo, hi, true)
	}
	a.data = append(a.data[:start], a.data[end:]...)
	return a
}

// ---------- Bitset container ----------

// BitsetContainer is a fixed 8192-byte bitmap of the 65536 low values.
// card is maintained incrementally.
type BitsetContainer struct {
	data []uint64
	card int
}

func newBitsetContainer() *BitsetContainer {
	return &BitsetContainer{data: make([]uint64, BitmapWords)}
}

func (b *BitsetContainer) Kind() Kind          { return Bitset }
func (b *BitsetContainer) Cardinality() int    { return b.card }
func (b *BitsetContainer) DeepCopy() Container { return b.clone() }
func (b *BitsetContainer) clone() Container {
	n := newBitsetContainer()
	n.card = b.card
	copy(n.data, b.data)
	return n
}
func (b *BitsetContainer) Contains(v uint16) bool {
	return b.data[v>>6]&(uint64(1)<<(v&63)) != 0
}
func (b *BitsetContainer) Min() uint16 {
	for i, w := range b.data {
		if w != 0 {
			return uint16(i*64 + bits.TrailingZeros64(w))
		}
	}
	panic("Min on empty bitset")
}
func (b *BitsetContainer) Max() uint16 {
	for i := BitmapWords - 1; i >= 0; i-- {
		if w := b.data[i]; w != 0 {
			return uint16(i*64 + 63 - bits.LeadingZeros64(w))
		}
	}
	panic("Max on empty bitset")
}
func (b *BitsetContainer) Rank(v uint16) int {
	last := int(v >> 6)
	total := 0
	for i := 0; i < last; i++ {
		total += bits.OnesCount64(b.data[i])
	}
	mask := ^uint64(0) >> (63 - (v & 63))
	return total + bits.OnesCount64(b.data[last]&mask)
}
func (b *BitsetContainer) Select(k int) uint16 {
	for i, w := range b.data {
		c := bits.OnesCount64(w)
		if k < c {
			for {
				t := bits.TrailingZeros64(w)
				if k == 0 {
					return uint16(i*64 + t)
				}
				w &= w - 1
				k--
			}
		}
		k -= c
	}
	panic("Select out of range")
}
func (b *BitsetContainer) RangeCard(lo, hi uint16) int {
	return rangeCardRank(b, lo, hi)
}
func (b *BitsetContainer) ForEach(fn func(uint16) bool) {
	for i, w := range b.data {
		base := uint16(i * 64)
		for w != 0 {
			t := bits.TrailingZeros64(w)
			if !fn(base + uint16(t)) {
				return
			}
			w &= w - 1
		}
	}
}

func (b *BitsetContainer) applyAdd(v uint16) Container {
	w := v >> 6
	m := uint64(1) << (v & 63)
	if b.data[w]&m == 0 {
		b.data[w] |= m
		b.card++
		// Adding rows to an already-dense bucket can turn it into very few
		// long runs (e.g. "all live rows"). Re-check periodically: counting
		// runs is one pass over 1024 words, so every 4096 additions is cheap.
		if b.card%4096 == 0 && b.card > MixedThreshold {
			return b.maybeRun()
		}
	}
	return b
}

// maybeRun converts a bitset to a run container when that is the smallest
// representation for its current cardinality and run count.
func (b *BitsetContainer) maybeRun() Container {
	runs := b.countRuns()
	if runs > runWordThreshold {
		return b
	}
	if chooseKind(b.card, runs) == Run {
		return b.toRunContainer()
	}
	return b
}
func (b *BitsetContainer) applyRemove(v uint16) Container {
	w := v >> 6
	m := uint64(1) << (v & 63)
	if b.data[w]&m != 0 {
		b.data[w] &^= m
		b.card--
		if b.card <= MixedThreshold {
			return containerFromSorted(sortedValues(b))
		}
	}
	return b
}
func (b *BitsetContainer) applyRange(lo, hi uint16, set bool) Container {
	if lo > hi {
		return b
	}
	b.card += rangeOpBitset(b.data, lo, hi, set)
	if b.card <= MixedThreshold {
		return containerFromSorted(sortedValues(b))
	}
	return b
}

// rangeOpBitset applies a range set/clear to data and returns the net change
// in the number of set bits (positive when setting, negative when clearing).
func rangeOpBitset(data []uint64, lo, hi uint16, set bool) int {
	changed := 0
	startW, endW := lo>>6, hi>>6
	startMask := ^uint64(0) << (lo & 63)
	endMask := ^uint64(0) >> (63 - (hi & 63))
	apply := func(w *uint64, mask uint64) {
		if set {
			changed += bits.OnesCount64(mask &^ *w)
			*w |= mask
		} else {
			changed -= bits.OnesCount64(*w & mask)
			*w &^= mask
		}
	}
	if startW == endW {
		apply(&data[startW], startMask&endMask)
		return changed
	}
	apply(&data[startW], startMask)
	for w := int(startW) + 1; w < int(endW); w++ {
		apply(&data[w], ^uint64(0))
	}
	apply(&data[endW], endMask)
	return changed
}

// ---------- Run container ----------

// RunContainer stores inclusive [start,end] runs sorted by start and disjoint.
type RunContainer struct {
	runs [][2]uint16
}

func newRunContainerFromSorted(vals []uint16) *RunContainer {
	rc := &RunContainer{}
	for i := 0; i < len(vals); i++ {
		j := i
		for j+1 < len(vals) && vals[j+1] == vals[j]+1 {
			j++
		}
		rc.runs = append(rc.runs, [2]uint16{vals[i], vals[j]})
		i = j
	}
	return rc
}
func (r *RunContainer) Kind() Kind          { return Run }
func (r *RunContainer) DeepCopy() Container { return r.clone() }
func (r *RunContainer) clone() Container {
	n := &RunContainer{runs: make([][2]uint16, len(r.runs))}
	copy(n.runs, r.runs)
	return n
}
func (r *RunContainer) Cardinality() int {
	c := 0
	for _, run := range r.runs {
		c += int(run[1]) - int(run[0]) + 1
	}
	return c
}
func (r *RunContainer) Min() uint16 { return r.runs[0][0] }
func (r *RunContainer) Max() uint16 { return r.runs[len(r.runs)-1][1] }

// findRun returns the first run whose end >= v.
func (r *RunContainer) findRun(v uint16) int {
	return sort.Search(len(r.runs), func(i int) bool { return r.runs[i][1] >= v })
}
func (r *RunContainer) Contains(v uint16) bool {
	i := r.findRun(v)
	return i < len(r.runs) && r.runs[i][0] <= v
}
func (r *RunContainer) Rank(v uint16) int {
	i := r.findRun(v)
	total := 0
	for j := 0; j < i; j++ {
		total += int(r.runs[j][1]) - int(r.runs[j][0]) + 1
	}
	if i < len(r.runs) && r.runs[i][0] <= v {
		total += int(v) - int(r.runs[i][0]) + 1
	}
	return total
}
func (r *RunContainer) Select(k int) uint16 {
	for _, run := range r.runs {
		n := int(run[1]) - int(run[0]) + 1
		if k < n {
			return run[0] + uint16(k)
		}
		k -= n
	}
	panic("Select out of range")
}
func (r *RunContainer) RangeCard(lo, hi uint16) int {
	return rangeCardRank(r, lo, hi)
}
func (r *RunContainer) ForEach(fn func(uint16) bool) {
	for _, run := range r.runs {
		for v := int(run[0]); v <= int(run[1]); v++ {
			if !fn(uint16(v)) {
				return
			}
		}
	}
}

// toBitset materializes the run container as a bitset.
func (r *RunContainer) toBitset() *BitsetContainer {
	b := newBitsetContainer()
	for _, run := range r.runs {
		b.card += rangeOpBitset(b.data, run[0], run[1], true)
	}
	return b
}

// normalizeRun merges adjacent runs, then converts to the canonical type.
func (r *RunContainer) normalizeRun() Container {
	out := r.runs[:0]
	for _, run := range r.runs {
		if run[1] < run[0] {
			continue
		}
		if len(out) > 0 && int(run[0]) <= int(out[len(out)-1][1])+1 {
			if run[1] > out[len(out)-1][1] {
				out[len(out)-1][1] = run[1]
			}
			continue
		}
		out = append(out, run)
	}
	r.runs = out
	return canonicalRun(r)
}

// canonicalRun picks the representation with the smallest footprint.
func canonicalRun(r *RunContainer) Container {
	n := r.Cardinality()
	if n == 0 {
		return &ArrayContainer{}
	}
	switch chooseKind(n, len(r.runs)) {
	case Run:
		return r
	case Array:
		return &ArrayContainer{data: sortedValues(r)}
	default:
		return r.toBitset()
	}
}

// runRemoveRangeRaw returns runs minus [lo,hi].
func runRemoveRangeRaw(runs [][2]uint16, lo, hi uint16) [][2]uint16 {
	out := runs[:0]
	for _, run := range runs {
		if run[1] < lo || run[0] > hi {
			out = append(out, run)
			continue
		}
		if run[0] < lo {
			out = append(out, [2]uint16{run[0], lo - 1})
		}
		if run[1] > hi {
			out = append(out, [2]uint16{hi + 1, run[1]})
		}
	}
	return out
}

func (r *RunContainer) applyAdd(v uint16) Container {
	i := r.findRun(v)
	if i < len(r.runs) && r.runs[i][0] <= v {
		return r // already present
	}
	// i is the insertion point; neighbours are i-1 and i.
	mergePrev := i > 0 && r.runs[i-1][1]+1 == v
	mergeNext := i < len(r.runs) && r.runs[i][0] == v+1
	switch {
	case mergePrev && mergeNext:
		r.runs[i-1][1] = r.runs[i][1]
		r.runs = append(r.runs[:i], r.runs[i+1:]...)
	case mergePrev:
		r.runs[i-1][1] = v
	case mergeNext:
		r.runs[i][0] = v
	default:
		r.runs = append(r.runs, [2]uint16{})
		copy(r.runs[i+1:], r.runs[i:])
		r.runs[i] = [2]uint16{v, v}
	}
	return r.normalizeRun()
}

func (r *RunContainer) applyRemove(v uint16) Container {
	i := r.findRun(v)
	if i >= len(r.runs) || r.runs[i][0] > v {
		return r
	}
	run := r.runs[i]
	switch {
	case run[0] == v && run[1] == v:
		r.runs = append(r.runs[:i], r.runs[i+1:]...)
	case run[0] == v:
		r.runs[i][0] = v + 1
	case run[1] == v:
		r.runs[i][1] = v - 1
	default:
		r.runs = append(r.runs, [2]uint16{})
		copy(r.runs[i+2:], r.runs[i+1:])
		r.runs[i+1] = [2]uint16{v + 1, run[1]}
		r.runs[i][1] = v - 1
	}
	return canonicalRun(r)
}

func (r *RunContainer) applyRange(lo, hi uint16, set bool) Container {
	if lo > hi {
		return r
	}
	if !set {
		r.runs = runRemoveRangeRaw(r.runs, lo, hi)
		return canonicalRun(r)
	}
	r.runs = append(r.runs, [2]uint16{lo, hi})
	sort.Slice(r.runs, func(i, j int) bool { return r.runs[i][0] < r.runs[j][0] })
	return r.normalizeRun()
}

func finishSorted(vals []uint16) Container { return containerFromSorted(vals) }

// countRuns counts maximal runs of set bits.
func (b *BitsetContainer) countRuns() int {
	runs := 0
	prev := uint64(0)
	for _, w := range b.data {
		starts := w &^ ((w << 1) | prev)
		runs += bits.OnesCount64(starts)
		prev = w >> 63
	}
	return runs
}

// toRunContainer converts a bitset to runs (no cardinality threshold applied).
func (b *BitsetContainer) toRunContainer() *RunContainer {
	r := &RunContainer{}
	start := -1
	for i := 0; i < BitmapWords; i++ {
		base := i * 64
		w := b.data[i]
		if w == ^uint64(0) {
			if start < 0 {
				start = base
			}
			continue
		}
		if start >= 0 {
			// A run carried in from full words before this one ends either at
			// the end of the previous word or after this word's leading ones.
			lead := bits.TrailingZeros64(^w)
			r.runs = append(r.runs, [2]uint16{uint16(start), uint16(base + lead - 1)})
			start = -1
			if lead > 0 {
				w &^= (uint64(1) << uint(lead)) - 1
			}
		}
		for w != 0 {
			t := bits.TrailingZeros64(w)
			ones := bits.TrailingZeros64(^w >> uint(t))
			if ones > 64-t {
				ones = 64 - t
			}
			if t+ones == 64 {
				start = base + t
				break
			}
			r.runs = append(r.runs, [2]uint16{uint16(base + t), uint16(base + t + ones - 1)})
			w &^= ((uint64(1) << uint(ones)) - 1) << uint(t)
		}
	}
	if start >= 0 {
		r.runs = append(r.runs, [2]uint16{uint16(start), 0xffff})
	}
	return r
}

// RunCount reports the number of runs.
func (r *RunContainer) RunCount() int { return len(r.runs) }
