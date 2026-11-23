package core

import "math"

// Vec3 三维向量，所有几何计算统一使用 float64。
type Vec3 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

func (v Vec3) Add(o Vec3) Vec3      { return Vec3{v.X + o.X, v.Y + o.Y, v.Z + o.Z} }
func (v Vec3) Sub(o Vec3) Vec3      { return Vec3{v.X - o.X, v.Y - o.Y, v.Z - o.Z} }
func (v Vec3) Scale(s float64) Vec3 { return Vec3{v.X * s, v.Y * s, v.Z * s} }
func (v Vec3) Dot(o Vec3) float64   { return v.X*o.X + v.Y*o.Y + v.Z*o.Z }
func (v Vec3) Cross(o Vec3) Vec3 {
	return Vec3{
		v.Y*o.Z - v.Z*o.Y,
		v.Z*o.X - v.X*o.Z,
		v.X*o.Y - v.Y*o.X,
	}
}
func (v Vec3) Length() float64 { return math.Sqrt(v.Dot(v)) }

// Normalize 返回单位向量；零向量保持为零向量由调用方保证不发生。
func (v Vec3) Normalize() Vec3 {
	n := v.Length()
	if n == 0 {
		return Vec3{}
	}
	return v.Scale(1 / n)
}

// IsFinite 判断向量各分量是否为有限实数。
func (v Vec3) IsFinite() bool {
	return math.IsInf(v.X, 0) == false && math.IsNaN(v.X) == false &&
		math.IsInf(v.Y, 0) == false && math.IsNaN(v.Y) == false &&
		math.IsInf(v.Z, 0) == false && math.IsNaN(v.Z) == false
}

// AABB 轴对齐包围盒，Min 各分量必须 <= Max 对应分量。
type AABB struct {
	Min Vec3 `json:"min"`
	Max Vec3 `json:"max"`
}

// Valid 校验数值有限且最小角不大于最大角。
func (b AABB) Valid() bool {
	if !b.Min.IsFinite() || !b.Max.IsFinite() {
		return false
	}
	return b.Min.X <= b.Max.X && b.Min.Y <= b.Max.Y && b.Min.Z <= b.Max.Z
}

// Center 包围盒中心。
func (b AABB) Center() Vec3 {
	return b.Min.Add(b.Max).Scale(0.5)
}

// Corners 返回包围盒的 8 个角点。
func (b AABB) Corners() [8]Vec3 {
	mn, mx := b.Min, b.Max
	return [8]Vec3{
		{mn.X, mn.Y, mn.Z}, {mx.X, mn.Y, mn.Z},
		{mx.X, mx.Y, mn.Z}, {mn.X, mx.Y, mn.Z},
		{mn.X, mn.Y, mx.Z}, {mx.X, mn.Y, mx.Z},
		{mx.X, mx.Y, mx.Z}, {mn.X, mx.Y, mx.Z},
	}
}

// ClosestPoint 返回包围盒上距离 p 最近的点。
func (b AABB) ClosestPoint(p Vec3) Vec3 {
	clamp := func(x, lo, hi float64) float64 {
		if x < lo {
			return lo
		}
		if x > hi {
			return hi
		}
		return x
	}
	return Vec3{
		clamp(p.X, b.Min.X, b.Max.X),
		clamp(p.Y, b.Min.Y, b.Max.Y),
		clamp(p.Z, b.Min.Z, b.Max.Z),
	}
}

// Distance 返回点到包围盒的最近距离（盒内为 0）。
func (b AABB) Distance(p Vec3) float64 {
	return b.ClosestPoint(p).Sub(p).Length()
}

// IsNaN 判断 float64 是否为 NaN。
func IsNaN(x float64) bool { return math.IsNaN(x) }

// IsInf 判断 float64 是否为无穷。
func IsInf(x float64) bool { return math.IsInf(x, 0) }
