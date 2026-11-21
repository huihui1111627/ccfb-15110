package dataset

import (
	"fmt"
	"math/rand"
	"sort"
)

// Piano is one instrument's archive record.
type Piano struct {
	ID         uint32 `json:"id"`
	Brand      string `json:"brand"`
	Model      string `json:"model"`
	Store      string `json:"store"`
	Status     string `json:"status"`
	TunedMonth int    `json:"tunedMonth"` // months since 2014-01; -1 means never tuned
	HasCrack   bool   `json:"hasCrack"`
	Age        int    `json:"age"`
}

// Schema describes the generated value domains.
type Schema struct {
	Brands   []string
	Stores   []string
	Statuses []string
	// Models maps brand to its model list.
	Models map[string][]string
	// Months is the max tuning month offset (inclusive), 0 => 2014-01.
	Months int
	MaxAge int
}

func monthLabel(m int) string {
	if m < 0 {
		return "never"
	}
	y := 2014 + m/12
	mo := m%12 + 1
	return fmt.Sprintf("%04d-%02d", y, mo)
}

func MonthLabel(m int) string { return monthLabel(m) }

// DefaultSchema returns a realistic schema for a piano rental business.
func DefaultSchema() Schema {
	brands := []string{"Yamaha", "Kawai", "Steinway", "Bösendorfer", "Pearl River", "Hailun", "Kayserburg", "Schimmel", "C.Bechstein", "Fazioli"}
	models := map[string][]string{}
	brandModels := map[string]int{
		"Yamaha": 40, "Kawai": 30, "Steinway": 24, "Bösendorfer": 12,
		"Pearl River": 28, "Hailun": 18, "Kayserburg": 14, "Schimmel": 12,
		"C.Bechstein": 10, "Fazioli": 6,
	}
	for _, b := range brands {
		for i := 0; i < brandModels[b]; i++ {
			models[b] = append(models[b], fmt.Sprintf("%s-%c%d", shortBrand(b), 'A'+rune(i/10), i%10+1))
		}
	}
	stores := []string{}
	cities := []string{"北京朝阳", "北京海淀", "上海静安", "上海浦东", "广州天河", "深圳南山", "杭州西湖", "成都武侯", "武汉江汉", "南京鼓楼"}
	suffix := []string{"旗舰店", "中心店", "社区店", "仓库"}
	for _, c := range cities {
		for _, s := range suffix {
			stores = append(stores, c+s)
		}
	}
	statuses := []string{"在库可租", "已租出", "维保中", "待检测", "已退役"}
	return Schema{
		Brands:   brands,
		Stores:   stores,
		Statuses: statuses,
		Models:   models,
		Months:   143, // through 2025-12
		MaxAge:   80,
	}
}

func shortBrand(b string) string {
	switch b {
	case "Yamaha":
		return "Y"
	case "Kawai":
		return "K"
	case "Steinway":
		return "S"
	case "Bösendorfer":
		return "B"
	case "Pearl River":
		return "PR"
	case "Hailun":
		return "H"
	case "Kayserburg":
		return "KB"
	case "Schimmel":
		return "SC"
	case "C.Bechstein":
		return "CB"
	case "Fazioli":
		return "F"
	}
	return "X"
}

// Generate produces n pianos with skewed but plausible distributions.
func Generate(seed int64, n int) ([]Piano, Schema) {
	sch := DefaultSchema()
	rng := rand.New(rand.NewSource(seed))
	// brand weights: Yamaha and Kawai dominate
	brandW := []float64{0.28, 0.22, 0.10, 0.04, 0.12, 0.07, 0.05, 0.05, 0.04, 0.03}
	brandCum := make([]float64, len(brandW))
	acc := 0.0
	for i, w := range brandW {
		acc += w
		brandCum[i] = acc
	}
	pickBrand := func(r float64) int {
		for i, c := range brandCum {
			if r <= c {
				return i
			}
		}
		return len(brandCum) - 1
	}
	// store weights: flagship stores busier
	storeW := make([]float64, len(sch.Stores))
	for i := range storeW {
		switch i % 4 {
		case 0:
			storeW[i] = 4
		case 1:
			storeW[i] = 3
		case 2:
			storeW[i] = 2
		default:
			storeW[i] = 1
		}
	}
	storeCum := make([]float64, len(storeW))
	acc = 0
	for i, w := range storeW {
		acc += w
		storeCum[i] = acc
	}
	pickStore := func(r float64) int {
		for i, c := range storeCum {
			if r <= c {
				return i
			}
		}
		return len(storeCum) - 1
	}
	statusW := []float64{0.30, 0.45, 0.12, 0.09, 0.04}
	statusCum := make([]float64, len(statusW))
	acc = 0
	for i, w := range statusW {
		acc += w
		statusCum[i] = acc
	}
	pickStatus := func(r float64) int {
		for i, c := range statusCum {
			if r <= c {
				return i
			}
		}
		return len(statusCum) - 1
	}
	out := make([]Piano, n)
	for i := 0; i < n; i++ {
		bi := pickBrand(rng.Float64())
		models := sch.Models[sch.Brands[bi]]
		mi := int(rng.ExpFloat64() * float64(len(models)) / 2.5)
		if mi >= len(models) {
			mi = len(models) - 1
		}
		st := pickStatus(rng.Float64())
		p := Piano{
			ID:     uint32(i),
			Brand:  sch.Brands[bi],
			Model:  models[mi],
			Store:  sch.Stores[pickStore(rng.Float64()*storeCum[len(storeCum)-1])],
			Status: sch.Statuses[st],
			Age:    rng.Intn(sch.MaxAge + 1),
		}
		// Tuning recency: rented/tunable pianos tuned more recently; some never.
		var m int
		switch st {
		case 1: // rented: tuned within the last 2 years
			m = sch.Months - rng.Intn(24)
		case 0, 3: // in stock / pending inspection
			m = sch.Months - 24 - rng.Intn(60)
		case 2: // maintenance
			m = sch.Months - rng.Intn(12)
		default: // retired: old or never
			if rng.Float64() < 0.5 {
				m = -1
			} else {
				m = rng.Intn(sch.Months - 48)
			}
		}
		if rng.Float64() < 0.04 && m >= 0 {
			m = -1
		}
		p.TunedMonth = m
		p.HasCrack = rng.Float64() < 0.11
		// older pianos crack more
		if p.Age > 40 && rng.Float64() < 0.25 {
			p.HasCrack = true
		}
		out[i] = p
	}
	return out, sch
}

// DistinctValues returns sorted distinct values for each categorical field.
func DistinctValues(ps []Piano) map[string][]string {
	sets := map[string]map[string]struct{}{}
	for _, p := range ps {
		for k, v := range map[string]string{"brand": p.Brand, "model": p.Model, "store": p.Store, "status": p.Status} {
			if sets[k] == nil {
				sets[k] = map[string]struct{}{}
			}
			sets[k][v] = struct{}{}
		}
	}
	out := map[string][]string{}
	for k, m := range sets {
		vals := make([]string, 0, len(m))
		for v := range m {
			vals = append(vals, v)
		}
		sort.Strings(vals)
		out[k] = vals
	}
	return out
}
