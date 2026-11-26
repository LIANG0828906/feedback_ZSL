package strokelab

import "fmt"

func mulChecked(field string, a, b int64) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	const maxI64 = int64(^uint64(0) >> 1)
	const minI64 = -maxI64 - 1
	if a > 0 {
		if b > 0 && a > maxI64/b {
			return 0, withDetails(CodeOverflow, "integer overflow in %s", map[string]any{
				"field": field, "left": a, "right": b,
			}, "%s: %d * %d", field, a, b)
		}
		if b < 0 && b < minI64/a {
			return 0, withDetails(CodeOverflow, "integer overflow in %s", map[string]any{
				"field": field, "left": a, "right": b,
			}, "%s: %d * %d", field, a, b)
		}
	} else {
		if b > 0 && a < minI64/b {
			return 0, withDetails(CodeOverflow, "integer overflow in %s", map[string]any{
				"field": field, "left": a, "right": b,
			}, "%s: %d * %d", field, a, b)
		}
		if b < 0 && a != 0 && b < maxI64/a {
			return 0, withDetails(CodeOverflow, "integer overflow in %s", map[string]any{
				"field": field, "left": a, "right": b,
			}, "%s: %d * %d", field, a, b)
		}
	}
	return a * b, nil
}

func addChecked(field string, a, b int64) (int64, error) {
	const maxI64 = int64(^uint64(0) >> 1)
	const minI64 = -maxI64 - 1
	if (b > 0 && a > maxI64-b) || (b < 0 && a < minI64-b) {
		return 0, withDetails(CodeOverflow, "integer overflow in %s", map[string]any{
			"field": field, "left": a, "right": b,
		}, "%s: %d + %d", field, a, b)
	}
	return a + b, nil
}

func roundDiv(n, d int64, mode RoundingMode) (q, rem int64, err error) {
	if d == 0 {
		return 0, 0, coded(CodeOverflow, "division by zero")
	}
	if !mode.valid() {
		return 0, 0, coded(CodeVersion, "unknown rounding mode %q", mode)
	}
	const minI64 = -int64(^uint64(0)>>1) - 1
	if n == minI64 && d == -1 {
		return 0, 0, withDetails(CodeOverflow, "integer overflow in division", map[string]any{
			"left": n, "right": d,
		}, "%d / %d", n, d)
	}
	absN := absUint64(n)
	absD := absUint64(d)
	q = n / d
	rem = n % d
	if rem == 0 {
		return q, 0, nil
	}
	neg := (n < 0) != (d < 0)
	remAbs := absN % absD
	switch mode {
	case RoundTowardZero:
		return q, rem, nil
	case RoundHalfAwayFromZero:
		if remAbs >= (absD+1)/2 {
			if neg {
				q--
			} else {
				q++
			}
		}
		return q, n - q*d, nil
	case RoundHalfEven:
		if remAbs > absD-remAbs || (remAbs == absD-remAbs && (q%2 != 0)) {
			if neg {
				q--
			} else {
				q++
			}
		}
		return q, n - q*d, nil
	default:
		return 0, 0, coded(CodeVersion, "unknown rounding mode %q", mode)
	}
}

func absUint64(v int64) uint64 {
	if v < 0 {
		return uint64(-(v + 1)) + 1
	}
	return uint64(v)
}

// PermilleToWeight converts the legacy per-mille scale to internal
// per-ten-thousand units. It is exact and checked for overflow.
func PermilleToWeight(v int64) (int64, error) {
	out, err := mulChecked("weight_permille", v, 10)
	if err != nil {
		return 0, err
	}
	if out < 0 || out > WeightMax {
		return 0, withDetails(CodeOverflow, "weight outside internal range", map[string]any{
			"field": "weight_permille", "operand": v, "scale": 10,
		}, "weight_permille %d -> %d outside 0..%d", v, out, WeightMax)
	}
	return out, nil
}

// WeightToPermille converts internal per-ten-thousand units to the legacy
// scale. residual is the signed error from truncation/rounding.
func WeightToPermille(v int64, mode RoundingMode) (out, residual int64, err error) {
	out, residual, err = roundDiv(v, 10, mode)
	if err != nil {
		return 0, 0, err
	}
	if out < 0 || out > PermilleMax {
		return 0, residual, withDetails(CodeOverflow, "weight outside per-mille range", map[string]any{
			"field": "weight_pp10000", "operand": v, "divisor": 10,
		}, "weight %d / 10 = %d outside 0..%d", v, out, PermilleMax)
	}
	return out, residual, nil
}

// AbsToStrokedOffset encodes an absolute coordinate as an offset from an
// index-derived anchor using an integer quantum. recon + residual == abs.
func AbsToStrokedOffset(field string, abs, anchor, quantum int64, mode RoundingMode) (encoded, recon, residual int64, err error) {
	if quantum <= 0 {
		return 0, 0, 0, coded(CodeVersion, "coordinate quantum must be positive")
	}
	diff, err := addChecked(field, abs, -anchor)
	if err != nil {
		return 0, 0, 0, err
	}
	encoded, residual, err = roundDiv(diff, quantum, mode)
	if err != nil {
		return 0, 0, 0, err
	}
	scaled, err := mulChecked(field, encoded, quantum)
	if err != nil {
		return 0, 0, 0, err
	}
	recon, err = addChecked(field, anchor, scaled)
	if err != nil {
		return 0, 0, 0, err
	}
	residual = abs - recon
	return encoded, recon, residual, nil
}

func StrokedOffsetToAbs(field string, encoded, anchor, quantum int64) (int64, error) {
	if quantum <= 0 {
		return 0, coded(CodeVersion, "coordinate quantum must be positive")
	}
	scaled, err := mulChecked(field, encoded, quantum)
	if err != nil {
		return 0, err
	}
	return addChecked(field, anchor, scaled)
}

func alphaOver(dst, src uint8) uint8 {
	// Integer source-over: src + dst*(255-src)/255. The residual is rounded
	// half away from zero for a stable, documented raster rule.
	prod := int(dst) * (255 - int(src))
	v := int(src) + (prod+127)/255
	if v > 255 {
		v = 255
	}
	return uint8(v)
}

func clampU8(v int64) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

func isqrt(n int64) int64 {
	if n <= 0 {
		return 0
	}
	var x int64 = 1
	for x < n/x {
		x <<= 1
	}
	var y = (x + n/x) / 2
	for y < x {
		x = y
		y = (x + n/x) / 2
	}
	return x
}

func pct(v int64) string {
	return fmt.Sprintf("%d", v)
}
