package geom

import "math"

// Vec3 is a three component vector used for offsets, positions and directions.
type Vec3 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

func (v Vec3) Add(o Vec3) Vec3 {
	return Vec3{v.X + o.X, v.Y + o.Y, v.Z + o.Z}
}

func (v Vec3) Sub(o Vec3) Vec3 {
	return Vec3{v.X - o.X, v.Y - o.Y, v.Z - o.Z}
}

func (v Vec3) Mul(s float64) Vec3 {
	return Vec3{v.X * s, v.Y * s, v.Z * s}
}

func (v Vec3) Dot(o Vec3) float64 {
	return v.X*o.X + v.Y*o.Y + v.Z*o.Z
}

func (v Vec3) Cross(o Vec3) Vec3 {
	return Vec3{
		X: v.Y*o.Z - v.Z*o.Y,
		Y: v.Z*o.X - v.X*o.Z,
		Z: v.X*o.Y - v.Y*o.X,
	}
}

func (v Vec3) LengthSquared() float64 {
	return v.Dot(v)
}

func (v Vec3) Length() float64 {
	return math.Sqrt(v.LengthSquared())
}

func (v Vec3) Normalize() Vec3 {
	l := v.Length()
	if l < 1e-15 {
		return Vec3{}
	}
	return v.Mul(1 / l)
}

func (v Vec3) Lerp(o Vec3, t float64) Vec3 {
	return v.Add(o.Sub(v).Mul(t))
}

func (v Vec3) IsFinite() bool {
	return isFinite(v.X) && isFinite(v.Y) && isFinite(v.Z)
}

func (v Vec3) AlmostEqual(o Vec3, eps float64) bool {
	return math.Abs(v.X-o.X) <= eps &&
		math.Abs(v.Y-o.Y) <= eps &&
		math.Abs(v.Z-o.Z) <= eps
}

func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}
