package gait

import "math"

func sqrt(v float64) float64     { return math.Sqrt(v) }
func sin(v float64) float64      { return math.Sin(v) }
func cos(v float64) float64      { return math.Cos(v) }
func abs(v float64) float64      { return math.Abs(v) }
func round(v float64) float64    { return math.Round(v) }
func floor(v float64) float64    { return math.Floor(v) }
func ceil(v float64) float64     { return math.Ceil(v) }
func atan2(y, x float64) float64 { return math.Atan2(y, x) }
func acos(v float64) float64     { return math.Acos(v) }
func pi() float64                { return math.Pi }
func isFinite(v float64) bool    { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
