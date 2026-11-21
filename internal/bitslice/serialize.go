package bitslice

import (
	"encoding/binary"
	"fmt"
	"os"

	"pianoindex/internal/bitmap"
)

// A bit-sliced column serializes as a small manifest plus one mmap-friendly
// bitmap file per plane. Opening it maps every plane; queries then run on the
// mapped planes without any deserialization.

const columnMagic = "PBSLICE1"

// SaveColumn writes the planes of s to dir/prefix-N.pbm and a manifest.
func SaveColumn(s *Slice, dir, prefix string, rowCount uint64) error {
	for i, p := range s.Planes {
		if err := p.Serialize(fmt.Sprintf("%s/%s-%d.pbm", dir, prefix, i), rowCount); err != nil {
			return err
		}
	}
	if err := s.NonNull.Serialize(fmt.Sprintf("%s/%s-null.pbm", dir, prefix), rowCount); err != nil {
		return err
	}
	buf := make([]byte, 8+4+4)
	copy(buf, columnMagic)
	binary.LittleEndian.PutUint32(buf[8:], uint32(s.Width))
	binary.LittleEndian.PutUint32(buf[12:], uint32(len(s.Planes)))
	return os.WriteFile(fmt.Sprintf("%s/%s.manifest", dir, prefix), buf, 0o644)
}

// MappedColumn is a bit-sliced column whose planes are mmap'd.
type MappedColumn struct {
	Width   int
	Planes  []*bitmap.Bitmap
	NonNull *bitmap.Bitmap
	open    []*bitmap.MappedBitmap
}

// OpenColumn maps a serialized column. The caller must Close it.
func OpenColumn(dir, prefix string) (*MappedColumn, error) {
	man, err := os.ReadFile(fmt.Sprintf("%s/%s.manifest", dir, prefix))
	if err != nil {
		return nil, err
	}
	if len(man) < 16 || string(man[0:8]) != columnMagic {
		return nil, fmt.Errorf("bad column manifest")
	}
	w := int(binary.LittleEndian.Uint32(man[8:]))
	n := int(binary.LittleEndian.Uint32(man[12:]))
	if w != n || w < 1 || w > 64 {
		return nil, fmt.Errorf("bad plane count")
	}
	mc := &MappedColumn{Width: w}
	for i := 0; i < w; i++ {
		m, err := bitmap.OpenMapped(fmt.Sprintf("%s/%s-%d.pbm", dir, prefix, i))
		if err != nil {
			mc.Close()
			return nil, err
		}
		mc.open = append(mc.open, m)
		mc.Planes = append(mc.Planes, m.Bitmap())
	}
	m, err := bitmap.OpenMapped(fmt.Sprintf("%s/%s-null.pbm", dir, prefix))
	if err != nil {
		mc.Close()
		return nil, err
	}
	mc.open = append(mc.open, m)
	mc.NonNull = m.Bitmap()
	return mc, nil
}

func (mc *MappedColumn) Close() {
	for _, m := range mc.open {
		m.Close()
	}
	mc.open = nil
}

// Slice projects the mapped planes back to the in-memory type so existing
// predicate code can run against them.
func (mc *MappedColumn) Slice() *Slice {
	return &Slice{Width: mc.Width, Planes: mc.Planes, NonNull: mc.NonNull}
}
