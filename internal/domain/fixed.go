package domain

import (
	"math"
	"math/big"
	"strings"
)

// FixedStats makes fixed-point arithmetic observable. Overflow is always an
// error; saturation counters document intentional floors such as max(0, x).
type FixedStats struct {
	Overflows   int64 `json:"overflows"`
	Saturations int64 `json:"saturations"`
}

func (s *FixedStats) Add(other FixedStats) {
	s.Overflows += other.Overflows
	s.Saturations += other.Saturations
}

func CheckedMul(a, b int64, path string) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	z := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	if !z.IsInt64() {
		return 0, NewBizError(EFixedPointOverflow, path, "multiplication %d * %d exceeds int64", a, b)
	}
	return z.Int64(), nil
}

func CheckedAdd(a, b int64, path string) (int64, error) {
	z := new(big.Int).Add(big.NewInt(a), big.NewInt(b))
	if !z.IsInt64() {
		return 0, NewBizError(EFixedPointOverflow, path, "addition %d + %d exceeds int64", a, b)
	}
	return z.Int64(), nil
}

func CheckedSub(a, b int64, path string) (int64, error) {
	z := new(big.Int).Sub(big.NewInt(a), big.NewInt(b))
	if !z.IsInt64() {
		return 0, NewBizError(EFixedPointOverflow, path, "subtraction %d - %d exceeds int64", a, b)
	}
	return z.Int64(), nil
}

// MulDivFloor computes floor(value*mul/div) with an explicit overflow check.
func MulDivFloor(value, mul, div int64, path string) (int64, error) {
	if div <= 0 {
		return 0, NewBizError(EFixedPointOverflow, path, "non-positive divisor %d", div)
	}
	if value < 0 || mul < 0 {
		return 0, NewBizError(EFixedPointOverflow, path, "negative fixed-point operand %d*%d/%d", value, mul, div)
	}
	if value == 0 || mul == 0 {
		return 0, nil
	}
	z := new(big.Int).Mul(big.NewInt(value), big.NewInt(mul))
	z.Quo(z, big.NewInt(div))
	if !z.IsInt64() {
		return 0, NewBizError(EFixedPointOverflow, path, "muldiv %d*%d/%d exceeds int64", value, mul, div)
	}
	return z.Int64(), nil
}

func ClampNonNegative(value int64, stats *FixedStats) int64 {
	if value < 0 {
		if stats != nil {
			stats.Saturations++
		}
		return 0
	}
	return value
}

// ParseDecimalCores converts a decimal core count to integer millicores.
// At most three fractional digits are accepted so the conversion is exact.
func ParseDecimalCores(raw string, path string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, NewBizError(ESchemaRequired, path, "missing core capacity")
	}
	if strings.HasPrefix(raw, "-") {
		return 0, NewBizError(EInvalidCapacity, path, "negative core value %q", raw)
	}
	if strings.ContainsAny(raw, "eE+") {
		return 0, NewBizError(EFixedPointPrecision, path, "exponent notation is not exact for millicore conversion: %q", raw)
	}
	parts := strings.Split(raw, ".")
	if len(parts) > 2 || parts[0] == "" {
		return 0, NewBizError(ESchemaInvalid, path, "invalid decimal core value %q", raw)
	}
	whole := parts[0]
	frac := ""
	if len(parts) == 2 {
		frac = parts[1]
	}
	if len(frac) > 3 {
		return 0, NewBizError(EFixedPointPrecision, path, "core value %q has more than millicore precision", raw)
	}
	for len(frac) < 3 {
		frac += "0"
	}
	if whole == "" {
		whole = "0"
	}
	if !allDigits(whole) || (frac != "" && !allDigits(frac)) {
		return 0, NewBizError(ESchemaInvalid, path, "invalid decimal core value %q", raw)
	}
	wholeValue := int64(0)
	for _, r := range whole {
		d := int64(r - '0')
		if wholeValue > (math.MaxInt64-d)/10 {
			return 0, NewBizError(EFixedPointOverflow, path, "core value %q overflows int64 millicores", raw)
		}
		wholeValue = wholeValue*10 + d
	}
	wholeMilli, err := CheckedMul(wholeValue, 1000, path)
	if err != nil {
		return 0, err
	}
	fracMilli := int64(0)
	for _, r := range frac {
		fracMilli = fracMilli*10 + int64(r-'0')
	}
	if fracMilli >= 1000 {
		return 0, NewBizError(EFixedPointOverflow, path, "fractional millicores overflow for %q", raw)
	}
	return CheckedAdd(wholeMilli, fracMilli, path)
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func PercentToBasisPoints(percent int64, path string) (int64, error) {
	if percent < 0 {
		return 0, NewBizError(ENegativeQuota, path, "negative percentage %d", percent)
	}
	return CheckedMul(percent, 100, path)
}

func MillicoresToCores(milli int64) string {
	if milli%1000 == 0 {
		return big.NewInt(milli / 1000).String()
	}
	whole := milli / 1000
	frac := milli % 1000
	text := big.NewInt(frac).String()
	for len(text) < 3 {
		text = "0" + text
	}
	text = strings.TrimRight(text, "0")
	return big.NewInt(whole).String() + "." + text
}
