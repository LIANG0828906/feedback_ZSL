package samplegen

import "math"

type fVec3 struct {
	X, Y, Z float64
}

func fv(x, y, z float64) fVec3 { return fVec3{x, y, z} }

func (a fVec3) add(b fVec3) fVec3   { return fVec3{a.X + b.X, a.Y + b.Y, a.Z + b.Z} }
func (a fVec3) sub(b fVec3) fVec3   { return fVec3{a.X - b.X, a.Y - b.Y, a.Z - b.Z} }
func (a fVec3) mul(s float64) fVec3 { return fVec3{a.X * s, a.Y * s, a.Z * s} }
func (a fVec3) dot(b fVec3) float64 { return a.X*b.X + a.Y*b.Y + a.Z*b.Z }
func (a fVec3) cross(b fVec3) fVec3 {
	return fVec3{
		a.Y*b.Z - a.Z*b.Y,
		a.Z*b.X - a.X*b.Z,
		a.X*b.Y - a.Y*b.X,
	}
}

type fMat4 [16]float64

func fIdentity() fMat4 {
	return fMat4{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}
}

func fTranslation(v fVec3) fMat4 {
	m := fIdentity()
	m[3], m[7], m[11] = v.X, v.Y, v.Z
	return m
}

func fScale(v fVec3) fMat4 {
	return fMat4{v.X, 0, 0, 0, 0, v.Y, 0, 0, 0, 0, v.Z, 0, 0, 0, 0, 1}
}

func fRotationZ(angle float64) fMat4 {
	c, s := math.Cos(angle), math.Sin(angle)
	return fMat4{c, -s, 0, 0, s, c, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}
}

func fMul(a, b fMat4) fMat4 {
	var out fMat4
	for r := 0; r < 4; r++ {
		for c := 0; c < 4; c++ {
			var v float64
			for k := 0; k < 4; k++ {
				v += a[r*4+k] * b[k*4+c]
			}
			out[r*4+c] = v
		}
	}
	return out
}

func fTransform(m fMat4, p fVec3) fVec3 {
	return fVec3{
		X: m[0]*p.X + m[1]*p.Y + m[2]*p.Z + m[3],
		Y: m[4]*p.X + m[5]*p.Y + m[6]*p.Z + m[7],
		Z: m[8]*p.X + m[9]*p.Y + m[10]*p.Z + m[11],
	}
}

func fTransformDir(m fMat4, p fVec3) fVec3 {
	return fVec3{
		X: m[0]*p.X + m[1]*p.Y + m[2]*p.Z,
		Y: m[4]*p.X + m[5]*p.Y + m[6]*p.Z,
		Z: m[8]*p.X + m[9]*p.Y + m[10]*p.Z,
	}
}

func fPivot(pivot fVec3, kind string, vector fVec3, angle float64) fMat4 {
	switch kind {
	case "translate":
		return fTranslation(vector)
	case "rotate":
		return fMul(fTranslation(pivot), fMul(fRotationZ(angle), fTranslation(pivot.mul(-1))))
	case "scale":
		return fMul(fTranslation(pivot), fMul(fScale(vector), fTranslation(pivot.mul(-1))))
	default:
		return fIdentity()
	}
}

type fRayHit struct {
	hit      bool
	t        float64
	point    fVec3
	distance float64
}

func fRayAABB(o, d, mn, mx fVec3) fRayHit {
	tmin := -math.MaxFloat64
	tmax := math.MaxFloat64
	for axis := 0; axis < 3; axis++ {
		var ov, dv, lo, hi float64
		switch axis {
		case 0:
			ov, dv, lo, hi = o.X, d.X, mn.X, mx.X
		case 1:
			ov, dv, lo, hi = o.Y, d.Y, mn.Y, mx.Y
		default:
			ov, dv, lo, hi = o.Z, d.Z, mn.Z, mx.Z
		}
		if math.Abs(dv) < 1e-15 {
			if ov < lo || ov > hi {
				return fRayHit{}
			}
			continue
		}
		t1 := (lo - ov) / dv
		t2 := (hi - ov) / dv
		if t1 > t2 {
			t1, t2 = t2, t1
		}
		tmin = math.Max(tmin, t1)
		tmax = math.Min(tmax, t2)
		if tmin > tmax {
			return fRayHit{}
		}
	}
	t := tmin
	if t < 0 {
		t = 0
	}
	p := o.add(d.mul(t))
	distance := math.Sqrt(p.X*p.X + p.Y*p.Y + p.Z*p.Z)
	return fRayHit{hit: true, t: t, point: p, distance: distance}
}

func fRaySphere(o, d, c fVec3, r float64) fRayHit {
	oc := o.sub(c)
	a := d.dot(d)
	b := oc.dot(d)
	cc := oc.dot(oc) - r*r
	disc := b*b - a*cc
	if disc < 0 {
		return fRayHit{}
	}
	sq := math.Sqrt(disc)
	t := (-b - sq) / a
	if t < 0 {
		t = (-b + sq) / a
	}
	if t < 0 {
		return fRayHit{}
	}
	p := o.add(d.mul(t))
	distance := math.Sqrt(p.X*p.X + p.Y*p.Y + p.Z*p.Z)
	return fRayHit{hit: true, t: t, point: p, distance: distance}
}

func fRayTriangle(o, d, a, b, c fVec3) fRayHit {
	e1 := b.sub(a)
	e2 := c.sub(a)
	p := d.cross(e2)
	det := e1.dot(p)
	if math.Abs(det) < 1e-15 {
		return fRayHit{}
	}
	inv := 1 / det
	tv := o.sub(a)
	u := tv.dot(p) * inv
	if u < 0 || u > 1 {
		return fRayHit{}
	}
	q := tv.cross(e1)
	v := d.dot(q) * inv
	if v < 0 || u+v > 1 {
		return fRayHit{}
	}
	t := e2.dot(q) * inv
	if t < 0 {
		return fRayHit{}
	}
	point := o.add(d.mul(t))
	distance := math.Sqrt(point.X*point.X + point.Y*point.Y + point.Z*point.Z)
	return fRayHit{hit: true, t: t, point: point, distance: distance}
}

func fAABBVertices(min, max fVec3) []fVec3 {
	return []fVec3{
		{min.X, min.Y, min.Z}, {max.X, min.Y, min.Z},
		{min.X, max.Y, min.Z}, {max.X, max.Y, min.Z},
		{min.X, min.Y, max.Z}, {max.X, min.Y, max.Z},
		{min.X, max.Y, max.Z}, {max.X, max.Y, max.Z},
	}
}

func fTransformAABB(m fMat4, min, max fVec3) (fVec3, fVec3) {
	verts := fAABBVertices(min, max)
	var outMin, outMax fVec3
	for i, v := range verts {
		p := fTransform(m, v)
		if i == 0 {
			outMin, outMax = p, p
		} else {
			outMin = fVec3{math.Min(outMin.X, p.X), math.Min(outMin.Y, p.Y), math.Min(outMin.Z, p.Z)}
			outMax = fVec3{math.Max(outMax.X, p.X), math.Max(outMax.Y, p.Y), math.Max(outMax.Z, p.Z)}
		}
	}
	return outMin, outMax
}
