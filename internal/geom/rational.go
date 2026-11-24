package geom

import (
	"math"

	"cadlab/internal/model"
)

type ratPoint struct {
	A model.Vec3
	W float64
}

type ratGrid struct {
	Rows, Cols int
	DegU, DegV int
	KU, KV     []float64
	Data       []ratPoint
}

func ratGridFromPatch(p model.SurfacePatch) ratGrid {
	g := ratGrid{
		Rows: p.VCount, Cols: p.UCount, DegU: p.UDegree, DegV: p.VDegree,
		KU: append([]float64(nil), p.UKnots...), KV: append([]float64(nil), p.VKnots...),
		Data: make([]ratPoint, p.UCount*p.VCount),
	}
	for i, cp := range p.Control {
		g.Data[i] = ratPoint{A: cp.Position.Mul(cp.Weight), W: cp.Weight}
	}
	return g
}

// deriveURat applies the exact B-spline derivative control-net formula. The
// derivative knot vector drops its first and last knot as required by the
// lower-degree representation.
func deriveURat(g ratGrid) (ratGrid, bool) {
	if g.DegU < 1 || g.Cols < 2 {
		return ratGrid{}, false
	}
	out := ratGrid{
		Rows: g.Rows, Cols: g.Cols - 1, DegU: g.DegU - 1, DegV: g.DegV,
		KU:   append([]float64(nil), g.KU[1:len(g.KU)-1]...),
		KV:   append([]float64(nil), g.KV...),
		Data: make([]ratPoint, g.Rows*(g.Cols-1)),
	}
	scale := float64(g.DegU)
	for row := 0; row < g.Rows; row++ {
		for col := 0; col < out.Cols; col++ {
			a, b := g.Data[row*g.Cols+col], g.Data[row*g.Cols+col+1]
			den := g.KU[col+g.DegU+1] - g.KU[col+1]
			out.Data[row*out.Cols+col] = ratPoint{
				A: a.A.Add(b.A.Sub(a.A).Mul(scale / den)),
				W: a.W + (b.W-a.W)*scale/den,
			}
		}
	}
	return out, true
}

func deriveVRat(g ratGrid) (ratGrid, bool) {
	if g.DegV < 1 || g.Rows < 2 {
		return ratGrid{}, false
	}
	out := ratGrid{
		Rows: g.Rows - 1, Cols: g.Cols, DegU: g.DegU, DegV: g.DegV - 1,
		KU:   append([]float64(nil), g.KU...),
		KV:   append([]float64(nil), g.KV[1:len(g.KV)-1]...),
		Data: make([]ratPoint, (g.Rows-1)*g.Cols),
	}
	scale := float64(g.DegV)
	for row := 0; row < out.Rows; row++ {
		for col := 0; col < g.Cols; col++ {
			a, b := g.Data[row*g.Cols+col], g.Data[(row+1)*g.Cols+col]
			den := g.KV[row+g.DegV+1] - g.KV[row+1]
			out.Data[row*out.Cols+col] = ratPoint{
				A: a.A.Add(b.A.Sub(a.A).Mul(scale / den)),
				W: a.W + (b.W-a.W)*scale/den,
			}
		}
	}
	return out, true
}

func evalOptionalRat(g ratGrid, ok bool, u, v float64) (model.Vec3, float64) {
	if !ok {
		return model.Vec3{}, 0
	}
	return evalRatGrid(g, u, v)
}

func evalRatGrid(g ratGrid, u, v float64) (model.Vec3, float64) {
	u = math.Max(g.KU[g.DegU], math.Min(g.KU[g.Cols], u))
	v = math.Max(g.KV[g.DegV], math.Min(g.KV[g.Rows], v))
	bu := basisRow(g.KU, g.DegU, g.Cols, u)
	bv := basisRow(g.KV, g.DegV, g.Rows, v)
	var a model.Vec3
	var w float64
	for row := 0; row < g.Rows; row++ {
		for col := 0; col < g.Cols; col++ {
			f := bu[col] * bv[row]
			p := g.Data[row*g.Cols+col]
			a = a.Add(p.A.Mul(f))
			w += p.W * f
		}
	}
	return a, w
}

func basisRow(knots []float64, degree, count int, u float64) []float64 {
	out := make([]float64, count+1)
	if degree < 0 {
		return out
	}
	span := findSpan(count-1, degree, u, knots)
	vals := basisFuns(span, degree, u, knots)
	for j := 0; j <= degree; j++ {
		idx := span - degree + j
		if idx >= 0 && idx < count {
			out[idx] = vals[j]
		}
	}
	return out
}

func basisFuns(span, degree int, u float64, knots []float64) []float64 {
	n := make([]float64, degree+1)
	left := make([]float64, degree+1)
	right := make([]float64, degree+1)
	n[0] = 1
	for j := 1; j <= degree; j++ {
		left[j] = u - knots[span+1-j]
		right[j] = knots[span+j] - u
		saved := 0.0
		for r := 0; r < j; r++ {
			den := right[r+1] + left[j-r]
			if math.Abs(den) < 1e-15 {
				n[r] = saved
				saved = 0
				continue
			}
			temp := n[r] / den
			n[r] = saved + right[r+1]*temp
			saved = left[j-r] * temp
		}
		n[j] = saved
	}
	return n
}
