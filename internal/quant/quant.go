// Package quant contains overflow-checked integer quantization.
//
// The presentation core never accumulates wall-clock time in float64. Source
// seconds are parsed as exact decimal values, converted to integer
// nanoseconds/samples, and then quantized to frames.
package quant

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"presentkernel/internal/domain"
)

const (
	ParamVersion = "quant-int-v1"

	ScalePermille   = 1000
	ScalePermyriad  = 10000
	NanosecondsPerS = int64(1_000_000_000)
)

type RoundingMode string

const (
	RoundFloor    RoundingMode = "floor"
	RoundCeil     RoundingMode = "ceil"
	RoundHalfUp   RoundingMode = "round-half-up"
	RoundTruncate RoundingMode = "truncate"
	DeclaredFloor RoundingMode = "declared-floor"
	DeclaredRound RoundingMode = "declared-round-half-up"
)

func NormalizeMode(mode string) RoundingMode {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "floor", "truncate", "declared-floor":
		return RoundFloor
	case "ceil":
		return RoundCeil
	case "round-half-up", "half-up", "declared-round-half-up":
		return RoundHalfUp
	default:
		return RoundHalfUp
	}
}

// Remainder records a discarded fractional quantization position.
type Remainder struct {
	Field       string `json:"field"`
	Value       int64  `json:"value"`
	Numerator   int64  `json:"numerator"`
	Denominator int64  `json:"denominator"`
	Remainder   int64  `json:"remainder"`
	Mode        string `json:"mode"`
	Result      int64  `json:"result"`
}

func checkedMul(a, b int64, field string, operands []int64) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	if a > math.MaxInt64/b || a < math.MinInt64/b {
		return 0, domain.OvfError(field, operands, "integer multiplication overflow")
	}
	return a * b, nil
}

func roundQuotient(num, den int64, mode RoundingMode) (int64, int64) {
	if den < 0 {
		num, den = -num, -den
	}
	q := num / den
	r := num % den
	negative := num < 0
	absR := r
	if absR < 0 {
		absR = -absR
	}
	switch mode {
	case RoundCeil:
		if r != 0 && !negative {
			q++
		}
	case RoundHalfUp:
		if 2*absR >= den {
			if negative {
				q--
			} else {
				q++
			}
		}
	case RoundFloor, RoundTruncate:
		if r != 0 && negative {
			q--
		}
	}
	return q, r
}

// DecimalToNanoseconds parses a JSON-friendly decimal number of seconds.
func DecimalToNanoseconds(field, decimalSeconds string, mode RoundingMode) (int64, Remainder, error) {
	rem := Remainder{Field: field, Denominator: NanosecondsPerS, Mode: string(mode)}
	s := strings.TrimSpace(decimalSeconds)
	if s == "" {
		return 0, rem, domain.SchemaError(field, "empty seconds value")
	}
	negative := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	parts := strings.SplitN(s, ".", 2)
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, rem, domain.SchemaError(field, "invalid decimal seconds %q", decimalSeconds)
	}
	var frac string
	if len(parts) == 2 {
		frac = parts[1]
	}
	roundUp := false
	if len(frac) > 9 {
		if tenth, perr := strconv.ParseInt(frac[9:10], 10, 64); perr == nil && tenth >= 5 {
			roundUp = true
		}
		frac = frac[:9]
	}
	padded := frac + strings.Repeat("0", 9-len(frac))
	fracN, err := strconv.ParseInt(padded, 10, 64)
	if err != nil {
		return 0, rem, domain.SchemaError(field, "invalid fractional seconds %q", decimalSeconds)
	}
	if roundUp {
		fracN++
	}
	ns, err := checkedMul(whole, NanosecondsPerS, field, []int64{whole, NanosecondsPerS})
	if err != nil {
		return 0, rem, err
	}
	if ns > math.MaxInt64-fracN {
		return 0, rem, domain.OvfError(field, []int64{whole, NanosecondsPerS}, "nanosecond total overflow")
	}
	ns += fracN
	if negative {
		ns = -ns
	}
	rem.Value = ns
	rem.Result = ns
	return ns, rem, nil
}

func MsToSamples(field string, ms int64, sampleRateHz int, mode RoundingMode) (int64, Remainder, error) {
	num, err := checkedMul(ms, int64(sampleRateHz), field, []int64{ms, int64(sampleRateHz)})
	if err != nil {
		return 0, Remainder{}, err
	}
	q, r := roundQuotient(num, 1000, mode)
	rem := Remainder{Field: field, Value: ms, Numerator: num, Denominator: 1000, Remainder: r, Mode: string(mode), Result: q}
	return q, rem, nil
}

func SecondsToFrames(field, seconds string, fps int, mode RoundingMode) (int, Remainder, error) {
	ns, _, err := DecimalToNanoseconds(field, seconds, mode)
	if err != nil {
		return 0, Remainder{}, err
	}
	num, err := checkedMul(ns, int64(fps), field, []int64{ns, int64(fps)})
	if err != nil {
		return 0, Remainder{}, err
	}
	q, r := roundQuotient(num, NanosecondsPerS, mode)
	if q > math.MaxInt32 || q < 0 {
		return 0, Remainder{}, domain.OvfError(field, []int64{ns, int64(fps)}, "frame number outside int32 range")
	}
	rem := Remainder{Field: field, Value: ns, Numerator: num, Denominator: NanosecondsPerS, Remainder: r, Mode: string(mode), Result: q}
	return int(q), rem, nil
}

func MsToFrames(field string, ms int64, fps int, mode RoundingMode) (int, Remainder, error) {
	num, err := checkedMul(ms, int64(fps), field, []int64{ms, int64(fps)})
	if err != nil {
		return 0, Remainder{}, err
	}
	q, r := roundQuotient(num, 1000, mode)
	if q > math.MaxInt32 || q < 0 {
		return 0, Remainder{}, domain.OvfError(field, []int64{ms, int64(fps)}, "frame number outside int32 range")
	}
	rem := Remainder{Field: field, Value: ms, Numerator: num, Denominator: 1000, Remainder: r, Mode: string(mode), Result: q}
	return int(q), rem, nil
}

func SamplesToFrames(field string, samples int64, sampleRateHz, fps int, mode RoundingMode) (int, Remainder, error) {
	if sampleRateHz <= 0 {
		return 0, Remainder{}, domain.SchemaError(field, "sample rate must be positive")
	}
	num, err := checkedMul(samples, int64(fps), field, []int64{samples, int64(fps)})
	if err != nil {
		return 0, Remainder{}, err
	}
	q, r := roundQuotient(num, int64(sampleRateHz), mode)
	if q > math.MaxInt32 || q < 0 {
		return 0, Remainder{}, domain.OvfError(field, []int64{samples, int64(fps)}, "frame number outside int32 range")
	}
	rem := Remainder{Field: field, Value: samples, Numerator: num, Denominator: int64(sampleRateHz), Remainder: r, Mode: string(mode), Result: q}
	return int(q), rem, nil
}

func FramesToMs(field string, frame int, fps int, mode RoundingMode) (int64, Remainder, error) {
	num, err := checkedMul(int64(frame), 1000, field, []int64{int64(frame), 1000})
	if err != nil {
		return 0, Remainder{}, err
	}
	q, r := roundQuotient(num, int64(fps), mode)
	rem := Remainder{Field: field, Value: int64(frame), Numerator: num, Denominator: int64(fps), Remainder: r, Mode: string(mode), Result: q}
	return q, rem, nil
}

func SamplesToNanoseconds(field string, samples int64, sampleRateHz int, mode RoundingMode) (int64, Remainder, error) {
	if sampleRateHz <= 0 {
		return 0, Remainder{}, domain.SchemaError(field, "sample rate must be positive")
	}
	num, err := checkedMul(samples, NanosecondsPerS, field, []int64{samples, NanosecondsPerS})
	if err != nil {
		return 0, Remainder{}, err
	}
	q, r := roundQuotient(num, int64(sampleRateHz), mode)
	rem := Remainder{Field: field, Value: samples, Numerator: num, Denominator: int64(sampleRateHz), Remainder: r, Mode: string(mode), Result: q}
	return q, rem, nil
}

func NanosecondsToSamples(field string, ns int64, sampleRateHz int, mode RoundingMode) (int64, Remainder, error) {
	if sampleRateHz <= 0 {
		return 0, Remainder{}, domain.SchemaError(field, "sample rate must be positive")
	}
	num, err := checkedMul(ns, int64(sampleRateHz), field, []int64{ns, int64(sampleRateHz)})
	if err != nil {
		return 0, Remainder{}, err
	}
	q, r := roundQuotient(num, NanosecondsPerS, mode)
	rem := Remainder{Field: field, Value: ns, Numerator: num, Denominator: NanosecondsPerS, Remainder: r, Mode: string(mode), Result: q}
	return q, rem, nil
}

func SamplesToMs(field string, samples int64, sampleRateHz int, mode RoundingMode) (int64, Remainder, error) {
	if sampleRateHz <= 0 {
		return 0, Remainder{}, domain.SchemaError(field, "sample rate must be positive")
	}
	num, err := checkedMul(samples, 1000, field, []int64{samples, 1000})
	if err != nil {
		return 0, Remainder{}, err
	}
	q, r := roundQuotient(num, int64(sampleRateHz), mode)
	rem := Remainder{Field: field, Value: samples, Numerator: num, Denominator: int64(sampleRateHz), Remainder: r, Mode: string(mode), Result: q}
	return q, rem, nil
}

// ScaleFixedPoint converts between permille and permyriad scales. For
// lossless round trips generated samples use values aligned to the coarser
// scale; unaligned values return a nonzero remainder/truncation marker.
func ScaleFixedPoint(field string, value int, fromScale, toScale int, mode RoundingMode) (int, Remainder, error) {
	if fromScale <= 0 || toScale <= 0 {
		return 0, Remainder{}, domain.SchemaError(field, "fixed-point scales must be positive")
	}
	num, err := checkedMul(int64(value), int64(toScale), field, []int64{int64(value), int64(toScale)})
	if err != nil {
		return 0, Remainder{}, err
	}
	q, r := roundQuotient(num, int64(fromScale), mode)
	if q > math.MaxInt32 || q < math.MinInt32 {
		return 0, Remainder{}, domain.OvfError(field, []int64{int64(value), int64(toScale)}, "scaled value outside int32 range")
	}
	rem := Remainder{Field: field, Value: int64(value), Numerator: num, Denominator: int64(fromScale), Remainder: r, Mode: string(mode), Result: q}
	return int(q), rem, nil
}

func MustScale(value, from, to int) int {
	v, _, err := ScaleFixedPoint("test", value, from, to, RoundFloor)
	if err != nil {
		panic(err)
	}
	return v
}

func FormatDecimal(whole int64, nanos int64) string {
	return fmt.Sprintf("%d.%09d", whole, nanos)
}
