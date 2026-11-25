package hashutil

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
)

func JSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("hashutil: marshal: %v", err))
	}
	return HashBytes(b)
}

func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func Short(v any) string {
	return JSON(v)[:16]
}

func Uint64(parts ...any) uint64 {
	h := sha256.New()
	for _, p := range parts {
		switch v := p.(type) {
		case string:
			h.Write([]byte(v))
		case []byte:
			h.Write(v)
		case int:
			var b [8]byte
			binary.LittleEndian.PutUint64(b[:], uint64(v))
			h.Write(b[:])
		case int64:
			var b [8]byte
			binary.LittleEndian.PutUint64(b[:], uint64(v))
			h.Write(b[:])
		case uint64:
			var b [8]byte
			binary.LittleEndian.PutUint64(b[:], v)
			h.Write(b[:])
		case float64:
			var b [8]byte
			binary.LittleEndian.PutUint64(b[:], math.Float64bits(v))
			h.Write(b[:])
		default:
			b, _ := json.Marshal(v)
			h.Write(b)
		}
		h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	return binary.LittleEndian.Uint64(sum[:8])
}

type SplitMix64 struct {
	state uint64
}

func NewSplitMix(seed uint64) *SplitMix64 {
	if seed == 0 {
		seed = 0x9e3779b97f4a7c15
	}
	return &SplitMix64{state: seed}
}

func (s *SplitMix64) Next() uint64 {
	s.state += 0x9e3779b97f4a7c15
	z := s.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (s *SplitMix64) Float64() float64 {
	return float64(s.Next()>>11) / float64(1<<53)
}

func (s *SplitMix64) Intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(s.Next() % uint64(n))
}
