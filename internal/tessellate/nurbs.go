package tessellate

import (
	"math"

	"cadpipe/internal/cad"
)

type basisTable struct {
	knots  []float64
	degree int
	u      float64
	memo   map[[2]int]float64
}

func newBasis(knots []float64, degree int, u float64) *basisTable {
	return &basisTable{knots: knots, degree: degree, u: u, memo: make(map[[2]int]float64)}
}

func (b *basisTable) N(i, p int) float64 {
	key := [2]int{i, p}
	if v, ok := b.memo[key]; ok {
		return v
	}
	var v float64
	switch {
	case p == 0:
		last := len(b.knots) - 1
		if i == last-1 && b.u == b.knots[last] {
			v = 1
		} else if i >= 0 && i+1 < len(b.knots) && b.knots[i] <= b.u && b.u < b.knots[i+1] {
			v = 1
		}
	default:
		leftDen := b.knots[i+p] - b.knots[i]
		if leftDen != 0 {
			v += (b.u - b.knots[i]) / leftDen * b.N(i, p-1)
		}
		rightDen := b.knots[i+p+1] - b.knots[i+1]
		if rightDen != 0 && i+1 < len(b.knots)-p-1 {
			v += (b.knots[i+p+1] - b.u) / rightDen * b.N(i+1, p-1)
		}
	}
	b.memo[key] = v
	return v
}

func basisValues(controlCount, degree int, knots []float64, u float64) (int, []float64) {
	n := controlCount - 1
	span := degree
	switch {
	case u >= knots[n+1]:
		span = n
	case u <= knots[degree]:
		span = degree
	default:
		low := degree
		high := n + 1
		for high-low > 1 {
			mid := (low + high) / 2
			if u < knots[mid] {
				high = mid
			} else {
				low = mid
			}
		}
		span = low
	}

	N := make([]float64, degree+1)
	left := make([]float64, degree+1)
	right := make([]float64, degree+1)
	N[0] = 1
	for j := 1; j <= degree; j++ {
		left[j] = u - knots[span+1-j]
		right[j] = knots[span+j] - u
		saved := 0.0
		for r := 0; r < j; r++ {
			den := right[r+1] + left[j-r]
			temp := 0.0
			if den != 0 {
				temp = N[r] / den
			}
			N[r] = saved + right[r+1]*temp
			saved = left[j-r] * temp
		}
		N[j] = saved
	}
	return span - degree, N
}

func Evaluate(p *cad.Patch, u, v float64) cad.Vec3 {
	du := p.DegreeU
	dv := p.DegreeV
	ui, nu := basisValues(len(p.ControlPoints), du, p.KnotsU, u)
	vj, nv := basisValues(len(p.ControlPoints[0]), dv, p.KnotsV, v)
	var sumW, x, y, z float64
	for ii := 0; ii <= du; ii++ {
		i := ui + ii
		if i < 0 || i >= len(p.ControlPoints) || ii >= len(nu) || nu[ii] == 0 {
			continue
		}
		for jj := 0; jj <= dv; jj++ {
			j := vj + jj
			if j < 0 || j >= len(p.ControlPoints[i]) || jj >= len(nv) || nv[jj] == 0 {
				continue
			}
			w := 1.0
			if i < len(p.Weights) && j < len(p.Weights[i]) && p.Weights[i][j] != 0 {
				w = p.Weights[i][j]
			}
			f := w * nu[ii] * nv[jj]
			q := p.ControlPoints[i][j].Metres()
			x += q.X * f
			y += q.Y * f
			z += q.Z * f
			sumW += f
		}
	}
	if sumW != 0 {
		x /= sumW
		y /= sumW
		z /= sumW
	}
	return cad.Vec3{X: x, Y: y, Z: z}
}

func partialDerivatives(p *cad.Patch, u, v float64) (du, dv, duu, dvv, duv cad.Vec3) {
	const h = 1e-5
	eval := Evaluate

	u0, u1 := u-h, u+h
	hu := 2 * h
	if u0 < 0 {
		u0, u1 = 0, h
		hu = h
	} else if u1 > 1 {
		u0, u1 = 1-h, 1
		hu = h
	}
	du = scale(sub(eval(p, u1, v), eval(p, u0, v)), 1/hu)

	v0, v1 := v-h, v+h
	hv := 2 * h
	if v0 < 0 {
		v0, v1 = 0, h
		hv = h
	} else if v1 > 1 {
		v0, v1 = 1-h, 1
		hv = h
	}
	dv = scale(sub(eval(p, u, v1), eval(p, u, v0)), 1/hv)

	const h2 = 1e-3
	switch {
	case u-h2 < 0:
		duu = scale(sub(add(eval(p, u+2*h2, v), eval(p, u, v)), scale(eval(p, u+h2, v), 2)), 1/(h2*h2))
	case u+h2 > 1:
		duu = scale(sub(add(eval(p, u, v), eval(p, u-2*h2, v)), scale(eval(p, u-h2, v), 2)), 1/(h2*h2))
	default:
		duu = scale(sub(add(eval(p, u+h2, v), eval(p, u-h2, v)), scale(eval(p, u, v), 2)), 1/(h2*h2))
	}
	switch {
	case v-h2 < 0:
		dvv = scale(sub(add(eval(p, u, v+2*h2), eval(p, u, v)), scale(eval(p, u, v+h2), 2)), 1/(h2*h2))
	case v+h2 > 1:
		dvv = scale(sub(add(eval(p, u, v), eval(p, u, v-2*h2)), scale(eval(p, u, v-h2), 2)), 1/(h2*h2))
	default:
		dvv = scale(sub(add(eval(p, u, v+h2), eval(p, u, v-h2)), scale(eval(p, u, v), 2)), 1/(h2*h2))
	}

	su, sv := 1.0, 1.0
	if u+h2 > 1 {
		su = -1
	}
	if v+h2 > 1 {
		sv = -1
	}
	duv = sub(
		sub(eval(p, u+su*h2, v+sv*h2), eval(p, u+su*h2, v)),
		sub(eval(p, u, v+sv*h2), eval(p, u, v)),
	)
	duv = scale(duv, 1/(su*sv*h2*h2))
	return
}

func NormalAt(p *cad.Patch, u, v float64) cad.Vec3 {
	du, dv, _, _, _ := partialDerivatives(p, u, v)
	n := normalize(cross(du, dv))
	if math.IsNaN(n.X) || math.IsNaN(n.Y) || math.IsNaN(n.Z) || length(n) < 1e-12 {
		return cad.Vec3{X: 0, Y: 0, Z: 1}
	}
	return n
}

func CurvatureAt(p *cad.Patch, u, v float64) float64 {
	du, dv, duu, dvv, duv := partialDerivatives(p, u, v)
	n := normalize(cross(du, dv))
	if length(n) < 1e-12 {
		n = cad.Vec3{Z: 1}
	}
	return math.Abs(dot(duu, n)) + math.Abs(dot(dvv, n)) + 2*math.Abs(dot(duv, n))
}

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

func sub(a, b cad.Vec3) cad.Vec3 { return cad.Vec3{X: a.X - b.X, Y: a.Y - b.Y, Z: a.Z - b.Z} }
func add(a, b cad.Vec3) cad.Vec3 { return cad.Vec3{X: a.X + b.X, Y: a.Y + b.Y, Z: a.Z + b.Z} }
func scale(a cad.Vec3, s float64) cad.Vec3 {
	return cad.Vec3{X: a.X * s, Y: a.Y * s, Z: a.Z * s}
}
func cross(a, b cad.Vec3) cad.Vec3 {
	return cad.Vec3{X: a.Y*b.Z - a.Z*b.Y, Y: a.Z*b.X - a.X*b.Z, Z: a.X*b.Y - a.Y*b.X}
}
func dot(a, b cad.Vec3) float64 { return a.X*b.X + a.Y*b.Y + a.Z*b.Z }
func length(a cad.Vec3) float64 { return math.Sqrt(dot(a, a)) }
func normalize(a cad.Vec3) cad.Vec3 {
	l := length(a)
	if l == 0 {
		return cad.Vec3{}
	}
	return scale(a, 1/l)
}
