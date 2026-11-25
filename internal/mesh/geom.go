package mesh

import "math"

func Add(a, b Vec3) Vec3           { return Vec3{a[0] + b[0], a[1] + b[1], a[2] + b[2]} }
func Sub(a, b Vec3) Vec3           { return Vec3{a[0] - b[0], a[1] - b[1], a[2] - b[2]} }
func Scale(a Vec3, s float64) Vec3 { return Vec3{a[0] * s, a[1] * s, a[2] * s} }
func Dot(a, b Vec3) float64        { return a[0]*b[0] + a[1]*b[1] + a[2]*b[2] }

func Cross(a, b Vec3) Vec3 {
	return Vec3{
		a[1]*b[2] - a[2]*b[1],
		a[2]*b[0] - a[0]*b[2],
		a[0]*b[1] - a[1]*b[0],
	}
}

func Norm(a Vec3) float64 { return math.Sqrt(Dot(a, a)) }

func Distance(a, b Vec3) float64 { return Norm(Sub(a, b)) }

func Normalized(a Vec3) Vec3 {
	n := Norm(a)
	if n == 0 {
		return Vec3{}
	}
	return Scale(a, 1/n)
}

func FaceNormal(m *Mesh, f Face) Vec3 {
	return Normalized(Cross(Sub(m.Vertices[f.B], m.Vertices[f.A]), Sub(m.Vertices[f.C], m.Vertices[f.A])))
}

// PointTriangleDistance returns the closest Euclidean distance from p to a
// triangle. It uses Ericson's closest-point-on-triangle region tests.
func PointTriangleDistance(p, a, b, c Vec3) float64 {
	ab := Sub(b, a)
	ac := Sub(c, a)
	ap := Sub(p, a)
	d1 := Dot(ab, ap)
	d2 := Dot(ac, ap)
	if d1 <= 0 && d2 <= 0 {
		return Distance(p, a)
	}
	bp := Sub(p, b)
	d3 := Dot(ab, bp)
	d4 := Dot(ac, bp)
	if d3 >= 0 && d4 <= d3 {
		return Distance(p, b)
	}
	vc := d1*d4 - d3*d2
	if vc <= 0 && d1 >= 0 && d3 <= 0 {
		v := d1 / (d1 - d3)
		return Distance(p, Add(a, Scale(ab, v)))
	}
	cp := Sub(p, c)
	d5 := Dot(ab, cp)
	d6 := Dot(ac, cp)
	if d6 >= 0 && d5 <= d6 {
		return Distance(p, c)
	}
	vb := d5*d2 - d1*d6
	if vb <= 0 && d2 >= 0 && d6 <= 0 {
		w := d2 / (d2 - d6)
		return Distance(p, Add(a, Scale(ac, w)))
	}
	va := d3*d6 - d5*d4
	if va <= 0 && (d4-d3) >= 0 && (d5-d6) >= 0 {
		w := (d4 - d3) / ((d4 - d3) + (d5 - d6))
		return Distance(p, Add(b, Scale(Sub(c, b), w)))
	}
	denom := 1 / (va + vb + vc)
	v := vb * denom
	w := vc * denom
	closest := Add(a, Add(Scale(ab, v), Scale(ac, w)))
	return Distance(p, closest)
}

func PointMeshDistance(p Vec3, m *Mesh) (float64, int) {
	best := math.Inf(1)
	bestFace := -1
	for i, f := range m.Faces {
		d := PointTriangleDistance(p, m.Vertices[f.A], m.Vertices[f.B], m.Vertices[f.C])
		if d < best {
			best = d
			bestFace = i
		}
	}
	if math.IsInf(best, 1) {
		return math.Inf(1), -1
	}
	return best, bestFace
}
