package skillcore

import (
	"fmt"
	"math/big"
)

type Unit string

const (
	UnitTick        Unit = "tick"
	UnitMillisecond Unit = "millisecond"
	UnitPermille    Unit = "permille"
	UnitPermyriad   Unit = "permyriad"
	UnitResource    Unit = "resource"
	UnitValue       Unit = "value"
)

func overflowError(field, unit, message string) *Error {
	return &Error{
		Code:    CodeOverflow,
		Field:   field,
		Path:    field,
		Unit:    unit,
		Message: message,
	}
}

// MulDivRounded computes value*mul/div using integer arithmetic only and the
// requested rounding direction. Every intermediate is arbitrary precision; the
// final result must fit int64.
func MulDivRounded(value, mul, div int64, rounding Rounding, field string, unit Unit) (int64, error) {
	if div == 0 {
		return 0, overflowError(field, string(unit), "division by zero")
	}
	num := new(big.Int).Mul(big.NewInt(value), big.NewInt(mul))
	den := big.NewInt(div)
	q, r := new(big.Int), new(big.Int)
	q.QuoRem(num, den, r)
	if r.Sign() != 0 {
		adjust := roundingAdjustment(r, den, rounding)
		if adjust != 0 {
			q.Add(q, big.NewInt(adjust))
		}
	}
	if !q.IsInt64() {
		return 0, overflowError(field, string(unit), "conversion result exceeds int64")
	}
	return q.Int64(), nil
}

// MulDivExact is the export-safe variant: a non-zero remainder is rejected so
// a high -> low -> high conversion cannot silently change the internal value.
func MulDivExact(value, mul, div int64, field string, unit Unit) (int64, bool, error) {
	if div == 0 {
		return 0, false, overflowError(field, string(unit), "division by zero")
	}
	num := new(big.Int).Mul(big.NewInt(value), big.NewInt(mul))
	den := big.NewInt(div)
	q, r := new(big.Int), new(big.Int)
	q.QuoRem(num, den, r)
	if !q.IsInt64() {
		return 0, false, overflowError(field, string(unit), "conversion result exceeds int64")
	}
	return q.Int64(), r.Sign() == 0, nil
}

func roundingAdjustment(remainder, denominator *big.Int, rounding Rounding) int64 {
	if remainder.Sign() == 0 || rounding == RoundTrunc {
		return 0
	}
	positive := denominator.Sign() > 0
	sameSign := (remainder.Sign() > 0) == positive
	switch rounding {
	case RoundFloor:
		if !sameSign {
			return -1
		}
		return 0
	case RoundCeil:
		if sameSign {
			return 1
		}
		return 0
	case RoundNearest:
		twice := new(big.Int).Abs(remainder)
		twice.Lsh(twice, 1)
		absDen := new(big.Int).Abs(denominator)
		if twice.Cmp(absDen) >= 0 {
			if sameSign {
				return 1
			}
			return -1
		}
		return 0
	default:
		return 0
	}
}

func MillisToTicks(ms, tickMillis int64, rounding Rounding, field string) (int64, error) {
	if tickMillis <= 0 {
		return 0, overflowError("tickMillis", string(UnitMillisecond), "tickMillis must be positive")
	}
	return MulDivRounded(ms, 1, tickMillis, rounding, field, UnitMillisecond)
}

func MillisToTicksExact(ms, tickMillis int64, field string) (int64, bool, error) {
	if tickMillis <= 0 {
		return 0, false, overflowError("tickMillis", string(UnitMillisecond), "tickMillis must be positive")
	}
	return MulDivExact(ms, 1, tickMillis, field, UnitMillisecond)
}

func TicksToMillis(ticks, tickMillis int64, rounding Rounding, field string) (int64, error) {
	if tickMillis <= 0 {
		return 0, overflowError("tickMillis", string(UnitMillisecond), "tickMillis must be positive")
	}
	return MulDivRounded(ticks, tickMillis, 1, rounding, field, UnitMillisecond)
}

func TicksToMillisExact(ticks, tickMillis int64, field string) (int64, bool, error) {
	if tickMillis <= 0 {
		return 0, false, overflowError("tickMillis", string(UnitMillisecond), "tickMillis must be positive")
	}
	return MulDivExact(ticks, tickMillis, 1, field, UnitMillisecond)
}

func PermilleToBP(value int64, rounding Rounding, field string) (int64, error) {
	return MulDivRounded(value, 10, 1, rounding, field, UnitPermille)
}

func PermilleToBPExact(value int64, field string) (int64, bool, error) {
	return MulDivExact(value, 10, 1, field, UnitPermille)
}

func BPToPermille(value int64, rounding Rounding, field string) (int64, error) {
	return MulDivRounded(value, 1, 10, rounding, field, UnitPermille)
}

func BPToPermilleExact(value int64, field string) (int64, bool, error) {
	return MulDivExact(value, 1, 10, field, UnitPermille)
}

func ScaleToInternal(value, sourceScale int64, rounding Rounding, field string) (int64, error) {
	if sourceScale <= 0 {
		return 0, overflowError(field, string(UnitResource), "resource scale must be positive")
	}
	return MulDivRounded(value, InternalScale, sourceScale, rounding, field, UnitResource)
}

func ScaleToInternalExact(value, sourceScale int64, field string) (int64, bool, error) {
	if sourceScale <= 0 {
		return 0, false, overflowError(field, string(UnitResource), "resource scale must be positive")
	}
	return MulDivExact(value, InternalScale, sourceScale, field, UnitResource)
}

func InternalToScale(value, targetScale int64, rounding Rounding, field string) (int64, error) {
	if targetScale <= 0 {
		return 0, overflowError(field, string(UnitResource), "resource scale must be positive")
	}
	return MulDivRounded(value, targetScale, InternalScale, rounding, field, UnitResource)
}

func InternalToScaleExact(value, targetScale int64, field string) (int64, bool, error) {
	if targetScale <= 0 {
		return 0, false, overflowError(field, string(UnitResource), "resource scale must be positive")
	}
	return MulDivExact(value, targetScale, InternalScale, field, UnitResource)
}

// ConvertSerializedDuration converts a declared duration to internal ticks.
func ConvertSerializedDuration(value int64, unit Unit, tickMillis int64, rounding Rounding, field string) (int64, error) {
	switch unit {
	case UnitTick:
		return value, nil
	case UnitMillisecond:
		return MillisToTicks(value, tickMillis, rounding, field)
	default:
		return 0, domainErr(CodeVersion, field, fmt.Sprintf("unsupported duration unit %q", unit))
	}
}

func EffectiveCooldown(baseTicks, cdrBP int64, rounding Rounding, field string) (int64, error) {
	if cdrBP < 0 || cdrBP > InternalScale {
		return 0, domainErr(CodeInput, field, "cooldown reduction must be in [0,10000]")
	}
	return MulDivRounded(baseTicks, InternalScale-cdrBP, InternalScale, rounding, field, UnitTick)
}
