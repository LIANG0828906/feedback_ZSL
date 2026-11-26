package xconv

import (
	"math"

	"screenrelay/internal/model"
)

type Rounding int

const (
	RoundFloor Rounding = iota
	RoundCeil
	RoundNearest
	RoundTruncate
)

func MulChecked(a, b int64, field string) (int64, error) {
	switch {
	case a == 0 || b == 0:
		return 0, nil
	case a == math.MinInt64 && b == -1:
		return 0, model.OvfError(field, a, b)
	case b == math.MinInt64 && a == -1:
		return 0, model.OvfError(field, a, b)
	}

	if a > 0 && b > 0 && a > math.MaxInt64/b {
		return 0, model.OvfError(field, a, b)
	}
	if a < 0 && b < 0 {
		na := -a
		nb := -b
		if na > math.MaxInt64/nb {
			return 0, model.OvfError(field, a, b)
		}
	}
	if a > 0 && b < 0 {
		if b < -1 && a > math.MaxInt64/(-b) {
			return 0, model.OvfError(field, a, b)
		}
	}
	if a < 0 && b > 0 {
		if a < -1 && -a > math.MaxInt64/b {
			return 0, model.OvfError(field, a, b)
		}
	}
	return a * b, nil
}

func AddChecked(a, b int64, field string) (int64, error) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, model.OvfError(field, a, b)
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, model.OvfError(field, a, b)
	}
	return a + b, nil
}

func SubChecked(a, b int64, field string) (int64, error) {
	if b > 0 && a < math.MinInt64+b {
		return 0, model.OvfError(field, a, b)
	}
	if b < 0 && a > math.MaxInt64+b {
		return 0, model.OvfError(field, a, b)
	}
	return a - b, nil
}

// DivModFloor returns q and r with q=floor(a/b), 0 <= r < b. b must be positive.
func DivModFloor(a, b int64) (q, r int64) {
	if b <= 0 {
		panic("xconv: divisor must be positive")
	}
	q = a / b
	r = a % b
	if r < 0 {
		q--
		r += b
	}
	return q, r
}

// DivModCeil returns ceil(a/b) and a non-negative distance from the exact numerator
// to the rounded result, expressed in numerator units.
func DivModCeil(a, b int64) (q, residualNumerator int64) {
	floorQ, rem := DivModFloor(a, b)
	if rem == 0 {
		return floorQ, 0
	}
	return floorQ + 1, b - rem
}

type ScaledResult struct {
	Value            int64 `json:"value"`
	Remainder        int64 `json:"remainder"`
	RoundingError    int64 `json:"rounding_error"`
	Denominator      int64 `json:"denominator"`
	ExactNumerator   int64 `json:"exact_numerator"`
	RoundedNumerator int64 `json:"rounded_numerator"`
}

// Scale rounds value*num/den without floating point. The remainder is always the
// non-negative numerator distance from floor(exact) to the exact value.
func Scale(value, num, den int64, mode Rounding, field string) (ScaledResult, error) {
	if den <= 0 {
		return ScaledResult{}, model.NewError(model.EInternal, "invalid scale denominator %d", den)
	}
	product, err := MulChecked(value, num, field)
	if err != nil {
		return ScaledResult{}, err
	}
	floorQ, rem := DivModFloor(product, den)
	result := ScaledResult{Remainder: rem, Denominator: den, ExactNumerator: product}
	switch mode {
	case RoundFloor, RoundTruncate:
		result.Value = floorQ
		result.RoundedNumerator = floorQ * den
		result.RoundingError = rem
	case RoundCeil:
		if rem == 0 {
			result.Value = floorQ
		} else {
			result.Value = floorQ + 1
		}
		result.RoundedNumerator = result.Value * den
		if rem == 0 {
			result.RoundingError = 0
		} else {
			result.RoundingError = den - rem
		}
	case RoundNearest:
		if 2*rem >= den {
			result.Value = floorQ + 1
			result.RoundingError = den - rem
		} else {
			result.Value = floorQ
			result.RoundingError = rem
		}
		result.RoundedNumerator = result.Value * den
	default:
		return ScaledResult{}, model.NewError(model.EInternal, "unsupported rounding mode %d", mode)
	}
	return result, nil
}

func LogicalToCoord(value int64, field string) (int64, error) {
	return MulChecked(value, model.InternalCoordUnit, field)
}

func CoordToLogicalFloor(value int64) (logical, remainderUnits int64) {
	return DivModFloor(value, model.InternalCoordUnit)
}

func AbsoluteToOffset(abs, origin model.Point, field string) (model.Point, error) {
	x, err := SubChecked(abs.X, origin.X, field+".x")
	if err != nil {
		return model.Point{}, err
	}
	y, err := SubChecked(abs.Y, origin.Y, field+".y")
	if err != nil {
		return model.Point{}, err
	}
	return model.Point{X: x, Y: y}, nil
}

func OffsetToAbsolute(offset, origin model.Point, field string) (model.Point, error) {
	x, err := AddChecked(offset.X, origin.X, field+".x")
	if err != nil {
		return model.Point{}, err
	}
	y, err := AddChecked(offset.Y, origin.Y, field+".y")
	if err != nil {
		return model.Point{}, err
	}
	return model.Point{X: x, Y: y}, nil
}

func MillisecondsToTick(ms int64, field string) (int64, error) {
	perMS := model.TicksPerSecond / 1000
	return MulChecked(ms, perMS, field)
}

func TickToMillisecondsFloor(tick int64) (ms, remainderTicks int64) {
	return DivModFloor(tick, model.TicksPerSecond/1000)
}

func SourceTimeToInternal(value int64, basis model.TimeUnit, field string) (int64, error) {
	if basis.InternalPerSource <= 0 {
		return 0, model.SchemaError(field, "invalid time basis multiplier")
	}
	return MulChecked(value, basis.InternalPerSource, field)
}

func InternalTimeToSourceFloor(tick int64, basis model.TimeUnit) (source, remainderInternalTicks int64) {
	return DivModFloor(tick, basis.InternalPerSource)
}
