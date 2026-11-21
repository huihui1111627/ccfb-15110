package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pianoindex/internal/dataset"
	"pianoindex/internal/idx"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	ps, _ := dataset.Generate(3, 5000)
	return New(idx.Build(ps))
}

func getJSON(t *testing.T, h http.Handler, path string, out any) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code >= 300 && w.Body.Len() > 0 {
		t.Logf("body: %s", w.Body.String())
	}
	if out != nil && w.Code == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	return w.Code
}

func TestEndpoints(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()

	var meta map[string]any
	if code := getJSON(t, h, "/api/meta", &meta); code != 200 {
		t.Fatalf("meta status %d", code)
	}
	if meta["pianos"].(float64) != 5000 {
		t.Fatalf("unexpected piano count")
	}

	var bm bitmapResp
	if code := getJSON(t, h, "/api/bitmap?name="+strings.ReplaceAll("brand=Yamaha", "=", "%3D"), &bm); code != 200 {
		t.Fatalf("bitmap status %d", code)
	}
	if !bm.Exists || bm.Cardinality == 0 || len(bm.Buckets) == 0 {
		t.Fatalf("bitmap response incomplete: %+v", bm)
	}
	if bm.Buckets[0].Kind == "" {
		t.Fatalf("missing kind")
	}

	var comp []map[string]any
	if code := getJSON(t, h, "/api/composition", &comp); code != 200 || len(comp) < 4 {
		t.Fatalf("composition bad")
	}
	var mem map[string]any
	if code := getJSON(t, h, "/api/memory", &mem); code != 200 {
		t.Fatalf("memory bad")
	}
	if mem["total"].(float64) <= 0 {
		t.Fatalf("zero memory")
	}

	// query via GET
	var qr map[string]any
	if code := getJSON(t, h, "/api/query?q="+urlenc(`brand = Yamaha & age >= 20`), &qr); code != 200 {
		t.Fatalf("query bad")
	}
	if qr["rows"].(float64) <= 0 {
		t.Fatalf("no rows")
	}
	// bad query
	if code := getJSON(t, h, "/api/query?q="+urlenc(`brand =`), nil); code != 400 {
		t.Fatalf("expected 400 for bad query, got %d", code)
	}

	// range op
	var rr map[string]any
	if code := getJSON(t, h, "/api/range?name=crack%3Dtrue&op=flip&lo=0&hi=70000", &rr); code != 200 {
		t.Fatalf("range bad")
	}
	if rr["bucketsSpanned"].(float64) != 2 {
		t.Fatalf("bucketsSpanned=%v", rr["bucketsSpanned"])
	}
	// undo
	getJSON(t, h, "/api/range?name=crack%3Dtrue&op=flip&lo=0&hi=70000", nil)
}

type bitmapResp struct {
	Name        string `json:"name"`
	Exists      bool   `json:"exists"`
	Cardinality int    `json:"cardinality"`
	Buckets     []struct {
		Kind string `json:"kind"`
	} `json:"buckets"`
}

func urlenc(s string) string {
	r := strings.NewReplacer(" ", "%20", "=", "%3D", "&", "%26", ">", "%3E", "<", "%3C", `"`, "%22")
	return r.Replace(s)
}

func TestMutateEndpoint(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()
	body := `{"action":"delete","row":3}`
	req := httptest.NewRequest(http.MethodPost, "/api/mutate", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("mutate status %d: %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["live"].(float64) != 4999 {
		t.Fatalf("live=%v", out["live"])
	}
}
