// Package num provides overflow-checked, integer-only conversions. No floating
// point is used anywhere in the conversion path.
package num

import (
	"math"

	"audioforge/internal/errs"
	"audioforge/internal/model"
)

func CheckedMul(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if a > 0 && b > 0 && a > math.MaxInt64/b {
		return 0, false
	}
	if a < 0 && b < 0 && a < math.MaxInt64/b {
		return 0, false
	}
	if a > 0 && b < 0 && b < math.MinInt64/a {
		return 0, false
	}
	if a < 0 && b > 0 && a < math.MinInt64/b {
		return 0, false
	}
	return a * b, true
}

// ScaleDiv computes n*num/den with a declared integer rounding direction.
func ScaleDiv(n, num, den int64, rounding model.Rounding) (int64, bool) {
	if den == 0 {
		return 0, false
	}
	product, ok := CheckedMul(n, num)
	if !ok {
		return 0, false
	}
	if den < 0 {
		product = -product
		den = -den
	}
	q := product / den
	r := product % den
	if r < 0 {
		r = -r
	}
	switch rounding {
	case model.RoundCeil:
		if product > 0 && r != 0 {
			q++
		}
	case model.RoundNearest:
		if r >= (den+1)/2 {
			if product >= 0 {
				q++
			} else {
				q--
			}
		}
	}
	return q, true
}

func TimeToSamples(value, sampleRate, tickRate int64, unit model.TimeUnit, rounding model.Rounding) (int64, bool) {
	switch unit {
	case model.TimeTick:
		if tickRate <= 0 {
			return 0, false
		}
		return ScaleDiv(value, sampleRate, tickRate, rounding)
	case model.TimeMillisecond:
		return ScaleDiv(value, sampleRate, 1000, rounding)
	default:
		return 0, false
	}
}

func SamplesToTime(samples, sampleRate, tickRate int64, unit model.TimeUnit, rounding model.Rounding) (int64, bool) {
	switch unit {
	case model.TimeTick:
		if tickRate <= 0 {
			return 0, false
		}
		return ScaleDiv(samples, tickRate, sampleRate, rounding)
	case model.TimeMillisecond:
		return ScaleDiv(samples, 1000, sampleRate, rounding)
	default:
		return 0, false
	}
}

func IntensityToDuration(intensity, quantumSamples int64, rounding model.Rounding) (int64, error) {
	if intensity < 0 {
		return 0, errs.Overflow("", "intensity", "intensity must not be negative")
	}
	v, ok := ScaleDiv(intensity, quantumSamples, 1, rounding)
	if !ok {
		return 0, errs.Overflow("", "duration_samples", "integer overflow converting intensity to duration")
	}
	return v, nil
}

func DurationToIntensity(duration, quantumSamples int64, rounding model.Rounding) (int64, error) {
	if duration < 0 {
		return 0, errs.Overflow("", "duration_samples", "duration must not be negative")
	}
	v, ok := ScaleDiv(duration, 1, quantumSamples, rounding)
	if !ok {
		return 0, errs.Overflow("", "intensity", "integer overflow converting duration to intensity")
	}
	return v, nil
}

func Quantize(value, unit int64, rounding model.Rounding) (int64, bool) {
	if unit <= 0 {
		return 0, false
	}
	return ScaleDiv(value, 1, unit, rounding)
}
