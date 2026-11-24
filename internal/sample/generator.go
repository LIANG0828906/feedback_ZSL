package sample

import (
	"fmt"
	"math"
	"math/rand"

	"cadlab/internal/model"
)

const DefaultSeed int64 = 20261124

// Generate builds a fully local, deterministic CAD fixture. The truth evaluator
// below uses direct rational B-spline basis evaluation and is intentionally
// separate from the de Boor/tessellation path in internal/geom.
func Generate(seed int64) model.Dataset {
	if seed == 0 {
		seed = DefaultSeed
	}
	rng := rand.New(rand.NewSource(seed))
	ds := model.Dataset{
		Seed:      seed,
		Tolerance: 2e-5,
		AngleTol:  0.75,
		Generator: "cadlab.deterministic.nurbs.v1",
		Generated: "2026-01-01T00:00:00Z",
	}

	for i := 0; i < 3; i++ {
		patch := makePatch(i+1, rng)
		ds.Patches = append(ds.Patches, patch)
		appendTruth(&ds.Truth, patch)
	}
	ds.Meshes = []model.SubdivMesh{
		makeInspectionPlate(),
		makeDatumMarker(7.0, -0.15, 0.0),
	}
	return ds
}

func makePatch(id int, rng *rand.Rand) model.SurfacePatch {
	const degree = 3
	const count = 4
	p := model.SurfacePatch{
		ID:        id,
		Name:      fmt.Sprintf("NURBS-%02d", id),
		UDegree:   degree,
		VDegree:   degree,
		UCount:    count,
		VCount:    count,
		UKnots:    []float64{0, 0, 0, 0, 1, 1, 1, 1},
		VKnots:    []float64{0, 0, 0, 0, 1, 1, 1, 1},
		Tolerance: 1e-9,
	}

	xOffset := float64(id-2) * 5.2
	phase := float64(id) * 0.73
	for row := 0; row < count; row++ {
		for col := 0; col < count; col++ {
			u := float64(col) / float64(count-1)
			v := float64(row) / float64(count-1)
			jitter := (rng.Float64() - 0.5) * 0.44
			wave := math.Sin((u*2.2+phase)*math.Pi) * math.Cos((v*1.7-phase*0.35)*math.Pi)
			ridge := math.Exp(-math.Pow((u-0.62)*3.2, 2)-math.Pow((v-0.38)*2.4, 2)) * 1.15
			height := 0.55*wave + ridge + jitter
			if id == 2 {
				height *= 1.35
			}
			weight := 0.78 + rng.Float64()*0.55
			p.Control = append(p.Control, model.ControlPoint{
				Row: row,
				Col: col,
				Position: model.Vec3{
					X: xOffset + (u-0.5)*4.0,
					Y: height,
					Z: (v - 0.5) * 3.6,
				},
				Weight: weight,
			})
		}
	}
	p.Trim = []model.Vec2{
		{U: 0.07, V: 0.08},
		{U: 0.93, V: 0.08},
		{U: 0.93, V: 0.82},
		{U: 0.61, V: 0.92},
		{U: 0.09, V: 0.86},
		{U: 0.07, V: 0.08},
	}
	return p
}

func appendTruth(gt *model.GroundTruth, p model.SurfacePatch) {
	u0, u1, v0, v1 := p.Domain()
	const n = 8
	for iy := 0; iy <= n; iy++ {
		for ix := 0; ix <= n; ix++ {
			u := u0 + (u1-u0)*float64(ix)/float64(n)
			v := v0 + (v1-v0)*float64(iy)/float64(n)
			pos, su, sv, suu, suv, svv := evalDirect(p, u, v)
			nrm := su.Cross(sv).Normalize()
			k := maxPrincipalCurvature(su, sv, suu, suv, svv, nrm)
			gt.Samples = append(gt.Samples, model.TruthSample{
				PatchID: p.ID, U: u, V: v, Position: pos, Normal: nrm, Curvature: k,
			})
		}
	}

	box := model.EmptyBBox()
	const dense = 24
	for iy := 0; iy <= dense; iy++ {
		for ix := 0; ix <= dense; ix++ {
			u := u0 + (u1-u0)*float64(ix)/float64(dense)
			v := v0 + (v1-v0)*float64(iy)/float64(dense)
			pos, _, _, _, _, _ := evalDirect(p, u, v)
			box.Add(pos)
		}
	}
	gt.Bounds = append(gt.Bounds, model.BBoxTruth{PatchID: p.ID, Box: box})

	gt.TrimChecks = append(gt.TrimChecks,
		model.TrimTruth{PatchID: p.ID, Point: model.Vec2{U: 0.5, V: 0.5}, Inside: pointInPolygon(model.Vec2{U: 0.5, V: 0.5}, p.Trim)},
		model.TrimTruth{PatchID: p.ID, Point: model.Vec2{U: 0.01, V: 0.5}, Inside: pointInPolygon(model.Vec2{U: 0.01, V: 0.5}, p.Trim)},
		model.TrimTruth{PatchID: p.ID, Point: model.Vec2{U: 0.5, V: 0.98}, Inside: pointInPolygon(model.Vec2{U: 0.5, V: 0.98}, p.Trim)},
	)
}

func evalDirect(p model.SurfacePatch, u, v float64) (model.Vec3, model.Vec3, model.Vec3, model.Vec3, model.Vec3, model.Vec3) {
	base := ratGridFromPatch(p)
	du, okU := deriveU(base)
	dv, okV := deriveV(base)
	var duu, dvv, duv ratGrid
	var okUU, okVV, okUV bool
	if okU {
		duu, okUU = deriveU(du)
		duv, okUV = deriveV(du)
	}
	if okV {
		dvv, okVV = deriveV(dv)
	}
	a00, w00 := evalRatGrid(base, u, v)
	s := a00.Mul(1 / w00)
	a10, w10 := evalOptional(du, okU, u, v)
	a01, w01 := evalOptional(dv, okV, u, v)
	a20, w20 := evalOptional(duu, okUU, u, v)
	a02, w02 := evalOptional(dvv, okVV, u, v)
	a11, w11 := evalOptional(duv, okUV, u, v)
	su := a10.Sub(s.Mul(w10)).Mul(1 / w00)
	sv := a01.Sub(s.Mul(w01)).Mul(1 / w00)
	suu := a20.Sub(su.Mul(2 * w10)).Sub(s.Mul(w20)).Mul(1 / w00)
	svv := a02.Sub(sv.Mul(2 * w01)).Sub(s.Mul(w02)).Mul(1 / w00)
	suv := a11.Sub(su.Mul(w01)).Sub(sv.Mul(w10)).Sub(s.Mul(w11)).Mul(1 / w00)
	return s, su, sv, suu, suv, svv
}

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

func deriveU(g ratGrid) (ratGrid, bool) {
	if g.DegU < 1 || g.Cols < 2 {
		return ratGrid{}, false
	}
	out := ratGrid{
		Rows: g.Rows, Cols: g.Cols - 1, DegU: g.DegU - 1, DegV: g.DegV,
		KU:   append([]float64(nil), g.KU[1:len(g.KU)-1]...),
		KV:   append([]float64(nil), g.KV...),
		Data: make([]ratPoint, g.Rows*(g.Cols-1)),
	}
	p := float64(g.DegU)
	for row := 0; row < g.Rows; row++ {
		for col := 0; col < out.Cols; col++ {
			a, b := g.Data[row*g.Cols+col], g.Data[row*g.Cols+col+1]
			den := g.KU[col+g.DegU+1] - g.KU[col+1]
			out.Data[row*out.Cols+col] = ratPoint{
				A: a.A.Add(b.A.Sub(a.A).Mul(p / den)),
				W: a.W + (b.W-a.W)*p/den,
			}
		}
	}
	return out, true
}

func deriveV(g ratGrid) (ratGrid, bool) {
	if g.DegV < 1 || g.Rows < 2 {
		return ratGrid{}, false
	}
	out := ratGrid{
		Rows: g.Rows - 1, Cols: g.Cols, DegU: g.DegU, DegV: g.DegV - 1,
		KU:   append([]float64(nil), g.KU...),
		KV:   append([]float64(nil), g.KV[1:len(g.KV)-1]...),
		Data: make([]ratPoint, (g.Rows-1)*g.Cols),
	}
	p := float64(g.DegV)
	for row := 0; row < out.Rows; row++ {
		for col := 0; col < g.Cols; col++ {
			a, b := g.Data[row*g.Cols+col], g.Data[(row+1)*g.Cols+col]
			den := g.KV[row+g.DegV+1] - g.KV[row+1]
			out.Data[row*out.Cols+col] = ratPoint{
				A: a.A.Add(b.A.Sub(a.A).Mul(p / den)),
				W: a.W + (b.W-a.W)*p/den,
			}
		}
	}
	return out, true
}

func evalOptional(g ratGrid, ok bool, u, v float64) (model.Vec3, float64) {
	if !ok {
		return model.Vec3{}, 0
	}
	return evalRatGrid(g, u, v)
}

func evalRatGrid(g ratGrid, u, v float64) (model.Vec3, float64) {
	u = math.Max(g.KU[g.DegU], math.Min(g.KU[g.Cols], u))
	v = math.Max(g.KV[g.DegV], math.Min(g.KV[g.Rows], v))
	bu := localBasisRow(g.KU, g.DegU, g.Cols, u)
	bv := localBasisRow(g.KV, g.DegV, g.Rows, v)
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

func localBasisRow(knots []float64, degree, count int, u float64) []float64 {
	out := make([]float64, count+1)
	if degree < 0 {
		return out
	}
	n := count - 1
	span := findSpan(n, degree, u, knots)
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

func findSpan(n, p int, u float64, knots []float64) int {
	if u >= knots[n+1] {
		return n
	}
	if u <= knots[p] {
		return p
	}
	low, high := p, n+1
	mid := (low + high) / 2
	for u < knots[mid] || u >= knots[mid+1] {
		if u < knots[mid] {
			high = mid
		} else {
			low = mid
		}
		mid = (low + high) / 2
	}
	return mid
}

func maxPrincipalCurvature(su, sv, suu, suv, svv, n model.Vec3) float64 {
	e, f, g := su.Dot(su), su.Dot(sv), sv.Dot(sv)
	l, m, nn := n.Dot(suu), n.Dot(suv), n.Dot(svv)
	den := e*g - f*f
	if math.Abs(den) < 1e-14 {
		return 0
	}
	gauss := (l*nn - m*m) / den
	mean := (e*nn - 2*f*m + g*l) / (2 * den)
	disc := mean*mean - gauss
	if disc < 0 {
		disc = 0
	}
	return math.Abs(mean) + math.Sqrt(disc)
}

func pointInPolygon(p model.Vec2, poly []model.Vec2) bool {
	inside := false
	j := len(poly) - 1
	for i := 0; i < len(poly); i++ {
		yi, yj := poly[i].V, poly[j].V
		xi, xj := poly[i].U, poly[j].U
		if (yi > p.V) != (yj > p.V) {
			x := (xj-xi)*(p.V-yi)/(yj-yi) + xi
			if p.U < x {
				inside = !inside
			}
		}
		j = i
	}
	return inside
}

func makeInspectionPlate() model.SubdivMesh {
	m := model.SubdivMesh{ID: 1, Name: "inspection-plate", Color: model.Color{R: 43, G: 56, B: 64}}
	minX, maxX := -8.0, 8.0
	minZ, maxZ := -2.8, 2.8
	top, bottom := -1.45, -1.72
	corners := []model.Vec3{
		{X: minX, Y: bottom, Z: minZ}, {X: maxX, Y: bottom, Z: minZ},
		{X: maxX, Y: bottom, Z: maxZ}, {X: minX, Y: bottom, Z: maxZ},
		{X: minX, Y: top, Z: minZ}, {X: maxX, Y: top, Z: minZ},
		{X: maxX, Y: top, Z: maxZ}, {X: minX, Y: top, Z: maxZ},
	}
	m.Vertices = append(m.Vertices, corners...)
	quads := [][4]int{
		{4, 5, 6, 7}, {0, 3, 2, 1}, {0, 4, 7, 3}, {1, 2, 6, 5}, {3, 7, 6, 2}, {0, 1, 5, 4},
	}
	for _, q := range quads {
		m.Indices = append(m.Indices, q[0], q[1], q[2], q[0], q[2], q[3])
	}
	m.Normals = make([]model.Vec3, len(m.Vertices))
	for i := range m.Normals {
		m.Normals[i] = model.Vec3{Y: 1}
	}
	return m
}

func makeDatumMarker(x, y, z float64) model.SubdivMesh {
	m := model.SubdivMesh{ID: 2, Name: "datum-marker", Color: model.Color{R: 214, G: 150, B: 58}}
	m.Vertices = []model.Vec3{
		{X: x, Y: y + 0.55, Z: z}, {X: x - 0.25, Y: y, Z: z - 0.25},
		{X: x + 0.25, Y: y, Z: z - 0.25}, {X: x + 0.25, Y: y, Z: z + 0.25},
		{X: x - 0.25, Y: y, Z: z + 0.25},
	}
	m.Indices = []int{
		0, 2, 1, 0, 3, 2, 0, 4, 3, 0, 1, 4,
		1, 2, 3, 1, 3, 4,
	}
	m.Normals = make([]model.Vec3, len(m.Vertices))
	for i := range m.Vertices {
		m.Normals[i] = m.Vertices[i].Sub(model.Vec3{X: x, Y: y, Z: z}).Normalize()
	}
	return m
}
