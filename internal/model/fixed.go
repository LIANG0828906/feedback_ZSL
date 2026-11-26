package model

import (
	"math"
)

type QuantizedT struct {
	Ticks    int64
	Residual float64
}

func QuantizeFloor(t float64, scale int64) QuantizedT {
	x := t * float64(scale)
	q := math.Floor(x)
	return QuantizedT{Ticks: int64(q), Residual: x - q}
}

func QuantizeCeil(t float64, scale int64) QuantizedT {
	x := t * float64(scale)
	q := math.Ceil(x)
	return QuantizedT{Ticks: int64(q), Residual: x - q}
}

func TicksToFloat(ticks int64, scale int64) float64 {
	return float64(ticks) / float64(scale)
}

func IsQuantized(v float64, scale int64) bool {
	if scale <= 0 || !isFinite(v) {
		return false
	}
	x := v * float64(scale)
	return math.Abs(x-math.Round(x)) <= 1e-7
}
