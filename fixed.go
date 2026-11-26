package leasekit

import (
	"math"
	"strconv"
	"strings"
)

const (
	// MaxSafeMilli keeps tick/second multiplication and expiry addition away
	// from int64 overflow. It is about 100 years in milliseconds.
	MaxSafeMilli Milli = 100 * 365 * 24 * 60 * 60 * 1000
)

func checkedAddMilli(a, b Milli) (Milli, error) {
	if b > 0 && a > MaxSafeMilli-b {
		return 0, validationError(CodeCountOverflow, "time", "millisecond addition overflow near %d + %d", a, b)
	}
	if b < 0 && a < -MaxSafeMilli-b {
		return 0, validationError(CodeCountOverflow, "time", "millisecond subtraction overflow near %d + %d", a, b)
	}
	return a + b, nil
}

func checkedMulInt64(a, b int64) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	if a > math.MaxInt64/b || a < math.MinInt64/b {
		return 0, validationError(CodeCountOverflow, "time", "integer multiplication overflow: %d * %d", a, b)
	}
	return a * b, nil
}

// TicksToMS converts an integer tick count using the declared milliseconds per
// tick. The domain intentionally uses int64 milliseconds rather than float.
func TicksToMS(ticks int64, msPerTick int64) (Milli, error) {
	if msPerTick <= 0 {
		return 0, validationError(CodeInvalidDuration, "time.msPerTick", "must be positive, got %d", msPerTick)
	}
	ms, err := checkedMulInt64(ticks, msPerTick)
	if err != nil {
		return 0, err
	}
	if ms < 0 || Milli(ms) > MaxSafeMilli {
		return 0, validationError(CodeInvalidDuration, "time.ticks", "converted duration %dms is out of range", ms)
	}
	return Milli(ms), nil
}

// DecimalSecondsToMS parses a JSON number such as 2.05 exactly. Floats are not
// used, preventing a schema-scale boundary from being moved by binary rounding.
func DecimalSecondsToMS(raw string) (Milli, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, validationError(CodeTimeConversion, "time.seconds", "empty decimal")
	}
	negative := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	parts := strings.SplitN(s, ".", 2)
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, validationError(CodeTimeConversion, "time.seconds", "invalid whole seconds %q", raw)
	}
	msPart := int64(0)
	if len(parts) == 2 {
		frac := parts[1]
		if len(frac) > 3 {
			// Declared normalization policy is truncation toward zero only for
			// sub-millisecond digits. Generated samples use <=3 digits.
			frac = frac[:3]
		}
		if len(frac) > 0 {
			v, perr := strconv.ParseInt(frac, 10, 64)
			if perr != nil {
				return 0, validationError(CodeTimeConversion, "time.seconds", "invalid fractional seconds %q", raw)
			}
			for i := len(frac); i < 3; i++ {
				v *= 10
			}
			msPart = v
		}
	}
	secMS, err := checkedMulInt64(whole, 1000)
	if err != nil {
		return 0, err
	}
	total, err := checkedAddMilli(Milli(secMS), Milli(msPart))
	if err != nil {
		return 0, err
	}
	if negative {
		total = -total
	}
	if total < 0 || total > MaxSafeMilli {
		return 0, validationError(CodeInvalidDuration, "time.seconds", "converted duration %dms is out of range", total)
	}
	return total, nil
}
