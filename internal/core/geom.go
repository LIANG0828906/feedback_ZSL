package core

import (
	"math"
)

func (v Vec3) Add(o Vec3) Vec3      { return Vec3{v[0] + o[0], v[1] + o[1], v[2] + o[2]} }
func (v Vec3) Sub(o Vec3) Vec3      { return Vec3{v[0] - o[0], v[1] - o[1], v[2] - o[2]} }
func (v Vec3) Scale(s float64) Vec3 { return Vec3{v[0] * s, v[1] * s, v[2] * s} }
func (v Vec3) Dot(o Vec3) float64   { return v[0]*o[0] + v[1]*o[1] + v[2]*o[2] }
func (v Vec3) Cross(o Vec3) Vec3 {
	return Vec3{
		v[1]*o[2] - v[2]*o[1],
		v[2]*o[0] - v[0]*o[2],
		v[0]*o[1] - v[1]*o[0],
	}
}
func (v Vec3) Norm() float64 { return math.Sqrt(v.Dot(v)) }
func (v Vec3) Finite() bool {
	return FiniteFloat(v[0]) && FiniteFloat(v[1]) && FiniteFloat(v[2])
}

func (v Vec3) Normalize() Vec3 {
	n := v.Norm()
	if n == 0 || !FiniteFloat(n) {
		return Vec3{}
	}
	return v.Scale(1 / n)
}

func (q Quat) W() float64 { return q[0] }
func (q Quat) X() float64 { return q[1] }
func (q Quat) Y() float64 { return q[2] }
func (q Quat) Z() float64 { return q[3] }

func (q Quat) Finite() bool {
	for _, x := range q {
		if !FiniteFloat(x) {
			return false
		}
	}
	return true
}

func (q Quat) Norm() float64 {
	return math.Sqrt(q[0]*q[0] + q[1]*q[1] + q[2]*q[2] + q[3]*q[3])
}

func (q Quat) Normalized() Quat {
	n := q.Norm()
	if n == 0 || !FiniteFloat(n) {
		return Quat{1, 0, 0, 0}
	}
	return Quat{q[0] / n, q[1] / n, q[2] / n, q[3] / n}
}

func (q Quat) Conjugate() Quat { return Quat{q[0], -q[1], -q[2], -q[3]} }

func (q Quat) Mul(r Quat) Quat {
	return Quat{
		q[0]*r[0] - q[1]*r[1] - q[2]*r[2] - q[3]*r[3],
		q[0]*r[1] + q[1]*r[0] + q[2]*r[3] - q[3]*r[2],
		q[0]*r[2] - q[1]*r[3] + q[2]*r[0] + q[3]*r[1],
		q[0]*r[3] + q[1]*r[2] - q[2]*r[1] + q[3]*r[0],
	}
}

func (q Quat) Rotate(v Vec3) Vec3 {
	qn := q.Normalized()
	qx := Vec3{qn[1], qn[2], qn[3]}
	t := qx.Cross(v).Scale(2)
	return v.Add(t.Scale(qn[0])).Add(qx.Cross(t))
}

// Angle returns the smallest rotation angle in [0,pi].
func (q Quat) Angle() float64 {
	qn := q.Normalized()
	w := math.Abs(qn[0])
	if w > 1 {
		w = 1
	}
	return 2 * math.Acos(w)
}

func AxisAngle(axis Vec3, angle float64) Quat {
	a := axis.Normalize()
	if a.Norm() == 0 {
		return Quat{1, 0, 0, 0}
	}
	s := math.Sin(angle / 2)
	return Quat{math.Cos(angle / 2), a[0] * s, a[1] * s, a[2] * s}
}

// ProjectAngle clamps a quaternion's rotation magnitude while preserving axis.
func ProjectAngle(q Quat, maxAngle float64) Quat {
	qn := q.Normalized()
	angle := 2 * math.Acos(clamp(qn[0], -1, 1))
	if qn[0] < 0 {
		// Use the shorter arc.
		qn = Quat{-qn[0], -qn[1], -qn[2], -qn[3]}
		angle = 2 * math.Acos(clamp(qn[0], -1, 1))
	}
	if angle <= maxAngle+1e-15 || angle == 0 {
		return qn
	}
	axis := Vec3{qn[1], qn[2], qn[3]}.Scale(1 / math.Sin(angle/2))
	return AxisAngle(axis, maxAngle)
}

func Slerp(a, b Quat, t float64) Quat {
	an := a.Normalized()
	bn := b.Normalized()
	d := clamp(an[0]*bn[0]+an[1]*bn[1]+an[2]*bn[2]+an[3]*bn[3], -1, 1)
	if d < 0 {
		bn = Quat{-bn[0], -bn[1], -bn[2], -bn[3]}
		d = -d
	}
	if d > 0.9995 {
		out := Quat{
			an[0] + t*(bn[0]-an[0]),
			an[1] + t*(bn[1]-an[1]),
			an[2] + t*(bn[2]-an[2]),
			an[3] + t*(bn[3]-an[3]),
		}
		return out.Normalized()
	}
	theta0 := math.Acos(d)
	theta := theta0 * t
	sinTheta := math.Sin(theta)
	sinTheta0 := math.Sin(theta0)
	s0 := math.Cos(theta) - d*sinTheta/sinTheta0
	s1 := sinTheta / sinTheta0
	return Quat{
		s0*an[0] + s1*bn[0],
		s0*an[1] + s1*bn[1],
		s0*an[2] + s1*bn[2],
		s0*an[3] + s1*bn[3],
	}.Normalized()
}

func QuantizeQuat(q Quat, step float64) (Quat, float64) {
	if step <= 0 {
		step = 1.0 / 1024.0
	}
	var out Quat
	residual := 0.0
	for i := 0; i < 4; i++ {
		rounded := math.Round(q[i]/step) * step
		out[i] = rounded
		if d := math.Abs(q[i] - rounded); d > residual {
			residual = d
		}
	}
	out = out.Normalized()
	normResidual := 0.0
	for i := 0; i < 4; i++ {
		if d := math.Abs(q.Normalized()[i] - out[i]); d > normResidual {
			normResidual = d
		}
	}
	if normResidual > residual {
		residual = normResidual
	}
	return out, residual
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

func maxFloat(values ...float64) float64 {
	out := values[0]
	for _, v := range values[1:] {
		if v > out {
			out = v
		}
	}
	return out
}

func quatDistance(a, b Quat) float64 {
	d := math.Abs(a.Normalized().Mul(b.Normalized().Conjugate()).Angle())
	return d
}

func QuatFromVectors(from, to Vec3) Quat {
	a := from.Normalize()
	b := to.Normalize()
	if a.Norm() == 0 || b.Norm() == 0 {
		return Quat{1, 0, 0, 0}
	}
	d := clamp(a.Dot(b), -1, 1)
	if d < -0.999999 {
		axis := Vec3{1, 0, 0}.Cross(a)
		if axis.Norm() < 1e-6 {
			axis = Vec3{0, 0, 1}.Cross(a)
		}
		return AxisAngle(axis.Normalize(), math.Pi)
	}
	axis := a.Cross(b)
	q := Quat{1 + d, axis[0], axis[1], axis[2]}
	return q.Normalized()
}

func SignedAngleAround(q Quat, axis Vec3) float64 {
	qn := q.Normalized()
	a := axis.Normalize()
	if a.Norm() == 0 {
		return 0
	}
	return 2 * math.Atan2(a[0]*qn[1]+a[1]*qn[2]+a[2]*qn[3], qn[0])
}
