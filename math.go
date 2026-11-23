package spatialpick

import (
	"encoding/json"
	"math"
)

type Vec2 struct {
	X float64
	Y float64
}

func (v Vec2) MarshalJSON() ([]byte, error) {
	return json.Marshal([2]float64{v.X, v.Y})
}

func (v *Vec2) UnmarshalJSON(b []byte) error {
	var a [2]float64
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	v.X, v.Y = a[0], a[1]
	return nil
}

type Vec3 struct {
	X float64
	Y float64
	Z float64
}

func V3(x, y, z float64) Vec3 { return Vec3{X: x, Y: y, Z: z} }

func (v Vec3) MarshalJSON() ([]byte, error) {
	return json.Marshal([3]float64{v.X, v.Y, v.Z})
}

func (v *Vec3) UnmarshalJSON(b []byte) error {
	var a [3]float64
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	v.X, v.Y, v.Z = a[0], a[1], a[2]
	return nil
}

func (v Vec3) Add(o Vec3) Vec3        { return V3(v.X+o.X, v.Y+o.Y, v.Z+o.Z) }
func (v Vec3) Sub(o Vec3) Vec3        { return V3(v.X-o.X, v.Y-o.Y, v.Z-o.Z) }
func (v Vec3) Mul(s float64) Vec3     { return V3(v.X*s, v.Y*s, v.Z*s) }
func (v Vec3) Dot(o Vec3) float64     { return v.X*o.X + v.Y*o.Y + v.Z*o.Z }
func (v Vec3) Cross(o Vec3) Vec3      { return V3(v.Y*o.Z-v.Z*o.Y, v.Z*o.X-v.X*o.Z, v.X*o.Y-v.Y*o.X) }
func (v Vec3) LengthSquared() float64 { return v.Dot(v) }
func (v Vec3) Length() float64        { return math.Sqrt(v.LengthSquared()) }
func (v Vec3) Abs() Vec3              { return V3(math.Abs(v.X), math.Abs(v.Y), math.Abs(v.Z)) }
func (v Vec3) IsFinite() bool         { return finite(v.X) && finite(v.Y) && finite(v.Z) }
func (v Vec3) Min(o Vec3) Vec3        { return V3(math.Min(v.X, o.X), math.Min(v.Y, o.Y), math.Min(v.Z, o.Z)) }
func (v Vec3) Max(o Vec3) Vec3        { return V3(math.Max(v.X, o.X), math.Max(v.Y, o.Y), math.Max(v.Z, o.Z)) }
func (v Vec3) Component(i int) float64 {
	switch i {
	case 0:
		return v.X
	case 1:
		return v.Y
	default:
		return v.Z
	}
}

func (v Vec3) Normalize() (Vec3, bool) {
	l := v.Length()
	if l == 0 || !finite(l) {
		return Vec3{}, false
	}
	return v.Mul(1 / l), true
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

type Rect struct {
	X0 float64
	Y0 float64
	X1 float64
	Y1 float64
}

func (r Rect) MarshalJSON() ([]byte, error) {
	return json.Marshal([4]float64{r.X0, r.Y0, r.X1, r.Y1})
}

func (r *Rect) UnmarshalJSON(b []byte) error {
	var a [4]float64
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	r.X0, r.Y0, r.X1, r.Y1 = a[0], a[1], a[2], a[3]
	return nil
}

func (r Rect) Normalized() Rect {
	x0, x1 := math.Min(r.X0, r.X1), math.Max(r.X0, r.X1)
	y0, y1 := math.Min(r.Y0, r.Y1), math.Max(r.Y0, r.Y1)
	return Rect{X0: x0, Y0: y0, X1: x1, Y1: y1}
}

type AABB struct {
	Min Vec3 `json:"min"`
	Max Vec3 `json:"max"`
}

func EmptyAABB() AABB {
	return AABB{
		Min: V3(math.Inf(1), math.Inf(1), math.Inf(1)),
		Max: V3(math.Inf(-1), math.Inf(-1), math.Inf(-1)),
	}
}

func (a AABB) IsValid() bool {
	return a.Min.IsFinite() && a.Max.IsFinite() &&
		a.Min.X <= a.Max.X && a.Min.Y <= a.Max.Y && a.Min.Z <= a.Max.Z
}

func (a AABB) Union(o AABB) AABB {
	if !a.IsValid() {
		return o
	}
	if !o.IsValid() {
		return a
	}
	return AABB{Min: a.Min.Min(o.Min), Max: a.Max.Max(o.Max)}
}

func (a AABB) Overlaps(o AABB) bool {
	return a.Min.X <= o.Max.X && a.Max.X >= o.Min.X &&
		a.Min.Y <= o.Max.Y && a.Max.Y >= o.Min.Y &&
		a.Min.Z <= o.Max.Z && a.Max.Z >= o.Min.Z
}

func (a AABB) Expanded(e float64) AABB {
	d := V3(e, e, e)
	return AABB{Min: a.Min.Sub(d), Max: a.Max.Add(d)}
}

func (a AABB) Center() Vec3 {
	return a.Min.Add(a.Max).Mul(0.5)
}

func (a AABB) Size() Vec3 {
	return a.Max.Sub(a.Min)
}

func (a AABB) Diagonal() float64 {
	return a.Size().Length()
}

type Sphere struct {
	Center Vec3    `json:"center"`
	Radius float64 `json:"radius"`
}

// Mat4 is a row-major affine matrix.
type Mat4 [16]float64

func IdentityMat4() Mat4 {
	return Mat4{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}
}

func TranslationMat4(v Vec3) Mat4 {
	m := IdentityMat4()
	m[3], m[7], m[11] = v.X, v.Y, v.Z
	return m
}

func ScaleMat4(v Vec3) Mat4 {
	m := IdentityMat4()
	m[0], m[5], m[10] = v.X, v.Y, v.Z
	return m
}

func RotationXMat4(rad float64) Mat4 {
	c, s := math.Cos(rad), math.Sin(rad)
	m := IdentityMat4()
	m[5], m[6], m[9], m[10] = c, -s, s, c
	return m
}

func RotationYMat4(rad float64) Mat4 {
	c, s := math.Cos(rad), math.Sin(rad)
	m := IdentityMat4()
	m[0], m[2], m[8], m[10] = c, s, -s, c
	return m
}

func RotationZMat4(rad float64) Mat4 {
	c, s := math.Cos(rad), math.Sin(rad)
	m := IdentityMat4()
	m[0], m[1], m[4], m[5] = c, -s, s, c
	return m
}

// RotationEulerDegrees applies X, then Y, then Z in world-aligned axes.
func RotationEulerDegrees(v Vec3) Mat4 {
	return Mat4Multiply(RotationZMat4(v.Z*math.Pi/180), Mat4Multiply(RotationYMat4(v.Y*math.Pi/180), RotationXMat4(v.X*math.Pi/180)))
}

func Mat4Multiply(a, b Mat4) Mat4 {
	var out Mat4
	for i := 0; i < 4; i++ {
		for j := 0; j < 4; j++ {
			var s float64
			for k := 0; k < 4; k++ {
				s += a[i*4+k] * b[k*4+j]
			}
			out[i*4+j] = s
		}
	}
	return out
}

func (m Mat4) TransformPoint(v Vec3) Vec3 {
	return V3(
		m[0]*v.X+m[1]*v.Y+m[2]*v.Z+m[3],
		m[4]*v.X+m[5]*v.Y+m[6]*v.Z+m[7],
		m[8]*v.X+m[9]*v.Y+m[10]*v.Z+m[11],
	)
}

func (m Mat4) TransformVector(v Vec3) Vec3 {
	return V3(
		m[0]*v.X+m[1]*v.Y+m[2]*v.Z,
		m[4]*v.X+m[5]*v.Y+m[6]*v.Z,
		m[8]*v.X+m[9]*v.Y+m[10]*v.Z,
	)
}

func (m Mat4) IsFinite() bool {
	for _, v := range m {
		if !finite(v) {
			return false
		}
	}
	return true
}

func (m Mat4) IsAffine() bool {
	return m[12] == 0 && m[13] == 0 && m[14] == 0 && m[15] == 1
}

func Mat4InverseAffine(m Mat4) (Mat4, bool) {
	if !m.IsFinite() || !m.IsAffine() {
		return Mat4{}, false
	}
	a, b, c := m[0], m[1], m[2]
	d, e, f := m[4], m[5], m[6]
	g, h, i := m[8], m[9], m[10]
	det := a*(e*i-f*h) - b*(d*i-f*g) + c*(d*h-e*g)
	if math.Abs(det) < 1e-15 || !finite(det) {
		return Mat4{}, false
	}
	invDet := 1 / det
	var out Mat4
	out[0] = (e*i - f*h) * invDet
	out[1] = (c*h - b*i) * invDet
	out[2] = (b*f - c*e) * invDet
	out[4] = (f*g - d*i) * invDet
	out[5] = (a*i - c*g) * invDet
	out[6] = (c*d - a*f) * invDet
	out[8] = (d*h - e*g) * invDet
	out[9] = (b*g - a*h) * invDet
	out[10] = (a*e - b*d) * invDet
	t := V3(m[3], m[7], m[11])
	ti := V3(
		-(out[0]*t.X + out[1]*t.Y + out[2]*t.Z),
		-(out[4]*t.X + out[5]*t.Y + out[6]*t.Z),
		-(out[8]*t.X + out[9]*t.Y + out[10]*t.Z),
	)
	out[3], out[7], out[11] = ti.X, ti.Y, ti.Z
	out[15] = 1
	return out, out.IsFinite()
}

func (m Mat4) MaxRowAbsSum() float64 {
	best := 0.0
	for i := 0; i < 4; i++ {
		s := 0.0
		for j := 0; j < 4; j++ {
			s += math.Abs(m[i*4+j])
		}
		if s > best {
			best = s
		}
	}
	return best
}
