package domain

import (
	"fmt"
	"hash/fnv"
	"math"
	"math/bits"
	"strconv"
)

// MaskWidth applies a declared digest width. Width 64 is the identity and a
// width of zero is explicitly the zero digest.
func MaskWidth(v uint64, width uint) uint64 {
	if width >= 64 {
		return v
	}
	if width == 0 {
		return 0
	}
	return v & ((uint64(1) << width) - 1)
}

// Mix64 is a deterministic splitmix64-based digest mixer. All operations are
// fixed-width uint64 arithmetic, so repeated patches cannot introduce
// architecture-dependent widening or floating-point drift.
func Mix64(values ...uint64) uint64 {
	var x uint64 = 0x243f6a8885a308d3
	for _, v := range values {
		x ^= v + 0x9e3779b97f4a7c15 + (x << 6) + (x >> 2)
		x = splitMix64(x)
	}
	return splitMix64(x)
}

func splitMix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

func HashString(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

func StringDigest(s string) uint64 {
	return Mix64(HashString(s), uint64(len(s)))
}

func HammingDistance(a, b uint64, width uint) uint {
	return uint(bits.OnesCount64(MaskWidth(a^b, width)))
}

// SatAddInt64 adds non-negative values and reports whether the signed range was
// exceeded. The returned value is saturated at math.MaxInt64.
func SatAddInt64(a, b int64) (int64, bool) {
	if a < 0 || b < 0 {
		return 0, true
	}
	if a > math.MaxInt64-b {
		return math.MaxInt64, true
	}
	return a + b, false
}

// SatMulInt64 multiplies non-negative values with saturation.
func SatMulInt64(a, b int64) (int64, bool) {
	if a < 0 || b < 0 {
		return 0, true
	}
	if a == 0 || b == 0 {
		return 0, false
	}
	if a > math.MaxInt64/b {
		return math.MaxInt64, true
	}
	return a * b, false
}

// SatAddDelta applies a signed delta to a non-negative base. Overflow and
// underflow are both saturated and reported.
func SatAddDelta(base, delta int64) (int64, bool, bool) {
	if base < 0 {
		return 0, false, true
	}
	if delta >= 0 {
		if base > math.MaxInt64-delta {
			return math.MaxInt64, true, false
		}
		return base + delta, false, false
	}
	if delta < -base {
		return 0, false, true
	}
	return base + delta, false, false
}

func ParseDigestHex(s string, width uint) (uint64, error) {
	if s == "" {
		return 0, NewError(CodeDigestInvalid, "digest", "empty digest")
	}
	if len(s) < 2 || (s[:2] != "0x" && s[:2] != "0X") {
		return 0, Errorf(CodeDigestInvalid, "digest", "expected 0x-prefixed hex digest, got %q", s)
	}
	if s[2:] == "" {
		return 0, Errorf(CodeDigestInvalid, "digest", "empty digest payload")
	}
	v, err := strconv.ParseUint(s[2:], 16, 64)
	if err != nil {
		return 0, WrapError(CodeDigestInvalid, "digest", err)
	}
	if width == 0 {
		return 0, Errorf(CodeDigestInvalid, "digest", "digest %q exceeds declared width 0", s)
	}
	masked := MaskWidth(v, width)
	if masked != v {
		return 0, Errorf(CodeDigestInvalid, "digest", "digest %q has bits outside declared width %d", s, width)
	}
	return v, nil
}

func FormatDigest(v uint64) string {
	return fmt.Sprintf("0x%016x", v)
}
