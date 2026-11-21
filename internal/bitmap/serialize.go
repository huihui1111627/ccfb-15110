package bitmap

import (
	"encoding/binary"
	"fmt"
	"os"
	"unsafe"
)

// Serialized file layout (all integers little endian):
//
//	magic      [8]byte  "PBMIDX01"
//	version    uint32
//	rowCount   uint64   number of rows the bitmap was built for
//	flags      uint64
//	minRow     uint32
//	maxRow     uint32
//	keys       []uint16   (bucket high-16 keys)
//	containerOffsets []uint32  (relative to data section)
//	containerLengths []uint32  (payload length in bytes)
//	containerKinds   []uint8
//	data       container payloads
//
// Array payloads are uint16 arrays, bitset payloads are 1024 uint64 words and
// run payloads are (start,end) uint16 pairs. All payloads are 8-byte aligned,
// so an mmap'd file can be used in place without deserialization.

const (
	Magic          = "PBMIDX01"
	Version        = 1
	headerSize     = 8 + 4 + 8 + 8 + 4 + 4
	payloadAlign   = 8
	KindArrayByte  = byte(Array)
	KindBitsetByte = byte(Bitset)
	KindRunByte    = byte(Run)
)

type Serialized struct {
	RowCount uint64
	MinRow   uint32
	MaxRow   uint32
	Keys     []uint16
	Offsets  []uint32
	Lengths  []uint32
	Kinds    []uint8
	Data     []byte
}

func alignUp(n int, a int) int { return (n + a - 1) / a * a }

// Serialize writes the bitmap to path in the mmap-friendly layout.
func (b *Bitmap) Serialize(path string, rowCount uint64) error {
	nk := len(b.Keys)
	dataSize := 0
	payloadSizes := make([]int, nk)
	for i, c := range b.Conts {
		payloadSizes[i] = containerPayloadSize(c)
		dataSize = alignUp(dataSize, payloadAlign)
		dataSize += payloadSizes[i]
	}
	keysBytes := alignUp(2*nk, 8)
	offBytes := alignUp(4*nk, 8)
	lenBytes := alignUp(4*nk, 8)
	kindBytes := alignUp(nk, 8)
	total := headerSize + keysBytes + offBytes + lenBytes + kindBytes + dataSize

	buf := make([]byte, total)
	copy(buf[0:8], Magic)
	le := binary.LittleEndian
	le.PutUint32(buf[8:], Version)
	le.PutUint64(buf[12:], rowCount)
	le.PutUint64(buf[20:], uint64(nk)<<32)
	var minRow, maxRow uint32
	if nk > 0 {
		minRow = uint32(b.Keys[0]) << 16
		lastC := b.Conts[nk-1]
		maxRow = uint32(b.Keys[nk-1])<<16 | uint32(lastC.Max())
		if c, _ := b.container(b.Keys[0]); c != nil {
			minRow |= uint32(c.Min())
		}
	}
	le.PutUint32(buf[28:], minRow)
	le.PutUint32(buf[32:], maxRow)

	keysOff := headerSize
	offOff := keysOff + keysBytes
	lenOff := offOff + offBytes
	kindOff := lenOff + lenBytes
	dataOff := kindOff + kindBytes
	for i, k := range b.Keys {
		le.PutUint16(buf[keysOff+2*i:], k)
		buf[kindOff+i] = byte(b.Conts[i].Kind())
	}
	cur := 0
	for i, c := range b.Conts {
		cur = alignUp(cur, payloadAlign)
		le.PutUint32(buf[offOff+4*i:], uint32(cur))
		le.PutUint32(buf[lenOff+4*i:], uint32(payloadSizes[i]))
		writeContainerPayload(buf[dataOff+cur:], c)
		cur += payloadSizes[i]
	}
	return os.WriteFile(path, buf, 0o644)
}

func containerPayloadSize(c Container) int {
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
	panic("unknown container")
}

func writeContainerPayload(dst []byte, c Container) {
	switch x := c.(type) {
	case *ArrayContainer:
		for i, v := range x.data {
			binary.LittleEndian.PutUint16(dst[2*i:], v)
		}
	case *RunContainer:
		for i, run := range x.runs {
			binary.LittleEndian.PutUint16(dst[4*i:], run[0])
			binary.LittleEndian.PutUint16(dst[4*i+2:], run[1])
		}
	case *BitsetContainer:
		for i, w := range x.data {
			binary.LittleEndian.PutUint64(dst[8*i:], w)
		}
	}
}

// mmapFile maps a whole file read-only.
func mmapFile(path string) ([]byte, func(), error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if st.Size() == 0 {
		f.Close()
		return nil, nil, fmt.Errorf("empty file")
	}
	data, err := mmapRO(f, int(st.Size()))
	f.Close()
	if err != nil {
		return nil, nil, err
	}
	return data, func() { munmapRO(data) }, nil
}

// OpenMapped opens a serialized bitmap without deserializing it.
func OpenMapped(path string) (*MappedBitmap, error) {
	data, closer, err := mmapFile(path)
	if err != nil {
		return nil, err
	}
	le := binary.LittleEndian
	if string(data[0:8]) != Magic {
		closer()
		return nil, fmt.Errorf("bad magic")
	}
	if v := le.Uint32(data[8:]); v != Version {
		closer()
		return nil, fmt.Errorf("unsupported version %d", v)
	}
	rowCount := le.Uint64(data[12:])
	minRow := le.Uint32(data[28:])
	maxRow := le.Uint32(data[32:])
	hdr := headerSize
	nk := int(le.Uint64(data[20:]) >> 32)
	if nk < 0 || nk > 65536 {
		closer()
		return nil, fmt.Errorf("bad bucket count %d", nk)
	}
	if nk == 0 {
		return &MappedBitmap{bm: &Bitmap{}, closer: closer, RowCount: rowCount, MinRow: minRow, MaxRow: maxRow}, nil
	}
	keys := make([]uint16, nk)
	for i := range keys {
		keys[i] = le.Uint16(data[hdr+2*i:])
	}
	keysBytes := alignUp(2*nk, 8)
	offOff := hdr + keysBytes
	offBytes := alignUp(4*nk, 8)
	lenOff := offOff + offBytes
	lenBytes := alignUp(4*nk, 8)
	kindOff := lenOff + lenBytes
	kindBytes := alignUp(nk, 8)
	dataOff := kindOff + kindBytes
	offsets := make([]uint32, nk)
	lengths := make([]uint32, nk)
	kinds := make([]uint8, nk)
	for i := range offsets {
		offsets[i] = le.Uint32(data[offOff+4*i:])
		lengths[i] = le.Uint32(data[lenOff+4*i:])
		kinds[i] = data[kindOff+i]
	}
	conts := make([]Container, nk)
	for i := 0; i < nk; i++ {
		start := dataOff + int(offsets[i])
		seg := data[start : start+int(lengths[i])]
		k := Kind(kinds[i])
		switch k {
		case Array:
			n := len(seg) / 2
			conts[i] = &MMapContainer{kind: Array, arr: unsafe.Slice((*uint16)(unsafe.Pointer(&seg[0])), n), card: n}
		case Bitset:
			words := unsafe.Slice((*uint64)(unsafe.Pointer(&seg[0])), BitmapWords)
			c := 0
			for _, w := range words {
				c += popcount(w)
			}
			conts[i] = &MMapContainer{kind: Bitset, bits: words, card: c}
		case Run:
			n := len(seg) / 4
			runs := unsafe.Slice((*[2]uint16)(unsafe.Pointer(&seg[0])), n)
			c := 0
			for _, r := range runs {
				c += int(r[1]) - int(r[0]) + 1
			}
			conts[i] = &MMapContainer{kind: Run, runs: runs, card: c}
		}
	}
	return &MappedBitmap{bm: &Bitmap{Keys: keys, Conts: conts}, closer: closer, RowCount: rowCount, MinRow: minRow, MaxRow: maxRow}, nil
}

// MappedBitmap is a read-only bitmap over an mmap'd file.
type MappedBitmap struct {
	bm       *Bitmap
	closer   func()
	RowCount uint64
	MinRow   uint32
	MaxRow   uint32
}

func (m *MappedBitmap) Bitmap() *Bitmap { return m.bm }
func (m *MappedBitmap) Close()          { m.closer() }

func (m *MMapContainer) Kind() Kind          { return m.kind }
func (m *MMapContainer) Cardinality() int    { return m.card }
func (m *MMapContainer) DeepCopy() Container { return m.materialize() }
func (m *MMapContainer) SizeBytes() int {
	switch m.kind {
	case Array:
		return 2 * len(m.arr)
	case Bitset:
		return 8 * len(m.bits)
	case Run:
		return 4 * len(m.runs)
	}
	return 0
}

// MMapContainer is a container whose backing storage lives in an mmap'd file.
// It is read-only; any write operation materializes an in-memory copy first.
type MMapContainer struct {
	kind Kind
	// exactly one of these is set
	arr  []uint16
	bits []uint64
	runs [][2]uint16
	card int
}

func (m *MMapContainer) materialize() Container {
	switch m.kind {
	case Array:
		d := make([]uint16, len(m.arr))
		copy(d, m.arr)
		return &ArrayContainer{data: d}
	case Bitset:
		b := newBitsetContainer()
		copy(b.data, m.bits)
		b.card = m.card
		return b
	case Run:
		r := &RunContainer{runs: make([][2]uint16, len(m.runs))}
		copy(r.runs, m.runs)
		return r
	}
	panic("unknown mmap kind")
}

func (m *MMapContainer) Contains(v uint16) bool {
	switch m.kind {
	case Array:
		lo, hi := 0, len(m.arr)
		for lo < hi {
			mid := (lo + hi) / 2
			if m.arr[mid] < v {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		return lo < len(m.arr) && m.arr[lo] == v
	case Bitset:
		return m.bits[v>>6]&(uint64(1)<<(v&63)) != 0
	case Run:
		lo, hi := 0, len(m.runs)
		for lo < hi {
			mid := (lo + hi) / 2
			if m.runs[mid][1] < v {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		return lo < len(m.runs) && m.runs[lo][0] <= v
	}
	return false
}

func (m *MMapContainer) Min() uint16 {
	switch m.kind {
	case Array:
		return m.arr[0]
	case Bitset:
		for i, w := range m.bits {
			if w != 0 {
				return uint16(i*64 + trailingZeros(w))
			}
		}
	case Run:
		return m.runs[0][0]
	}
	panic("Min on empty")
}
func (m *MMapContainer) Max() uint16 {
	switch m.kind {
	case Array:
		return m.arr[len(m.arr)-1]
	case Bitset:
		for i := len(m.bits) - 1; i >= 0; i-- {
			if w := m.bits[i]; w != 0 {
				return uint16(i*64 + 63 - leadingZeros(w))
			}
		}
	case Run:
		return m.runs[len(m.runs)-1][1]
	}
	panic("Max on empty")
}
func (m *MMapContainer) Rank(v uint16) int {
	switch m.kind {
	case Array:
		lo, hi := 0, len(m.arr)
		for lo < hi {
			mid := (lo + hi) / 2
			if m.arr[mid] <= v {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		return lo
	case Bitset:
		last := int(v >> 6)
		total := 0
		for i := 0; i < last; i++ {
			total += popcount(m.bits[i])
		}
		mask := ^uint64(0) >> (63 - (v & 63))
		return total + popcount(m.bits[last]&mask)
	case Run:
		total := 0
		for _, r := range m.runs {
			if r[0] > v {
				break
			}
			if r[1] <= v {
				total += int(r[1]) - int(r[0]) + 1
			} else {
				total += int(v) - int(r[0]) + 1
				break
			}
		}
		return total
	}
	return 0
}
func (m *MMapContainer) Select(k int) uint16 {
	switch m.kind {
	case Array:
		return m.arr[k]
	case Bitset:
		for i, w := range m.bits {
			c := popcount(w)
			if k < c {
				for {
					t := trailingZeros(w)
					if k == 0 {
						return uint16(i*64 + t)
					}
					w &= w - 1
					k--
				}
			}
			k -= c
		}
	case Run:
		for _, r := range m.runs {
			n := int(r[1]) - int(r[0]) + 1
			if k < n {
				return r[0] + uint16(k)
			}
			k -= n
		}
	}
	panic("Select out of range")
}
func (m *MMapContainer) RangeCard(lo, hi uint16) int {
	if lo > hi {
		return 0
	}
	if lo == 0 {
		return m.Rank(hi)
	}
	return m.Rank(hi) - m.Rank(lo-1)
}
func (m *MMapContainer) ForEach(fn func(uint16) bool) {
	switch m.kind {
	case Array:
		for _, v := range m.arr {
			if !fn(v) {
				return
			}
		}
	case Bitset:
		for i, w := range m.bits {
			base := uint16(i * 64)
			for w != 0 {
				t := trailingZeros(w)
				if !fn(base + uint16(t)) {
					return
				}
				w &= w - 1
			}
		}
	case Run:
		for _, r := range m.runs {
			for v := int(r[0]); v <= int(r[1]); v++ {
				if !fn(uint16(v)) {
					return
				}
			}
		}
	}
}

func (m *MMapContainer) applyAdd(v uint16) Container    { return m.materialize().applyAdd(v) }
func (m *MMapContainer) applyRemove(v uint16) Container { return m.materialize().applyRemove(v) }
func (m *MMapContainer) applyRange(lo, hi uint16, set bool) Container {
	return m.materialize().applyRange(lo, hi, set)
}
