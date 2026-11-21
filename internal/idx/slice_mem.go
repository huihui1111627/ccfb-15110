package idx

import "pianoindex/internal/bitmap"

// fieldSummary aggregates the containers of one field across all its bitmaps.
type KindMix struct {
	Array   int `json:"array"`
	Bitset  int `json:"bitset"`
	Run     int `json:"run"`
	Size    int `json:"sizeByte"`
	Bitmaps int `json:"bitmaps"`
	Rows    int `json:"rows"`
}

func fieldMix(bitmaps []*bitmap.Bitmap) KindMix {
	var m KindMix
	for _, b := range bitmaps {
		st := b.Stats()
		m.Array += st.Arrays
		m.Bitset += st.Bitsets
		m.Run += st.Runs
		m.Size += st.SizeByte
		m.Bitmaps++
	}
	return m
}
