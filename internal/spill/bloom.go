package spill

import "encoding/binary"

// bloom is a fixed-size Bloom filter over SHA-256 keys. The keys are
// already uniformly distributed, so two independent 64-bit words of the
// key itself drive Kirsch–Mitzenmacher double hashing; no extra hash
// function is needed.
type bloom struct {
	bits []uint64
	m    uint64
}

func newBloom(n int64) *bloom {
	if n < 1 {
		n = 1
	}
	m := uint64(n) * bloomBitsPerKey
	m = (m + 63) &^ 63
	return &bloom{bits: make([]uint64, m/64), m: m}
}

func (b *bloom) add(k Key) {
	h1 := binary.LittleEndian.Uint64(k[0:8])
	h2 := binary.LittleEndian.Uint64(k[8:16]) | 1
	for i := uint64(0); i < bloomProbes; i++ {
		p := (h1 + i*h2) % b.m
		b.bits[p>>6] |= 1 << (p & 63)
	}
}

func (b *bloom) test(k Key) bool {
	h1 := binary.LittleEndian.Uint64(k[0:8])
	h2 := binary.LittleEndian.Uint64(k[8:16]) | 1
	for i := uint64(0); i < bloomProbes; i++ {
		p := (h1 + i*h2) % b.m
		if b.bits[p>>6]&(1<<(p&63)) == 0 {
			return false
		}
	}
	return true
}

// Bloom is an approximate, memory-fixed membership filter, exported for
// callers whose use is advisory (a false positive costs one extra probe,
// never a wrong result), such as dedup hints.
type Bloom struct{ b *bloom }

// NewBloom sizes a filter for about n keys at roughly a 1% false-positive rate.
func NewBloom(n int64) *Bloom { return &Bloom{b: newBloom(n)} }

// Add inserts k.
func (f *Bloom) Add(k Key) { f.b.add(k) }

// Test reports whether k may be present (never a false negative).
func (f *Bloom) Test(k Key) bool { return f.b.test(k) }
