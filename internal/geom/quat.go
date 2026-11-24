package geom

import "math"

// Quat is a unit quaternion in (w, x, y, z) order.
type Quat struct {
	W float64 `json:"w"`
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

func IdentityQuat() Quat {
	return Quat{W: 1}
}

func (q Quat) Normalize() Quat {
	l := math.Sqrt(q.W*q.W + q.X*q.X + q.Y*q.Y + q.Z*q.Z)
	if l < 1e-15 {
		return IdentityQuat()
	}
	if q.W < 0 {
		l = -l
	}
	return Quat{q.W / l, q.X / l, q.Y / l, q.Z / l}
}

func (q Quat) Mul(o Quat) Quat {
	return Quat{
		W: q.W*o.W - q.X*o.X - q.Y*o.Y - q.Z*o.Z,
		X: q.W*o.X + q.X*o.W + q.Y*o.Z - q.Z*o.Y,
		Y: q.W*o.Y - q.X*o.Z + q.Y*o.W + q.Z*o.X,
		Z: q.W*o.Z + q.X*o.Y - q.Y*o.X + q.Z*o.W,
	}
}

func (q Quat) Conjugate() Quat {
	return Quat{W: q.W, X: -q.X, Y: -q.Y, Z: -q.Z}
}

func (q Quat) Inverse() Quat {
	l := q.W*q.W + q.X*q.X + q.Y*q.Y + q.Z*q.Z
	if l < 1e-15 {
		return IdentityQuat()
	}
	return Quat{q.W / l, -q.X / l, -q.Y / l, -q.Z / l}
}

func (q Quat) Rotate(v Vec3) Vec3 {
	u := Vec3{q.X, q.Y, q.Z}
	t := u.Cross(v).Mul(2)
	return v.Add(t.Mul(q.W)).Add(u.Cross(t))
}

func (q Quat) Angle() float64 {
	w := math.Abs(q.W)
	if w > 1 {
		w = 1
	}
	return 2 * math.Acos(w)
}

func (q Quat) IsFinite() bool {
	return isFinite(q.W) && isFinite(q.X) && isFinite(q.Y) && isFinite(q.Z)
}

func QuatFromAxisAngle(axis Vec3, radians float64) Quat {
	axis = axis.Normalize()
	if axis.LengthSquared() < 0.5 {
		return IdentityQuat()
	}
	s := math.Sin(radians / 2)
	return Quat{W: math.Cos(radians / 2), X: axis.X * s, Y: axis.Y * s, Z: axis.Z * s}.Normalize()
}

func QuatFromTo(from, to Vec3) Quat {
	from = from.Normalize()
	to = to.Normalize()
	if from.LengthSquared() < 0.5 || to.LengthSquared() < 0.5 {
		return IdentityQuat()
	}
	d := from.Dot(to)
	if d >= 1-1e-12 {
		return IdentityQuat()
	}
	if d <= -1+1e-12 {
		axis := from.Cross(Vec3{0, 1, 0})
		if axis.LengthSquared() < 1e-12 {
			axis = from.Cross(Vec3{1, 0, 0})
		}
		return QuatFromAxisAngle(axis, math.Pi)
	}
	axis := from.Cross(to)
	q := Quat{W: 1 + d, X: axis.X, Y: axis.Y, Z: axis.Z}
	return q.Normalize()
}

func Slerp(a, b Quat, t float64) Quat {
	a = a.Normalize()
	b = b.Normalize()
	d := a.W*b.W + a.X*b.X + a.Y*b.Y + a.Z*b.Z
	if d < 0 {
		b = Quat{-b.W, -b.X, -b.Y, -b.Z}
		d = -d
	}
	if d > 0.9995 {
		return Quat{
			W: a.W + t*(b.W-a.W),
			X: a.X + t*(b.X-a.X),
			Y: a.Y + t*(b.Y-a.Y),
			Z: a.Z + t*(b.Z-a.Z),
		}.Normalize()
	}
	theta0 := math.Acos(math.Max(-1, math.Min(1, d)))
	theta := theta0 * t
	sin0 := math.Sin(theta0)
	s0 := math.Sin(theta0-theta) / sin0
	s1 := math.Sin(theta) / sin0
	return Quat{
		W: s0*a.W + s1*b.W,
		X: s0*a.X + s1*b.X,
		Y: s0*a.Y + s1*b.Y,
		Z: s0*a.Z + s1*b.Z,
	}.Normalize()
}

func ClampAngle(q Quat, maxRadians float64) (Quat, float64) {
	q = q.Normalize()
	if maxRadians <= 0 {
		return q, 0
	}
	angle := q.Angle()
	if angle <= maxRadians+1e-12 {
		return q, 0
	}
	if q.W < 0 {
		q = Quat{-q.W, -q.X, -q.Y, -q.Z}
	}
	axis := Vec3{q.X, q.Y, q.Z}
	if axis.LengthSquared() < 1e-15 {
		return IdentityQuat(), angle - maxRadians
	}
	return QuatFromAxisAngle(axis, maxRadians), angle - maxRadians
}
