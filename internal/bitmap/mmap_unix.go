//go:build darwin || linux || freebsd || netbsd || openbsd

package bitmap

import (
	"math/bits"
	"os"
	"syscall"
	"unsafe"
)

func mmapRO(f *os.File, size int) ([]byte, error) {
	if size == 0 {
		return nil, os.ErrInvalid
	}
	b, err := syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func munmapRO(b []byte) { _ = syscall.Munmap(b) }

func popcount(w uint64) int      { return bits.OnesCount64(w) }
func trailingZeros(w uint64) int { return bits.TrailingZeros64(w) }
func leadingZeros(w uint64) int  { return bits.LeadingZeros64(w) }

var _ = unsafe.Pointer(nil)
