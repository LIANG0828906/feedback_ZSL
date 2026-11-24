package model

import (
	"encoding/json"
	"math"
)

type Vec3 [3]float64

func V3(x, y, z float64) Vec3 { return Vec3{x, y, z} }

func (v Vec3) Add(o Vec3) Vec3    { return Vec3{v[0] + o[0], v[1] + o[1], v[2] + o[2]} }
func (v Vec3) Sub(o Vec3) Vec3    { return Vec3{v[0] - o[0], v[1] - o[1], v[2] - o[2]} }
func (v Vec3) Mul(s float64) Vec3 { return Vec3{v[0] * s, v[1] * s, v[2] * s} }
func (v Vec3) Dot(o Vec3) float64 { return v[0]*o[0] + v[1]*o[1] + v[2]*o[2] }
func (v Vec3) Length2() float64   { return v.Dot(v) }
func (v Vec3) Length() float64    { return math.Sqrt(v.Length2()) }
func (v Vec3) IsFinite() bool     { return isFinite(v[0]) && isFinite(v[1]) && isFinite(v[2]) }
func (v Vec3) IsZero(eps float64) bool {
	return math.Abs(v[0]) <= eps && math.Abs(v[1]) <= eps && math.Abs(v[2]) <= eps
}

func (v Vec3) Cross(o Vec3) Vec3 {
	return Vec3{
		v[1]*o[2] - v[2]*o[1],
		v[2]*o[0] - v[0]*o[2],
		v[0]*o[1] - v[1]*o[0],
	}
}

func (v Vec3) Normalize() (Vec3, bool) {
	l := v.Length()
	if l <= 1e-15 || !isFinite(l) {
		return Vec3{}, false
	}
	return v.Mul(1 / l), true
}

func (v Vec3) MulColor(c Color) Color {
	return Color{v[0] * c[0], v[1] * c[1], v[2] * c[2]}
}

func (v Vec3) MarshalJSON() ([]byte, error) {
	return json.Marshal([3]float64(v))
}

func (v *Vec3) UnmarshalJSON(b []byte) error {
	var raw [3]float64
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*v = Vec3(raw)
	return nil
}

type Color [3]float64
type Color3 = Color

func C3(r, g, b float64) Color { return Color{r, g, b} }

func (c Color) Add(o Color) Color   { return Color{c[0] + o[0], c[1] + o[1], c[2] + o[2]} }
func (c Color) Sub(o Color) Color   { return Color{c[0] - o[0], c[1] - o[1], c[2] - o[2]} }
func (c Color) Mul(s float64) Color { return Color{c[0] * s, c[1] * s, c[2] * s} }
func (c Color) MulColor(o Color) Color {
	return Color{c[0] * o[0], c[1] * o[1], c[2] * o[2]}
}
func (c Color) IsFinite() bool { return isFinite(c[0]) && isFinite(c[1]) && isFinite(c[2]) }
func (c Color) Max() float64 {
	return math.Max(c[0], math.Max(c[1], c[2]))
}

func (c Color) MarshalJSON() ([]byte, error) { return json.Marshal([3]float64(c)) }
func (c *Color) UnmarshalJSON(b []byte) error {
	var raw [3]float64
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*c = Color(raw)
	return nil
}

func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}
