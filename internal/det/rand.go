// Package det provides a tiny deterministic PRNG. Its byte stream is stable
// across Go versions and process runs, unlike package math/rand.
package det

import (
	"crypto/sha256"
	"encoding/binary"
)

type RNG struct {
	state uint64
}

func Seed(seed uint64, nodeID, purpose string) uint64 {
	h := sha256.New()
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], seed)
	h.Write(buf[:])
	h.Write([]byte{0})
	h.Write([]byte(nodeID))
	h.Write([]byte{0})
	h.Write([]byte(purpose))
	sum := h.Sum(nil)
	return binary.LittleEndian.Uint64(sum[:8])
}

func New(seed uint64, nodeID, purpose string) *RNG {
	return &RNG{state: Seed(seed, nodeID, purpose)}
}

func (r *RNG) Next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (r *RNG) Intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.Next() % uint64(n))
}

func (r *RNG) Int64n(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return int64(r.Next() % uint64(n))
}

func (r *RNG) Jitter(base, pct int64) int64 {
	if pct <= 0 {
		return base
	}
	span := base * pct / 100
	if span == 0 {
		return base
	}
	return base - span + r.Int64n(span*2+1)
}
