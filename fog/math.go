package fog

import (
	"math"
)

const DefaultFixedScale int64 = 1_000_000

type Vec3 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

func V(x, y, z float64) Vec3 { return Vec3{X: x, Y: y, Z: z} }

func (v Vec3) Add(o Vec3) Vec3 { return Vec3{v.X + o.X, v.Y + o.Y, v.Z + o.Z} }
func (v Vec3) Sub(o Vec3) Vec3 { return Vec3{v.X - o.X, v.Y - o.Y, v.Z - o.Z} }
func (v Vec3) Mul(s float64) Vec3 {
	return Vec3{v.X * s, v.Y * s, v.Z * s}
}
func (v Vec3) Dot(o Vec3) float64 { return v.X*o.X + v.Y*o.Y + v.Z*o.Z }
func (v Vec3) LengthSquared() float64 {
	return v.Dot(v)
}
func (v Vec3) Length() float64 { return math.Sqrt(v.LengthSquared()) }
func (v Vec3) Normalize() Vec3 {
	l := v.Length()
	if l == 0 {
		return v
	}
	return v.Mul(1 / l)
}
func (v Vec3) Cross(o Vec3) Vec3 {
	return Vec3{
		X: v.Y*o.Z - v.Z*o.Y,
		Y: v.Z*o.X - v.X*o.Z,
		Z: v.X*o.Y - v.Y*o.X,
	}
}
func (v Vec3) IsFinite() bool {
	return isFinite(v.X) && isFinite(v.Y) && isFinite(v.Z)
}

func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

type AABB struct {
	Min Vec3 `json:"min"`
	Max Vec3 `json:"max"`
}

func (b AABB) Size() Vec3 {
	return Vec3{b.Max.X - b.Min.X, b.Max.Y - b.Min.Y, b.Max.Z - b.Min.Z}
}

func (b AABB) Center() Vec3 {
	return b.Min.Add(b.Max).Mul(0.5)
}

func (b AABB) Contains(p Vec3) bool {
	return p.X >= b.Min.X && p.X <= b.Max.X &&
		p.Y >= b.Min.Y && p.Y <= b.Max.Y &&
		p.Z >= b.Min.Z && p.Z <= b.Max.Z
}

// Quantize converts a world-space scalar to the declared fixed-point domain.
// The residual is measured in world units and is retained on every interval
// endpoint for auditing. Quantization is the only place interval endpoints are
// rounded.
func Quantize(v float64, scale int64) (int64, float64) {
	if scale <= 0 {
		return 0, v
	}
	q := int64(math.Round(v * float64(scale)))
	return q, float64(q)/float64(scale) - v
}

func FixedToFloat(v int64, scale int64) float64 {
	if scale <= 0 {
		return float64(v)
	}
	return float64(v) / float64(scale)
}
