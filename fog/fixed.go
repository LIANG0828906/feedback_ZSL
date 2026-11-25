package fog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
)

const DefaultFixedScale int64 = 1_000_000

// Quantize rounds v to the declared fixed-point scale and returns the
// raw-minus-quantized residual for audit records.
func Quantize(v float64, scale int64) (q int64, residual float64) {
	if scale <= 0 {
		scale = DefaultFixedScale
	}
	scaled := v * float64(scale)
	q = int64(math.Round(scaled))
	quantized := float64(q) / float64(scale)
	return q, v - quantized
}

func FromFixed(q int64, scale int64) float64 {
	if scale <= 0 {
		scale = DefaultFixedScale
	}
	return float64(q) / float64(scale)
}

func fixedMultiple(v float64, scale int64) bool {
	if scale <= 0 {
		return false
	}
	q := v * float64(scale)
	return math.Abs(q-math.Round(q)) <= 1e-4
}

func clamp01(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return math.Max(0, math.Min(1, v))
}

func clamp(v, lo, hi float64) float64 {
	return math.Max(lo, math.Min(hi, v))
}

// ContentHash returns a stable SHA-256 hash of the JSON content. Structs used
// here have ordered fields, and encoding/json sorts map keys.
func ContentHash(prefix string, v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		sum := sha256.Sum256([]byte(prefix + ":" + err.Error()))
		return prefix + "-err-" + hex.EncodeToString(sum[:])[:16]
	}
	sum := sha256.Sum256(append([]byte(prefix+":"), b...))
	return prefix + "-" + hex.EncodeToString(sum[:])[:24]
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
