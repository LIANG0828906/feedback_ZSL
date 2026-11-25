package fog

import "math"

const (
	FixedScale      int64 = 1_000_000
	MaxCoefficient        = 10.0
	MaxDensityVoxel       = 255
)

type Fixed int64

func FixedFromFloat(v float64) Fixed {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return Fixed(math.Round(v * float64(FixedScale)))
}

func (f Fixed) Float64() float64 {
	return float64(f) / float64(FixedScale)
}

func (f Fixed) Add(other Fixed) Fixed { return f + other }

func (f Fixed) Sub(other Fixed) Fixed { return f - other }

func (f Fixed) Mul(other Fixed) Fixed {
	return Fixed((int64(f) * int64(other)) / FixedScale)
}

type Vec3 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

func (v Vec3) Add(o Vec3) Vec3 { return Vec3{v.X + o.X, v.Y + o.Y, v.Z + o.Z} }

func (v Vec3) Sub(o Vec3) Vec3 { return Vec3{v.X - o.X, v.Y - o.Y, v.Z - o.Z} }

func (v Vec3) Scale(s float64) Vec3 { return Vec3{v.X * s, v.Y * s, v.Z * s} }

func (v Vec3) Dot(o Vec3) float64 { return v.X*o.X + v.Y*o.Y + v.Z*o.Z }

func (v Vec3) Len() float64 { return math.Sqrt(v.Dot(v)) }

func (v Vec3) Normalize() Vec3 {
	n := v.Len()
	if n == 0 {
		return Vec3{}
	}
	return v.Scale(1 / n)
}

type FixedVec3 struct {
	X Fixed `json:"x"`
	Y Fixed `json:"y"`
	Z Fixed `json:"z"`
}

func fixedVecFromFloat(v Vec3) FixedVec3 {
	return FixedVec3{X: FixedFromFloat(v.X), Y: FixedFromFloat(v.Y), Z: FixedFromFloat(v.Z)}
}

func (v FixedVec3) Float64() Vec3 {
	return Vec3{X: v.X.Float64(), Y: v.Y.Float64(), Z: v.Z.Float64()}
}

type AABB struct {
	Min FixedVec3 `json:"min"`
	Max FixedVec3 `json:"max"`
}

func (b AABB) Contains(p Vec3) bool {
	min := b.Min.Float64()
	max := b.Max.Float64()
	return p.X >= min.X && p.X <= max.X && p.Y >= min.Y && p.Y <= max.Y && p.Z >= min.Z && p.Z <= max.Z
}
