package convert

import (
	"math"

	"audiobus/errs"
)

type Rounding int

const (
	Floor Rounding = iota
	HalfUp
	HalfDown
)

func MulChecked(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if a > math.MaxInt64/b || a < math.MinInt64/b {
		if b > 0 && (a > math.MaxInt64/b || a < math.MinInt64/b) {
			return 0, false
		}
		if b < 0 && (a < math.MaxInt64/b || a > math.MinInt64/b) {
			return 0, false
		}
	}
	return a * b, true
}

func ScaleChecked(value, numerator, denominator int64, rounding Rounding) (int64, *errs.Error) {
	if denominator <= 0 {
		return 0, errs.New(errs.ESchema, "conversion denominator must be positive")
	}
	product, ok := MulChecked(value, numerator)
	if !ok {
		return 0, &errs.Error{Code: errs.EOvf, Reason: "integer scale overflow", RawValue: value}
	}
	switch rounding {
	case Floor:
		return floorDiv(product, denominator), nil
	case HalfUp:
		if product >= 0 {
			return (2*product + denominator) / (2 * denominator), nil
		}
		return -((-2*product + denominator) / (2 * denominator)), nil
	case HalfDown:
		if product >= 0 {
			return (2*product + denominator - 1) / (2 * denominator), nil
		}
		return -((-2*product + denominator - 1) / (2 * denominator)), nil
	default:
		return floorDiv(product, denominator), nil
	}
}

func floorDiv(a, b int64) int64 {
	q := a / b
	r := a % b
	if r != 0 && ((r < 0) != (b < 0)) {
		q--
	}
	return q
}

func MsToSamples(ms, sampleRate int64) (int64, *errs.Error) {
	product, ok := MulChecked(ms, sampleRate)
	if !ok {
		return 0, &errs.Error{Code: errs.EOvf, Reason: "ms to samples overflow", RawValue: ms}
	}
	return floorDiv(product, 1000), nil
}

func SamplesToMs(samples, sampleRate int64) (int64, *errs.Error) {
	return ScaleChecked(samples, 1000, sampleRate, Floor)
}

func FrameToSamples(frame, frameSamples int64) (int64, *errs.Error) {
	v, ok := MulChecked(frame, frameSamples)
	if !ok {
		return 0, &errs.Error{Code: errs.EOvf, Reason: "frame to samples overflow", RawValue: frame}
	}
	return v, nil
}

func SamplesToFrame(samples, frameSamples int64, rounding Rounding) (int64, *errs.Error) {
	return ScaleChecked(samples, 1, frameSamples, rounding)
}
