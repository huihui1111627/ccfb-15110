// Package web serves the query UI and the index introspection API.
package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"pianoindex/internal/bitmap"
	"pianoindex/internal/dataset"
	"pianoindex/internal/idx"
	"pianoindex/internal/query"
)

//go:embed static/*
var staticFS embed.FS

type Server struct {
	ix  *idx.Index
	mux *http.ServeMux
	mu  sync.Mutex
	// idxMu serializes index reads and mutations so a mutation can never land
	// in the middle of an evaluation.
	idxMu sync.RWMutex
	hist  []HistEntry
}

type HistEntry struct {
	Query  string `json:"query"`
	Rows   int    `json:"rows"`
	Micros int64  `json:"micros"`
	At     string `json:"at"`
}

func New(ix *idx.Index) *Server {
	s := &Server{ix: ix, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.indexPage)
	s.mux.HandleFunc("/api/meta", s.apiMeta)
	s.mux.HandleFunc("/api/query", s.apiQuery)
	s.mux.HandleFunc("/api/bitmap", s.apiBitmap)
	s.mux.HandleFunc("/api/composition", s.apiComposition)
	s.mux.HandleFunc("/api/memory", s.apiMemory)
	s.mux.HandleFunc("/api/range", s.apiRange)
	s.mux.HandleFunc("/api/mutate", s.apiMutate)
	s.mux.HandleFunc("/api/history", s.apiHistory)
	s.mux.Handle("/static/", http.FileServer(http.FS(staticFS)))
}

func (s *Server) indexPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		log.Printf("json: %v", err)
	}
}

func (s *Server) apiMeta(w http.ResponseWriter, r *http.Request) {
	s.idxMu.RLock()
	defer s.idxMu.RUnlock()
	mem := s.ix.Memory()
	vals := func(f *idx.Field) []string { return f.Values }
	writeJSON(w, map[string]any{
		"pianos":   len(s.ix.Pianos),
		"live":     s.ix.All.Cardinality(),
		"brands":   vals(&s.ix.Brand),
		"models":   vals(&s.ix.Model),
		"stores":   vals(&s.ix.Store),
		"statuses": vals(&s.ix.Status),
		"memory":   mem,
		"examples": []string{
			`brand = Yamaha & status = "已租出" & age >= 10`,
			`(brand = Yamaha | brand = Kawai) & tuned < 2022-01 & crack = false`,
			`store = "北京朝阳旗舰店" & status in ("在库可租","维保中") & !crack = true`,
			`age between 20 .. 40 & tuned = never`,
			`!(status = "已退役") & brand in (Steinway, Bösendorfer)`,
		},
	})
}

type queryRequest struct {
	Query      string `json:"query"`
	Limit      int    `json:"limit"`
	ShowSample bool   `json:"showSample"`
}

type queryResponse struct {
	Trace  *idx.Trace      `json:"trace"`
	Rows   int             `json:"rows"`
	Sample []dataset.Piano `json:"sample"`
	Stats  bitmap.Stats    `json:"stats"`
	Query  string          `json:"query"`
	AsOf   string          `json:"asOf"`
}

func (s *Server) apiQuery(w http.ResponseWriter, r *http.Request) {
	req := queryRequest{Query: strings.TrimSpace(r.URL.Query().Get("q")), Limit: 50}
	if req.Query == "" {
		if r.Method == http.MethodPost {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
		}
	}
	if req.Limit <= 0 {
		req.Limit = 50
	}
	if req.Limit > 500 {
		req.Limit = 500
	}
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	node, err := query.Parse(req.Query)
	if err != nil {
		http.Error(w, fmt.Sprintf("查询语法错误: %v", err), 400)
		return
	}
	start := time.Now()
	res, tr := s.ix.Evaluate(node)
	elapsed := time.Since(start)
	sample := s.ix.Rows(res, req.Limit)
	resp := queryResponse{
		Trace: tr, Rows: res.Cardinality(), Sample: sample, Stats: res.Stats(),
		Query: query.String(node), AsOf: time.Now().Format("15:04:05"),
	}
	s.mu.Lock()
	s.hist = append([]HistEntry{{
		Query: resp.Query, Rows: resp.Rows, Micros: elapsed.Microseconds(), At: resp.AsOf,
	}}, s.hist...)
	if len(s.hist) > 50 {
		s.hist = s.hist[:50]
	}
	s.mu.Unlock()
	writeJSON(w, resp)
}

func (s *Server) apiHistory(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, s.hist)
}

type bucketJSON struct {
	Key         int     `json:"key"`
	RowRange    string  `json:"rowRange"`
	Kind        string  `json:"kind"`
	Cardinality int     `json:"cardinality"`
	SizeByte    int     `json:"sizeByte"`
	Runs        int     `json:"runs"`
	Density     float64 `json:"density"`
}

type bitmapResponse struct {
	Name        string       `json:"name"`
	Exists      bool         `json:"exists"`
	Cardinality int          `json:"cardinality"`
	Stats       bitmap.Stats `json:"stats"`
	Buckets     []bucketJSON `json:"buckets"`
	Truncated   bool         `json:"truncated"`
	Sample      []uint32     `json:"sample"`
	MemoryByte  int          `json:"memoryByte"`
}

func (s *Server) apiBitmap(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	maxBuckets := 64
	if v, err := strconv.Atoi(r.URL.Query().Get("buckets")); err == nil && v > 0 && v <= 4096 {
		maxBuckets = v
	}
	s.idxMu.RLock()
	defer s.idxMu.RUnlock()
	b := s.resolve(name)
	if b == nil {
		writeJSON(w, bitmapResponse{Name: name, Exists: false})
		return
	}
	resp := bitmapResponse{Name: name, Exists: true, Cardinality: b.Cardinality(), Stats: b.Stats()}
	resp.MemoryByte = resp.Stats.SizeByte
	for i, key := range b.Keys {
		if i >= maxBuckets {
			resp.Truncated = true
			break
		}
		c := b.Conts[i]
		bd := bucketJSON{
			Key: int(key), Kind: c.Kind().String(), Cardinality: c.Cardinality(),
			SizeByte: bitmap.ContainerBytes(c),
			RowRange: fmt.Sprintf("%d–%d", uint32(key)<<16, uint32(key)<<16|0xffff),
			Density:  float64(c.Cardinality()) / 65536,
		}
		if rc, ok := c.(*bitmap.RunContainer); ok {
			bd.Runs = rc.RunCount()
		}
		resp.Buckets = append(resp.Buckets, bd)
	}
	b.ForEach(func(row uint32) bool {
		resp.Sample = append(resp.Sample, row)
		return len(resp.Sample) < 50
	})
	writeJSON(w, resp)
}

// resolve finds a bitmap by display name, including derived numeric predicates.
func (s *Server) resolve(name string) *bitmap.Bitmap {
	if b := s.ix.BitmapByName(name); b != nil {
		return b
	}
	switch name {
	case "all":
		return s.ix.All
	case "crack=true":
		return s.ix.HasCrack
	case "tuned=never":
		return s.ix.NeverTuned
	}
	if rest, ok := strings.CutPrefix(name, "age#"); ok {
		if n, err := strconv.Atoi(rest); err == nil && n >= 0 && n < len(s.ix.Age.Planes) {
			return s.ix.Age.Planes[n]
		}
		return nil
	}
	if rest, ok := strings.CutPrefix(name, "tuned#"); ok {
		if n, err := strconv.Atoi(rest); err == nil && n >= 0 && n < len(s.ix.Tuned.Planes) {
			return s.ix.Tuned.Planes[n]
		}
		return nil
	}
	if strings.ContainsAny(name, "=<>!") {
		node, err := query.Parse(name)
		if err == nil {
			if t, ok := node.(*query.TermNode); ok && t.Term.Op != query.OpAll {
				res, _ := s.ix.Evaluate(node)
				return res
			}
		}
	}
	return nil
}

// apiComposition returns container composition per field and per named bitmap.
type bitmapComp struct {
	Name  string       `json:"name"`
	Stats bitmap.Stats `json:"stats"`
}

type fieldComp struct {
	Name    string       `json:"name"`
	Bitmaps []bitmapComp `json:"bitmaps"`
	Totals  bitmap.Stats `json:"totals"`
}

func (s *Server) apiComposition(w http.ResponseWriter, r *http.Request) {
	s.idxMu.RLock()
	defer s.idxMu.RUnlock()
	fields := []*idx.Field{&s.ix.Brand, &s.ix.Model, &s.ix.Store, &s.ix.Status}
	var out []fieldComp
	for _, f := range fields {
		fc := fieldComp{Name: f.Name}
		var tot bitmap.Stats
		for i, v := range f.Values {
			st := f.Bitmaps[i].Stats()
			fc.Bitmaps = append(fc.Bitmaps, bitmapComp{Name: f.Name + "=" + v, Stats: st})
			tot.Buckets += st.Buckets
			tot.Arrays += st.Arrays
			tot.Bitsets += st.Bitsets
			tot.Runs += st.Runs
			tot.Rows += st.Rows
			tot.SizeByte += st.SizeByte
		}
		fc.Totals = tot
		sort.Slice(fc.Bitmaps, func(i, j int) bool { return fc.Bitmaps[i].Name < fc.Bitmaps[j].Name })
		out = append(out, fc)
	}
	// numeric planes
	var planes []bitmapComp
	var planeTot bitmap.Stats
	for i, p := range s.ix.Age.Planes {
		st := p.Stats()
		planes = append(planes, bitmapComp{Name: fmt.Sprintf("age#%d", i), Stats: st})
		planeTot.Buckets += st.Buckets
		planeTot.Arrays += st.Arrays
		planeTot.Bitsets += st.Bitsets
		planeTot.Runs += st.Runs
		planeTot.Rows += st.Rows
		planeTot.SizeByte += st.SizeByte
	}
	for i, p := range s.ix.Tuned.Planes {
		st := p.Stats()
		planes = append(planes, bitmapComp{Name: fmt.Sprintf("tuned#%d", i), Stats: st})
		planeTot.Buckets += st.Buckets
		planeTot.Arrays += st.Arrays
		planeTot.Bitsets += st.Bitsets
		planeTot.Runs += st.Runs
		planeTot.Rows += st.Rows
		planeTot.SizeByte += st.SizeByte
	}
	out = append(out, fieldComp{Name: "bit-sliced", Bitmaps: planes, Totals: planeTot})
	writeJSON(w, out)
}

func (s *Server) apiMemory(w http.ResponseWriter, r *http.Request) {
	s.idxMu.RLock()
	defer s.idxMu.RUnlock()
	mem := s.ix.Memory()
	type fieldRow struct {
		Name    string `json:"name"`
		Bytes   int    `json:"bytes"`
		Bitmaps int    `json:"bitmaps"`
		Rows    int    `json:"rows"`
	}
	var rows []fieldRow
	fields := []struct {
		name string
		f    *idx.Field
	}{{"brand", &s.ix.Brand}, {"model", &s.ix.Model}, {"store", &s.ix.Store}, {"status", &s.ix.Status}}
	for _, fr := range fields {
		var st bitmap.Stats
		for i := range fr.f.Values {
			bs := fr.f.Bitmaps[i].Stats()
			st.Buckets += bs.Buckets
			st.Arrays += bs.Arrays
			st.Bitsets += bs.Bitsets
			st.Runs += bs.Runs
			st.Rows += bs.Rows
			st.SizeByte += bs.SizeByte
		}
		rows = append(rows, fieldRow{Name: fr.name, Bytes: st.SizeByte, Bitmaps: len(fr.f.Values), Rows: st.Rows})
	}
	var ageSt, tunedSt bitmap.Stats
	for _, p := range s.ix.Age.Planes {
		bs := p.Stats()
		ageSt.Buckets += bs.Buckets
		ageSt.Arrays += bs.Arrays
		ageSt.Bitsets += bs.Bitsets
		ageSt.Runs += bs.Runs
		ageSt.Rows += bs.Rows
		ageSt.SizeByte += bs.SizeByte
	}
	for _, p := range s.ix.Tuned.Planes {
		bs := p.Stats()
		tunedSt.Buckets += bs.Buckets
		tunedSt.Arrays += bs.Arrays
		tunedSt.Bitsets += bs.Bitsets
		tunedSt.Runs += bs.Runs
		tunedSt.Rows += bs.Rows
		tunedSt.SizeByte += bs.SizeByte
	}
	rows = append(rows, fieldRow{Name: "age (bit-sliced)", Bytes: ageSt.SizeByte, Bitmaps: s.ix.Age.Width, Rows: ageSt.Rows})
	rows = append(rows, fieldRow{Name: "tuned (bit-sliced)", Bytes: tunedSt.SizeByte, Bitmaps: s.ix.Tuned.Width, Rows: tunedSt.Rows})
	var crackSt, neverSt bitmap.Stats
	{
		bs := s.ix.HasCrack.Stats()
		crackSt = bs
		bs2 := s.ix.NeverTuned.Stats()
		neverSt = bs2
	}
	rows = append(rows, fieldRow{Name: "crack", Bytes: crackSt.SizeByte, Bitmaps: 1, Rows: crackSt.Rows})
	rows = append(rows, fieldRow{Name: "tuned=never", Bytes: neverSt.SizeByte, Bitmaps: 1, Rows: neverSt.Rows})
	var kindTot bitmap.Stats
	// container kind totals across everything
	add := func(st bitmap.Stats) {
		kindTot.Buckets += st.Buckets
		kindTot.Arrays += st.Arrays
		kindTot.Bitsets += st.Bitsets
		kindTot.Runs += st.Runs
		kindTot.SizeByte += st.SizeByte
	}
	add(ageSt)
	add(tunedSt)
	add(crackSt)
	add(neverSt)
	for _, fr := range fields {
		for i := range fr.f.Values {
			add(fr.f.Bitmaps[i].Stats())
		}
	}
	writeJSON(w, map[string]any{
		"fields":      rows,
		"total":       mem.TotalByte,
		"bitmaps":     mem.BitmapsCount,
		"rowBytes":    mem.RowBytes,
		"bitmapBytes": mem.TotalByte - mem.RowBytes,
		"pianos":      mem.PianoCount,
		"containers":  kindTot,
		"liveRows":    s.ix.All.Cardinality(),
	})
}

// apiRange demonstrates whole-range set/clear/flip with bucket-span accounting.
func (s *Server) apiRange(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := q.Get("name")
	op := q.Get("op")
	lo64, _ := strconv.ParseUint(q.Get("lo"), 10, 32)
	hi64, _ := strconv.ParseUint(q.Get("hi"), 10, 32)
	lo, hi := uint32(lo64), uint32(hi64)
	if lo > hi {
		lo, hi = hi, lo
	}
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	target := s.resolve(name)
	if target == nil {
		http.Error(w, "unknown bitmap", 404)
		return
	}
	start := time.Now()
	before := target.Cardinality()
	switch op {
	case "set":
		target.SetRange(lo, hi, true)
	case "clear":
		target.SetRange(lo, hi, false)
	case "flip":
		target.FlipRange(lo, hi)
	default:
		http.Error(w, "op must be set|clear|flip", 400)
		return
	}
	elapsed := time.Since(start)
	writeJSON(w, map[string]any{
		"name": name, "op": op, "lo": lo, "hi": hi,
		"bucketsSpanned": int(hi>>16) - int(lo>>16) + 1,
		"before":         before, "after": target.Cardinality(),
		"micros": elapsed.Microseconds(),
	})
}

type mutateRequest struct {
	Action string         `json:"action"`
	Row    uint32         `json:"row"`
	Piano  *dataset.Piano `json:"piano"`
}

// apiMutate applies an incremental add/update/delete and reports which bitmaps
// changed type.
func (s *Server) apiMutate(w http.ResponseWriter, r *http.Request) {
	var req mutateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	names := s.ix.BitmapNames()
	before := map[string]bitmap.Stats{}
	for _, n := range names {
		if b := s.ix.BitmapByName(n); b != nil {
			before[n] = b.Stats()
		}
	}
	switch req.Action {
	case "add":
		if req.Piano == nil {
			http.Error(w, "piano required", 400)
			return
		}
		s.ix.AddPiano(*req.Piano)
	case "update":
		if req.Piano == nil {
			http.Error(w, "piano required", 400)
			return
		}
		s.ix.UpdatePiano(req.Row, *req.Piano)
	case "delete":
		s.ix.RemovePiano(req.Row)
	default:
		http.Error(w, "action must be add|update|delete", 400)
		return
	}
	var changed []map[string]any
	for _, n := range names {
		b := s.ix.BitmapByName(n)
		if b == nil {
			continue
		}
		after := b.Stats()
		if after.Buckets != before[n].Buckets || after.Arrays != before[n].Arrays ||
			after.Bitsets != before[n].Bitsets || after.Runs != before[n].Runs ||
			after.Rows != before[n].Rows {
			changed = append(changed, map[string]any{
				"name": n, "rows": after.Rows,
				"array": after.Arrays, "bitset": after.Bitsets, "run": after.Runs,
			})
		}
	}
	writeJSON(w, map[string]any{
		"ok": true, "action": req.Action, "live": s.ix.All.Cardinality(),
		"changed": changed,
	})
}

// SaveMapped serializes the "all live rows" bitmap (and returns the path) so the
// mmap demo can be exercised from the UI.
func (s *Server) SaveMapped(path string) error {
	return s.ix.All.Serialize(path, uint64(len(s.ix.Pianos)))
}
