// Package idx is the piano archive index: one bitmap per field value, a
// bit-sliced slice per numeric field, and an evaluator that turns query
// expressions into bitmap algebra.
package idx

import (
	"sort"
	"sync"
	"time"

	"pianoindex/internal/bitmap"
	"pianoindex/internal/bitslice"
	"pianoindex/internal/dataset"
	"pianoindex/internal/query"
)

type Field struct {
	Name     string
	Values   []string
	Bitmaps  []*bitmap.Bitmap
	ValueIdx map[string]int
	// bitmapMap names the logical bitmap (e.g. "brand=Yamaha").
	bitmapMap map[string]*bitmap.Bitmap
}

type Index struct {
	mu sync.RWMutex
	// planes holds the word-form copies of numeric planes, rebuilt lazily after
	// any mutation so bit-sliced comparisons avoid reconverting bitmaps.
	planes *planeCache

	Pianos   []dataset.Piano
	RowCount int
	All      *bitmap.Bitmap // all live rows
	LiveRows int

	Brand  Field
	Model  Field
	Store  Field
	Status Field

	Age        *bitslice.Slice
	Tuned      *bitslice.Slice // -1 (never) represented by a separate bitmap
	NeverTuned *bitmap.Bitmap
	HasCrack   *bitmap.Bitmap

	// Diagnostic counters filled by the evaluator on the last query.
	LastTrace *Trace
}

type planeCache struct {
	age          []*bitmap.WordBitmap
	tuned        []*bitmap.WordBitmap
	nonNullAge   *bitmap.WordBitmap
	nonNullTuned *bitmap.WordBitmap
}

// Build constructs the index from piano records. Records are assumed to have
// row id == slice position == Piano.ID.
func Build(ps []dataset.Piano) *Index {
	ix := &Index{Pianos: ps, RowCount: len(ps)}
	ix.All = bitmap.New()
	brand := newField("brand")
	model := newField("model")
	store := newField("store")
	status := newField("status")
	ix.Age = bitslice.New(7)   // 0..127
	ix.Tuned = bitslice.New(9) // 0..511 months
	ix.NeverTuned = bitmap.New()
	ix.HasCrack = bitmap.New()

	for i := range ps {
		p := &ps[i]
		row := p.ID
		_ = row
		idx := uint32(i)
		ix.All.Add(idx)
		brand.add(idx, p.Brand)
		model.add(idx, p.Model)
		store.add(idx, p.Store)
		status.add(idx, p.Status)
		ix.Age.Add(idx, uint64(p.Age))
		if p.TunedMonth >= 0 {
			ix.Tuned.Add(idx, uint64(p.TunedMonth))
		} else {
			ix.NeverTuned.Add(idx)
		}
		if p.HasCrack {
			ix.HasCrack.Add(idx)
		}
	}
	ix.LiveRows = len(ps)
	brand.finish()
	model.finish()
	store.finish()
	status.finish()
	ix.Brand, ix.Model, ix.Store, ix.Status = brand, model, store, status
	ix.planes = ix.buildPlanes()
	return ix
}

func (ix *Index) buildPlanes() *planeCache {
	pc := &planeCache{}
	for _, p := range ix.Age.Planes {
		pc.age = append(pc.age, bitmap.FromBitmap(p))
	}
	for _, p := range ix.Tuned.Planes {
		pc.tuned = append(pc.tuned, bitmap.FromBitmap(p))
	}
	pc.nonNullAge = bitmap.FromBitmap(ix.Age.NonNull)
	pc.nonNullTuned = bitmap.FromBitmap(ix.Tuned.NonNull)
	return pc
}

// invalidatePlanes marks the word-form plane cache stale after a mutation.
func (ix *Index) invalidatePlanes() { ix.planes = nil }

func (ix *Index) planeCache() *planeCache {
	if ix.planes == nil {
		ix.planes = ix.buildPlanes()
	}
	return ix.planes
}

func newField(name string) Field {
	return Field{Name: name, ValueIdx: map[string]int{}, bitmapMap: map[string]*bitmap.Bitmap{}}
}

func (f *Field) add(row uint32, val string) {
	i, ok := f.ValueIdx[val]
	if !ok {
		i = len(f.Values)
		f.Values = append(f.Values, val)
		f.Bitmaps = append(f.Bitmaps, bitmap.New())
		f.ValueIdx[val] = i
		f.bitmapMap[f.Name+"="+val] = f.Bitmaps[i]
	}
	f.Bitmaps[i].Add(row)
}

func (f *Field) finish() { f.reindex() }

// reindex sorts values and rebuilds index maps. Bitmaps are never copied or
// reallocated, so callers holding a bitmap pointer stay valid.
func (f *Field) reindex() {
	sort.Sort(sortedField{f})
	f.bitmapMap = make(map[string]*bitmap.Bitmap, len(f.Values))
	for i, v := range f.Values {
		f.ValueIdx[v] = i
		f.bitmapMap[f.Name+"="+v] = f.Bitmaps[i]
	}
}

type sortedField struct{ f *Field }

func (s sortedField) Len() int { return len(s.f.Values) }
func (s sortedField) Swap(i, j int) {
	s.f.Values[i], s.f.Values[j] = s.f.Values[j], s.f.Values[i]
	s.f.Bitmaps[i], s.f.Bitmaps[j] = s.f.Bitmaps[j], s.f.Bitmaps[i]
}
func (s sortedField) Less(i, j int) bool { return s.f.Values[i] < s.f.Values[j] }

func (f *Field) BitmapFor(val string) *bitmap.Bitmap {
	if i, ok := f.ValueIdx[val]; ok {
		return f.Bitmaps[i]
	}
	return bitmap.New()
}

// BitmapByName resolves a display name like "brand=Yamaha" or "age>=10" to a
// bitmap (computed on demand for numeric predicates).
func (ix *Index) BitmapByName(name string) *bitmap.Bitmap {
	for _, f := range []*Field{&ix.Brand, &ix.Model, &ix.Store, &ix.Status} {
		if b, ok := f.bitmapMap[name]; ok {
			return b
		}
	}
	return nil
}

// BitmapNames lists every stored bitmap in a stable order.
func (ix *Index) BitmapNames() []string {
	var out []string
	for _, f := range []*Field{&ix.Brand, &ix.Model, &ix.Store, &ix.Status} {
		for _, v := range f.Values {
			out = append(out, f.Name+"="+v)
		}
	}
	widths := []struct {
		name string
		s    *bitslice.Slice
	}{
		{"age", ix.Age}, {"tuned", ix.Tuned},
	}
	for _, w := range widths {
		for i := 0; i < w.s.Width; i++ {
			out = append(out, w.name+"#"+itoa(i))
		}
	}
	out = append(out, "tuned=never", "crack=true", "all")
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [4]byte
	n := 0
	for i > 0 {
		buf[n] = byte('0' + i%10)
		i /= 10
		n++
	}
	for l, r := 0, n-1; l < r; l, r = l+1, r-1 {
		buf[l], buf[r] = buf[r], buf[l]
	}
	return string(buf[:n])
}

// Trace records per-operation costs for one query.
type Trace struct {
	Query             string         `json:"query"`
	Total             time.Duration  `json:"-"`
	TotalMicros       int64          `json:"totalMicros"`
	Rows              int            `json:"rows"`
	Steps             []Step         `json:"steps"`
	BitmapsTouched    []string       `json:"bitmapsTouched"`
	BitmapsScanned    int            `json:"bitmapsScanned"`
	BucketsTouched    int            `json:"bucketsTouched"`
	ContainersScanned int            `json:"containersScanned"`
	ContainerOps      map[string]int `json:"containerOps"`
}

type Step struct {
	Op     string `json:"op"`
	Detail string `json:"detail"`
	Micros int64  `json:"micros"`
	Result int    `json:"result"`
}

func (t *Trace) add(op, detail string, start time.Time, res int) {
	t.Steps = append(t.Steps, Step{Op: op, Detail: detail, Micros: time.Since(start).Microseconds(), Result: res})
}

// Evaluate runs a parsed query and returns the matching rows plus a trace.
func (ix *Index) Evaluate(n query.Node) (*bitmap.Bitmap, *Trace) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	tr := &Trace{Query: query.String(n), ContainerOps: map[string]int{}}
	start := time.Now()
	ev := &evaluator{ix: ix, tr: tr}
	res := ev.eval(n)
	tr.Total = time.Since(start)
	tr.ContainersScanned = tr.BitmapsScanned
	tr.TotalMicros = tr.Total.Microseconds()
	tr.Rows = res.Cardinality()
	tr.BucketsTouched = len(res.Keys)
	ix.LastTrace = tr
	return res, tr
}

type evaluator struct {
	ix *Index
	tr *Trace
}

func (e *evaluator) eval(n query.Node) *bitmap.Bitmap {
	switch x := n.(type) {
	case *query.AndNode:
		// Evaluate every operand, then AND them smallest-first: the running
		// result stays narrow and later intersections touch fewer containers.
		type operand struct {
			b   *bitmap.Bitmap
			src string
		}
		ops := make([]operand, 0, len(x.Kids))
		for _, k := range x.Kids {
			ops = append(ops, operand{b: e.eval(k), src: query.String(k)})
		}
		sort.SliceStable(ops, func(i, j int) bool {
			return ops[i].b.Cardinality() < ops[j].b.Cardinality()
		})
		acc := ops[0].b
		e.tr.add("AND", ops[0].src, time.Now(), acc.Cardinality())
		for _, o := range ops[1:] {
			st := time.Now()
			acc = acc.And(o.b)
			e.tr.add("AND", o.src, st, acc.Cardinality())
		}
		return acc
	case *query.OrNode:
		var acc *bitmap.Bitmap
		for _, k := range x.Kids {
			b := e.eval(k)
			st := time.Now()
			if acc == nil {
				acc = b
			} else {
				acc = acc.Or(b)
			}
			e.tr.add("OR", query.String(k), st, acc.Cardinality())
		}
		return acc
	case *query.NotNode:
		st := time.Now()
		b := e.eval(x.Kid)
		res := e.ix.All.AndNot(b)
		e.tr.add("NOT", query.String(x.Kid)+" (complement vs all live rows)", st, res.Cardinality())
		return res
	case *query.TermNode:
		return e.term(x.Term)
	}
	panic("unknown node")
}

func (e *evaluator) term(t query.Term) *bitmap.Bitmap {
	st := time.Now()
	var res *bitmap.Bitmap
	if t.Op == query.OpAll {
		res = e.ix.All.Clone()
		e.tr.add("TERM", "all", st, res.Cardinality())
		e.tr.BitmapsScanned++
		e.tr.BitmapsTouched = append(e.tr.BitmapsTouched, "all")
		return res
	}
	switch t.Field {
	case "brand":
		res = e.categorical(&e.ix.Brand, t)
	case "model":
		res = e.categorical(&e.ix.Model, t)
	case "store":
		res = e.categorical(&e.ix.Store, t)
	case "status":
		res = e.categorical(&e.ix.Status, t)
	case "age":
		res = e.numeric("age", e.ix.Age, t)
	case "tuned":
		res = e.ix.tunedTerm(t)
	case "crack":
		res = e.ix.crackTerm(t)
	}
	e.tr.add("TERM", query.String(&query.TermNode{Term: t}), st, res.Cardinality())
	e.tr.BitmapsScanned++
	return res
}

func (e *evaluator) categorical(f *Field, t query.Term) *bitmap.Bitmap {
	switch t.Op {
	case query.OpIn:
		var acc *bitmap.Bitmap
		for _, v := range t.StrIn {
			b := f.BitmapFor(v)
			if acc == nil {
				acc = b
			} else {
				acc = acc.Or(b)
			}
		}
		if acc == nil {
			return bitmap.New()
		}
		return acc
	default:
		b := f.BitmapFor(t.Str)
		switch t.Op {
		case query.OpEq:
			return b
		case query.OpNe:
			return e.ix.All.AndNot(b)
		}
	}
	return bitmap.New()
}

func (e *evaluator) numeric(name string, s *bitslice.Slice, t query.Term) *bitmap.Bitmap {
	pc := e.ix.planeCache()
	var planes []*bitmap.WordBitmap
	var nonNull *bitmap.WordBitmap
	if name == "age" {
		planes, nonNull = pc.age, pc.nonNullAge
	} else {
		planes, nonNull = pc.tuned, pc.nonNullTuned
	}
	nonNullBM := bitmap.FromBitmap(s.NonNull)
	switch t.Op {
	case query.OpGe:
		return nonNullBM.AndNot(bitmap.LessThanWord(planes, nonNull, uint64(t.Num))).ToBitmap()
	case query.OpLe:
		return bitmap.LessThanWord(planes, nonNull, uint64(t.Num)+1).ToBitmap()
	case query.OpGt:
		return nonNullBM.AndNot(bitmap.LessThanWord(planes, nonNull, uint64(t.Num)+1)).ToBitmap()
	case query.OpLt:
		return bitmap.LessThanWord(planes, nonNull, uint64(t.Num)).ToBitmap()
	case query.OpBetween:
		lo := nonNullBM.AndNot(bitmap.LessThanWord(planes, nonNull, uint64(t.Num)))
		hi := bitmap.LessThanWord(planes, nonNull, uint64(t.Num2)+1)
		return lo.And(hi).ToBitmap()
	case query.OpEq:
		return s.Equal(uint64(t.Num))
	case query.OpNe:
		return s.NotEqual(uint64(t.Num))
	}
	return bitmap.New()
}

// tunedTerm handles the special "never tuned" row set.
func (ix *Index) tunedTerm(t query.Term) *bitmap.Bitmap {
	if t.Str == "never" {
		if t.Op == query.OpNe {
			return ix.All.AndNot(ix.NeverTuned)
		}
		return ix.NeverTuned
	}
	pc := ix.planeCache()
	nonNullBM := bitmap.FromBitmap(ix.Tuned.NonNull)
	switch t.Op {
	case query.OpLt:
		return bitmap.LessThanWord(pc.tuned, pc.nonNullTuned, uint64(t.Num)).ToBitmap()
	case query.OpLe:
		return bitmap.LessThanWord(pc.tuned, pc.nonNullTuned, uint64(t.Num)+1).ToBitmap()
	case query.OpGt:
		return nonNullBM.AndNot(bitmap.LessThanWord(pc.tuned, pc.nonNullTuned, uint64(t.Num)+1)).ToBitmap()
	case query.OpGe:
		return nonNullBM.AndNot(bitmap.LessThanWord(pc.tuned, pc.nonNullTuned, uint64(t.Num))).ToBitmap()
	case query.OpBetween:
		lo := nonNullBM.AndNot(bitmap.LessThanWord(pc.tuned, pc.nonNullTuned, uint64(t.Num)))
		hi := bitmap.LessThanWord(pc.tuned, pc.nonNullTuned, uint64(t.Num2)+1)
		return lo.And(hi).ToBitmap()
	case query.OpEq:
		return ix.Tuned.Equal(uint64(t.Num))
	case query.OpNe:
		return ix.Tuned.NotEqual(uint64(t.Num))
	}
	return bitmap.New()
}

func (ix *Index) crackTerm(t query.Term) *bitmap.Bitmap {
	var b *bitmap.Bitmap
	if t.Num == 1 {
		b = ix.HasCrack
	} else {
		b = ix.All.AndNot(ix.HasCrack)
	}
	if t.Op == query.OpNe {
		return ix.All.AndNot(b)
	}
	return b
}

// Rows materializes the piano records for a result bitmap (capped for display).
func (ix *Index) Rows(b *bitmap.Bitmap, limit int) []dataset.Piano {
	out := make([]dataset.Piano, 0, minInt(limit, b.Cardinality()))
	b.ForEach(func(row uint32) bool {
		out = append(out, ix.Pianos[row])
		return len(out) < limit
	})
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ContainerComposition summarizes every bitmap in the index for the UI.
type ContainerComposition struct {
	Name    string         `json:"name"`
	Stats   bitmap.Stats   `json:"stats"`
	Buckets []BucketDetail `json:"buckets"`
}

type BucketDetail struct {
	Key         int    `json:"key"`
	Kind        string `json:"kind"`
	Cardinality int    `json:"cardinality"`
	SizeByte    int    `json:"sizeByte"`
	Runs        int    `json:"runs,omitempty"`
	Min         int    `json:"min"`
	Max         int    `json:"max"`
}

// Composition returns composition details for the named bitmap (or all).
func (ix *Index) Composition(name string, maxBuckets int) *ContainerComposition {
	b := ix.BitmapByName(name)
	if b == nil {
		switch name {
		case "crack=true":
			b = ix.HasCrack
		case "tuned=never":
			b = ix.NeverTuned
		case "all":
			b = ix.All
		default:
			return nil
		}
	}
	c := &ContainerComposition{Name: name, Stats: b.Stats()}
	for i, key := range b.Keys {
		cont := b.Conts[i]
		if len(c.Buckets) >= maxBuckets {
			break
		}
		bd := BucketDetail{
			Key: int(key), Kind: cont.Kind().String(),
			Cardinality: cont.Cardinality(), SizeByte: bitmap.ContainerSize(cont),
			Min: int(cont.Min()), Max: int(cont.Max()),
		}
		if rc, ok := cont.(*bitmap.RunContainer); ok {
			bd.Runs = rc.RunCount()
		}
		c.Buckets = append(c.Buckets, bd)
	}
	return c
}

// MemoryUsage totals the index footprint and breaks it down.
type MemoryUsage struct {
	TotalByte    int            `json:"totalByte"`
	Fields       map[string]int `json:"fields"`
	Bitmaps      map[string]int `json:"bitmaps"`
	RowBytes     int            `json:"rowBytes"`
	PianoCount   int            `json:"pianoCount"`
	BitmapsCount int            `json:"bitmapsCount"`
}

func (ix *Index) Memory() MemoryUsage {
	m := MemoryUsage{Fields: map[string]int{}, Bitmaps: map[string]int{}, PianoCount: len(ix.Pianos)}
	add := func(field, name string, b *bitmap.Bitmap) {
		sz := b.Stats().SizeByte
		m.Fields[field] += sz
		m.TotalByte += sz
		m.Bitmaps[name] = sz
		m.BitmapsCount++
	}
	for _, f := range []*Field{&ix.Brand, &ix.Model, &ix.Store, &ix.Status} {
		for i, v := range f.Values {
			add(f.Name, f.Name+"="+v, f.Bitmaps[i])
		}
	}
	for i := 0; i < ix.Age.Width; i++ {
		add("age", "age#"+itoa(i), ix.Age.Planes[i])
	}
	for i := 0; i < ix.Tuned.Width; i++ {
		add("tuned", "tuned#"+itoa(i), ix.Tuned.Planes[i])
	}
	add("tuned", "tuned=never", ix.NeverTuned)
	add("crack", "crack=true", ix.HasCrack)
	// raw records are part of the real footprint
	m.RowBytes = len(ix.Pianos) * 96
	m.TotalByte += m.RowBytes
	return m
}

// Incremental updates -------------------------------------------------------

// AddPiano appends a record and updates every bitmap incrementally.
func (ix *Index) AddPiano(p dataset.Piano) uint32 {
	ix.invalidatePlanes()
	ix.mu.Lock()
	defer ix.mu.Unlock()
	row := uint32(len(ix.Pianos))
	p.ID = row
	ix.Pianos = append(ix.Pianos, p)
	ix.All.Add(row)
	rowAdd := func(f *Field, val string) {
		f.add(row, val)
	}
	rowAdd(&ix.Brand, p.Brand)
	rowAdd(&ix.Model, p.Model)
	rowAdd(&ix.Store, p.Store)
	rowAdd(&ix.Status, p.Status)
	ix.Age.Add(row, uint64(p.Age))
	if p.TunedMonth >= 0 {
		ix.Tuned.Add(row, uint64(p.TunedMonth))
	} else {
		ix.NeverTuned.Add(row)
	}
	if p.HasCrack {
		ix.HasCrack.Add(row)
	}
	ix.LiveRows = len(ix.Pianos)
	// keep the sorted value list / name maps current
	ix.Brand.reindex()
	ix.Model.reindex()
	ix.Store.reindex()
	ix.Status.reindex()
	return row
}

// UpdatePiano changes a record in place and updates affected bitmaps. Numeric
// fields use bit-slice Update so only changed bit planes are touched.
func (ix *Index) UpdatePiano(row uint32, p dataset.Piano) {
	ix.invalidatePlanes()
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if !ix.All.Contains(row) {
		return // row was removed; ignore stale updates
	}
	old := ix.Pianos[row]
	p.ID = row
	ix.Pianos[row] = p
	if old.Brand != p.Brand {
		ix.Brand.remove(row, old.Brand)
		ix.Brand.add(row, p.Brand)
	}
	if old.Model != p.Model {
		ix.Model.remove(row, old.Model)
		ix.Model.add(row, p.Model)
	}
	if old.Store != p.Store {
		ix.Store.remove(row, old.Store)
		ix.Store.add(row, p.Store)
	}
	if old.Status != p.Status {
		ix.Status.remove(row, old.Status)
		ix.Status.add(row, p.Status)
	}
	if old.Age != p.Age {
		ix.Age.Update(row, uint64(old.Age), uint64(p.Age))
	}
	if old.TunedMonth != p.TunedMonth {
		switch {
		case old.TunedMonth < 0 && p.TunedMonth >= 0:
			ix.NeverTuned.Remove(row)
			ix.Tuned.Add(row, uint64(p.TunedMonth))
		case old.TunedMonth >= 0 && p.TunedMonth < 0:
			ix.Tuned.Remove(row)
			ix.NeverTuned.Add(row)
		default:
			ix.Tuned.Update(row, uint64(old.TunedMonth), uint64(p.TunedMonth))
		}
	}
	if old.HasCrack != p.HasCrack {
		if p.HasCrack {
			ix.HasCrack.Add(row)
		} else {
			ix.HasCrack.Remove(row)
		}
	}
}

// RemovePiano marks a row dead: the row is removed from every field bitmap and
// from All, so NOT now complements against the remaining live rows.
func (ix *Index) RemovePiano(row uint32) {
	ix.invalidatePlanes()
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if int(row) >= len(ix.Pianos) {
		return
	}
	p := ix.Pianos[row]
	ix.All.Remove(row)
	ix.Brand.remove(row, p.Brand)
	ix.Model.remove(row, p.Model)
	ix.Store.remove(row, p.Store)
	ix.Status.remove(row, p.Status)
	ix.Age.Remove(row)
	if p.TunedMonth >= 0 {
		ix.Tuned.Remove(row)
	} else {
		ix.NeverTuned.Remove(row)
	}
	ix.HasCrack.Remove(row)
	ix.LiveRows = ix.All.Cardinality()
}

func (f *Field) remove(row uint32, val string) {
	if i, ok := f.ValueIdx[val]; ok {
		f.Bitmaps[i].Remove(row)
	}
}
