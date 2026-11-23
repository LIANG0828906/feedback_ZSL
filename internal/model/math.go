package model

import (
	"math"
)

const quatEpsilon = 1e-12

// Vec3 is a right-handed, Y-up vector in metres.
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

func (v Vec3) Scale(s float64) Vec3 {
	return Vec3{v.X * s, v.Y * s, v.Z * s}
}

func (v Vec3) Dot(o Vec3) float64 {
	return v.X*o.X + v.Y*o.Y + v.Z*o.Z
}

func (v Vec3) Length() float64 {
	return math.Sqrt(v.Dot(v))
}

func (v Vec3) Normalize() Vec3 {
	n := v.Length()
	if n < quatEpsilon {
		return Vec3{}
	}
	return v.Scale(1 / n)
}

func (v Vec3) Lerp(o Vec3, t float64) Vec3 {
	return Vec3{
		X: v.X + (o.X-v.X)*t,
		Y: v.Y + (o.Y-v.Y)*t,
		Z: v.Z + (o.Z-v.Z)*t,
	}
}

func (v Vec3) IsFinite() bool {
	return isFinite(v.X) && isFinite(v.Y) && isFinite(v.Z)
}

// Quat is a unit quaternion in XYZW order.
type Quat struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
	W float64 `json:"w"`
}

var QuatIdentity = Quat{W: 1}

func (q Quat) IsFinite() bool {
	return isFinite(q.X) && isFinite(q.Y) && isFinite(q.Z) && isFinite(q.W)
}

func (q Quat) Norm() float64 {
	return math.Sqrt(q.X*q.X + q.Y*q.Y + q.Z*q.Z + q.W*q.W)
}

func (q Quat) Normalized() Quat {
	n := q.Norm()
	if n < quatEpsilon {
		return QuatIdentity
	}
	return Quat{q.X / n, q.Y / n, q.Z / n, q.W / n}
}

func (q Quat) Conjugate() Quat {
	return Quat{-q.X, -q.Y, -q.Z, q.W}
}

func (q Quat) Mul(o Quat) Quat {
	return Quat{
		X: q.W*o.X + q.X*o.W + q.Y*o.Z - q.Z*o.Y,
		Y: q.W*o.Y - q.X*o.Z + q.Y*o.W + q.Z*o.X,
		Z: q.W*o.Z + q.X*o.Y - q.Y*o.X + q.Z*o.W,
		W: q.W*o.W - q.X*o.X - q.Y*o.Y - q.Z*o.Z,
	}
}

func (q Quat) Dot(o Quat) float64 {
	return q.X*o.X + q.Y*o.Y + q.Z*o.Z + q.W*o.W
}

func (q Quat) Sub(o Quat) Quat {
	return Quat{q.X - o.X, q.Y - o.Y, q.Z - o.Z, q.W - o.W}
}

func (q Quat) Rotate(v Vec3) Vec3 {
	q = q.Normalized()
	u := Vec3{q.X, q.Y, q.Z}
	cross := u.Scale(1).Cross(v)
	return v.Add(cross.Scale(2 * q.W)).Add(u.Cross(cross).Scale(2))
}

func (v Vec3) Cross(o Vec3) Vec3 {
	return Vec3{
		X: v.Y*o.Z - v.Z*o.Y,
		Y: v.Z*o.X - v.X*o.Z,
		Z: v.X*o.Y - v.Y*o.X,
	}
}

func (q Quat) AngleRad() float64 {
	w := math.Abs(q.Normalized().W)
	if w > 1 {
		w = 1
	}
	return 2 * math.Acos(w)
}

func (q Quat) AngleDeg() float64 {
	return q.AngleRad() * 180 / math.Pi
}

func (q Quat) Slerp(o Quat, t float64) Quat {
	a := q.Normalized()
	b := o.Normalized()
	d := a.Dot(b)
	if d < 0 {
		b = Quat{-b.X, -b.Y, -b.Z, -b.W}
		d = -d
	}
	if d > 0.9995 {
		return Quat{
			X: a.X + (b.X-a.X)*t,
			Y: a.Y + (b.Y-a.Y)*t,
			Z: a.Z + (b.Z-a.Z)*t,
			W: a.W + (b.W-a.W)*t,
		}.Normalized()
	}
	if d > 1 {
		d = 1
	}
	theta := math.Acos(d)
	sinTheta := math.Sin(theta)
	if math.Abs(sinTheta) < quatEpsilon {
		return a
	}
	wa := math.Sin((1-t)*theta) / sinTheta
	wb := math.Sin(t*theta) / sinTheta
	return Quat{
		X: a.X*wa + b.X*wb,
		Y: a.Y*wa + b.Y*wb,
		Z: a.Z*wa + b.Z*wb,
		W: a.W*wa + b.W*wb,
	}.Normalized()
}

func QuatFromAxisAngle(axis Vec3, radians float64) Quat {
	axis = axis.Normalize()
	if axis.Length() < 0.5 {
		return QuatIdentity
	}
	s := math.Sin(radians / 2)
	return Quat{
		X: axis.X * s,
		Y: axis.Y * s,
		Z: axis.Z * s,
		W: math.Cos(radians / 2),
	}.Normalized()
}

func QuatFromEulerXYZ(x, y, z float64) Quat {
	qx := QuatFromAxisAngle(Vec3{X: 1}, x)
	qy := QuatFromAxisAngle(Vec3{Y: 1}, y)
	qz := QuatFromAxisAngle(Vec3{Z: 1}, z)
	return qx.Mul(qy).Mul(qz).Normalized()
}

// QuatFromTo returns the shortest rotation which maps unit vector a to b.
func QuatFromTo(a, b Vec3) Quat {
	a = a.Normalize()
	b = b.Normalize()
	if a.Length() < 0.5 || b.Length() < 0.5 {
		return QuatIdentity
	}
	d := clamp(a.Dot(b), -1, 1)
	if d > 1-1e-10 {
		return QuatIdentity
	}
	if d < -1+1e-10 {
		axis := a.Cross(Vec3{X: 1})
		if axis.Length() < 1e-6 {
			axis = a.Cross(Vec3{Y: 1})
		}
		return QuatFromAxisAngle(axis, math.Pi)
	}
	c := a.Cross(b)
	return Quat{X: c.X, Y: c.Y, Z: c.Z, W: 1 + d}.Normalized()
}

// WeightedQuatAverage is deterministic: joints and weights are consumed in
// ascending index order, independent of map iteration order.
func WeightedQuatAverage(quats []Quat, weights []float64) Quat {
	if len(quats) == 0 {
		return QuatIdentity
	}
	ref := quats[0].Normalized()
	acc := ref
	total := 1.0
	for i := 1; i < len(quats); i++ {
		w := 1.0
		if i < len(weights) && weights[i] > 0 {
			w = weights[i]
		}
		q := quats[i].Normalized()
		if acc.Dot(q) < 0 {
			q = Quat{-q.X, -q.Y, -q.Z, -q.W}
		}
		next := total + w
		acc = acc.Slerp(q, w/next)
		total = next
	}
	return acc.Normalized()
}

func ClampQuatAngle(q Quat, maxRad float64) (Quat, bool) {
	q = q.Normalized()
	angle := q.AngleRad()
	if angle <= maxRad+1e-12 {
		return q, false
	}
	axis := Vec3{q.X, q.Y, q.Z}
	if axis.Length() < 1e-10 {
		return QuatIdentity, true
	}
	return QuatFromAxisAngle(axis, maxRad), true
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

func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func FloatEqual(a, b, tolerance float64) bool {
	return math.Abs(a-b) <= tolerance
}

func QuatEqual(a, b Quat, tolerance float64) bool {
	if a.Dot(b) < 0 {
		b = Quat{-b.X, -b.Y, -b.Z, -b.W}
	}
	return FloatEqual(a.X, b.X, tolerance) &&
		FloatEqual(a.Y, b.Y, tolerance) &&
		FloatEqual(a.Z, b.Z, tolerance) &&
		FloatEqual(a.W, b.W, tolerance)
}
