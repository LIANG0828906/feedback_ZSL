package terrainbake

import (
	"fmt"
	"math"
	"sort"
)

type Vertex struct {
	ID       int   `json:"id"`
	X        int64 `json:"x"`
	Y        int64 `json:"y"`
	HeightMM int64 `json:"height_mm"`
}

type Triangle struct {
	A             int   `json:"a"`
	B             int   `json:"b"`
	C             int   `json:"c"`
	HeightErrorUM int64 `json:"height_error_um"`
	Boundary      bool  `json:"boundary"`
}

type Mesh struct {
	BlockID            string     `json:"block_id"`
	Vertices           []Vertex   `json:"vertices"`
	Triangles          []Triangle `json:"triangles"`
	LeafCount          int        `json:"leaf_count"`
	ToleranceUM        int64      `json:"tolerance_um"`
	HeightErrorMaxUM   int64      `json:"height_error_max_um"`
	BoundaryErrorMaxUM int64      `json:"boundary_error_max_um"`
	BoundarySpacing    int64      `json:"boundary_spacing"`
	Undersampled       bool       `json:"undersampled"`
	VertexDigest       string     `json:"vertex_digest"`
	TriangleDigest     string     `json:"triangle_digest"`
}

type MeshHeightFunc func(x, y int64) int64

type meshCell struct {
	x0, y0 int64
	size   int64
	errUM  int64
}

func (c meshCell) x1() int64 { return c.x0 + c.size }
func (c meshCell) y1() int64 { return c.y0 + c.size }

func (c meshCell) less(other meshCell) bool {
	if c.errUM != other.errUM {
		return c.errUM > other.errUM
	}
	if c.y0 != other.y0 {
		return c.y0 < other.y0
	}
	if c.x0 != other.x0 {
		return c.x0 < other.x0
	}
	return c.size < other.size
}

// BuildMesh produces a deterministic adaptive mesh for one block. Boundary
// cells are forced to the declared seam spacing; interior cells are refined
// only while their sampled chord error exceeds tolerance and the vertex and
// triangle budgets remain feasible.
func BuildMesh(p *Project, b BlockSpec, heightFn, boundaryHeightFn MeshHeightFunc) (*Mesh, error) {
	if p == nil {
		return nil, errSchema("project", "nil project")
	}
	if err := validateMeshParameters(b); err != nil {
		return nil, err
	}
	if b.Rect.Width() != b.Rect.Height() {
		return nil, errPara("block.size", "square blocks", "adaptive quadtree requires square block coverage")
	}
	sideSpacing, err := blockSideSpacing(p, b)
	if err != nil {
		return nil, err
	}
	rootSpacing := sideSpacing.minimum()
	if rootSpacing <= 0 {
		rootSpacing = DefaultSeamSpacing
	}
	if !isPowerOfTwo(rootSpacing) || !isPowerOfTwo(b.Rect.Width()) {
		return nil, errPara("seam.sample_spacing", "power of two dividing block size", "seam spacing and block size must be powers of two")
	}
	if b.Rect.Width()%rootSpacing != 0 {
		return nil, errPara("seam.sample_spacing", "divides block width and height", "seam spacing must divide block coverage")
	}
	if heightFn == nil {
		return nil, errPara("height", "non-nil", "height evaluator is required")
	}
	if boundaryHeightFn == nil {
		boundaryHeightFn = heightFn
	}
	combined := func(x, y int64) int64 {
		if isBlockBoundary(b.Rect, x, y) {
			return boundaryHeightFn(x, y)
		}
		return heightFn(x, y)
	}

	maxLeaves := minInt(b.Budget.MaxVertices/4, b.Budget.MaxTriangles/4)
	if maxLeaves < 1 {
		maxLeaves = 1
	}
	leaves := []meshCell{{x0: b.Rect.MinX, y0: b.Rect.MinY, size: b.Rect.Width()}}
	for {
		changed := false
		var next []meshCell
		for _, c := range leaves {
			need := sideSpacing.requiredFor(b.Rect, c)
			if need > 0 && c.size > need {
				next = append(next, splitCell(c)...)
				changed = true
			} else {
				next = append(next, c)
			}
		}
		leaves = next
		if !changed {
			break
		}
		if len(leaves) > maxLeaves {
			return nil, errPara(
				"budget.max_vertices",
				fmt.Sprintf(">= %d vertices for boundary spacing %d", len(leaves)*4, rootSpacing),
				"budget cannot hold the forced conforming boundary grid",
			)
		}
	}
	for i := range leaves {
		leaves[i].errUM = cellChordError(leaves[i], combined)
	}
	minCell := int64(2)
	if rootSpacing >= 8 {
		minCell = 2
	}
	for {
		best := -1
		for i, c := range leaves {
			if c.errUM <= b.ToleranceUM || c.size <= minCell {
				continue
			}
			if c.touchesBoundary(b.Rect) {
				continue
			}
			if best < 0 || c.less(leaves[best]) {
				best = i
			}
		}
		if best < 0 || len(leaves)+3 > maxLeaves {
			break
		}
		c := leaves[best]
		leaves = append(leaves[:best], leaves[best+1:]...)
		children := splitCell(c)
		for i := range children {
			children[i].errUM = cellChordError(children[i], combined)
		}
		leaves = append(leaves, children...)
	}
	sort.Slice(leaves, func(i, j int) bool {
		if leaves[i].y0 != leaves[j].y0 {
			return leaves[i].y0 < leaves[j].y0
		}
		if leaves[i].x0 != leaves[j].x0 {
			return leaves[i].x0 < leaves[j].x0
		}
		return leaves[i].size < leaves[j].size
	})

	mesh, err := triangulateLeaves(b, leaves, combined)
	if err != nil {
		return nil, err
	}
	mesh.LeafCount = len(leaves)
	mesh.BoundarySpacing = rootSpacing
	mesh.ToleranceUM = b.ToleranceUM
	if len(mesh.Vertices) > b.Budget.MaxVertices {
		return nil, errPara(
			"budget.max_vertices",
			fmt.Sprintf(">= %d", len(mesh.Vertices)),
			"adaptive result exceeds declared vertex budget",
		)
	}
	if len(mesh.Triangles) > b.Budget.MaxTriangles {
		return nil, errPara(
			"budget.max_triangles",
			fmt.Sprintf(">= %d", len(mesh.Triangles)),
			"adaptive result exceeds declared triangle budget",
		)
	}
	mesh.VertexDigest = digestVertices(mesh.Vertices)
	mesh.TriangleDigest = digestTriangles(mesh.Triangles)
	return mesh, nil
}

type sideSpacings struct {
	left, right, bottom, top int64
}

func (s sideSpacings) minimum() int64 {
	min := int64(0)
	for _, v := range []int64{s.left, s.right, s.bottom, s.top} {
		if v > 0 && (min == 0 || v < min) {
			min = v
		}
	}
	return min
}

func (s sideSpacings) requiredFor(rect Rect, c meshCell) int64 {
	var need int64
	add := func(v int64) {
		if v > 0 && (need == 0 || v < need) {
			need = v
		}
	}
	if c.x0 == rect.MinX && s.left > 0 {
		add(s.left)
	}
	if c.x1() == rect.MaxX && s.right > 0 {
		add(s.right)
	}
	if c.y0 == rect.MinY && s.bottom > 0 {
		add(s.bottom)
	}
	if c.y1() == rect.MaxY && s.top > 0 {
		add(s.top)
	}
	return need
}

func blockSideSpacing(p *Project, b BlockSpec) (sideSpacings, error) {
	var out sideSpacings
	set := func(side *int64, v int64, name string) error {
		if *side == 0 {
			*side = v
			return nil
		}
		if *side != v {
			return errPara(
				"seams."+b.ID+"."+name+".sample_spacing",
				"one spacing per block side",
				"conflicting seam spacings on one block side",
			)
		}
		return nil
	}
	for _, s := range p.Seams {
		if s.A != b.ID && s.B != b.ID {
			continue
		}
		if s.SampleSpacing <= 0 || !isPowerOfTwo(s.SampleSpacing) {
			return sideSpacings{}, errPara("seam."+s.ID+".sample_spacing", "positive power of two", "invalid seam sampling")
		}
		if s.Axis == AxisVertical {
			if s.Coordinate == b.Rect.MinX {
				if err := set(&out.left, s.SampleSpacing, "left"); err != nil {
					return sideSpacings{}, err
				}
			}
			if s.Coordinate == b.Rect.MaxX {
				if err := set(&out.right, s.SampleSpacing, "right"); err != nil {
					return sideSpacings{}, err
				}
			}
		} else {
			if s.Coordinate == b.Rect.MinY {
				if err := set(&out.bottom, s.SampleSpacing, "bottom"); err != nil {
					return sideSpacings{}, err
				}
			}
			if s.Coordinate == b.Rect.MaxY {
				if err := set(&out.top, s.SampleSpacing, "top"); err != nil {
					return sideSpacings{}, err
				}
			}
		}
	}
	if out.left == 0 {
		out.left = DefaultSeamSpacing
	}
	if out.right == 0 {
		out.right = DefaultSeamSpacing
	}
	if out.bottom == 0 {
		out.bottom = DefaultSeamSpacing
	}
	if out.top == 0 {
		out.top = DefaultSeamSpacing
	}
	return out, nil
}

func validateMeshParameters(b BlockSpec) error {
	const (
		minVerts = 16
		maxVerts = 1_000_000
		minTris  = 8
		maxTris  = 2_000_000
		minTol   = 1
		maxTol   = 1_000_000_000
	)
	if b.Budget.MaxVertices < minVerts || b.Budget.MaxVertices > maxVerts {
		return errPara("budget.max_vertices", fmt.Sprintf("[%d,%d]", minVerts, maxVerts), "vertex budget is outside the legal interval")
	}
	if b.Budget.MaxTriangles < minTris || b.Budget.MaxTriangles > maxTris {
		return errPara("budget.max_triangles", fmt.Sprintf("[%d,%d]", minTris, maxTris), "triangle budget is outside the legal interval")
	}
	if b.ToleranceUM < minTol || b.ToleranceUM > maxTol {
		return errPara("chord_tolerance", fmt.Sprintf("[%d,%d] um", minTol, maxTol), "chord tolerance is outside the legal interval")
	}
	return nil
}

func splitCell(c meshCell) []meshCell {
	h := c.size / 2
	return []meshCell{
		{x0: c.x0, y0: c.y0, size: h},
		{x0: c.x0 + h, y0: c.y0, size: h},
		{x0: c.x0, y0: c.y0 + h, size: h},
		{x0: c.x0 + h, y0: c.y0 + h, size: h},
	}
}

func (c meshCell) touchesBoundary(rect Rect) bool {
	return c.x0 == rect.MinX || c.y0 == rect.MinY || c.x1() == rect.MaxX || c.y1() == rect.MaxY
}

func isBlockBoundary(rect Rect, x, y int64) bool {
	return x == rect.MinX || x == rect.MaxX || y == rect.MinY || y == rect.MaxY
}

func isPowerOfTwo(v int64) bool {
	return v > 0 && v&(v-1) == 0
}

func cellChordError(c meshCell, height MeshHeightFunc) int64 {
	// Compare analytic heights with bilinear interpolation at the centre and
	// four edge midpoints. This is a conservative sampling estimate used only
	// to decide refinement; final per-triangle errors are reported separately.
	h00 := height(c.x0, c.y0)
	h10 := height(c.x1(), c.y0)
	h01 := height(c.x0, c.y1())
	h11 := height(c.x1(), c.y1())
	mx := c.x0 + c.size/2
	my := c.y0 + c.size/2
	points := []struct {
		x, y int64
		pred int64
	}{
		{mx, my, (h00 + h10 + h01 + h11) / 4},
		{mx, c.y0, (h00 + h10) / 2},
		{mx, c.y1(), (h01 + h11) / 2},
		{c.x0, my, (h00 + h01) / 2},
		{c.x1(), my, (h10 + h11) / 2},
	}
	var maxErr int64
	for _, p := range points {
		d := height(p.x, p.y) - p.pred
		if d < 0 {
			d = -d
		}
		um := d * 1000
		if um > maxErr {
			maxErr = um
		}
	}
	return maxErr
}

func triangulateLeaves(b BlockSpec, leaves []meshCell, height MeshHeightFunc) (*Mesh, error) {
	vertical := map[int64]map[int64]bool{}
	horizontal := map[int64]map[int64]bool{}
	for _, c := range leaves {
		pts := [][2]int64{{c.x0, c.y0}, {c.x1(), c.y0}, {c.x0, c.y1()}, {c.x1(), c.y1()}}
		for _, p := range pts {
			if vertical[p[0]] == nil {
				vertical[p[0]] = map[int64]bool{}
			}
			vertical[p[0]][p[1]] = true
			if horizontal[p[1]] == nil {
				horizontal[p[1]] = map[int64]bool{}
			}
			horizontal[p[1]][p[0]] = true
		}
	}
	mesh := &Mesh{BlockID: b.ID, Vertices: []Vertex{}, Triangles: []Triangle{}}
	index := map[[2]int64]int{}
	addVertex := func(x, y int64) int {
		key := [2]int64{x, y}
		if id, ok := index[key]; ok {
			return id
		}
		id := len(mesh.Vertices)
		index[key] = id
		mesh.Vertices = append(mesh.Vertices, Vertex{ID: id, X: x, Y: y, HeightMM: height(x, y)})
		return id
	}
	for _, c := range leaves {
		poly := make([][2]int64, 0, 16)
		// Bottom: left -> right.
		if xs := sortedKeysBetween(horizontal[c.y0], c.x0, c.x1()); len(xs) > 0 {
			for _, x := range xs {
				poly = append(poly, [2]int64{x, c.y0})
			}
		}
		// Right: bottom -> top.
		if ys := sortedKeysBetween(vertical[c.x1()], c.y0, c.y1()); len(ys) > 0 {
			for _, y := range ys {
				if y == c.y0 {
					continue
				}
				poly = append(poly, [2]int64{c.x1(), y})
			}
		}
		// Top: right -> left.
		if xs := sortedKeysBetween(horizontal[c.y1()], c.x0, c.x1()); len(xs) > 0 {
			for i := len(xs) - 1; i >= 0; i-- {
				if xs[i] == c.x1() {
					continue
				}
				poly = append(poly, [2]int64{xs[i], c.y1()})
			}
		}
		// Left: top -> bottom.
		if ys := sortedKeysBetween(vertical[c.x0], c.y0, c.y1()); len(ys) > 0 {
			for i := len(ys) - 1; i >= 0; i-- {
				if ys[i] == c.y1() || ys[i] == c.y0 {
					continue
				}
				poly = append(poly, [2]int64{c.x0, ys[i]})
			}
		}
		poly = dedupePolygon(poly)
		if len(poly) < 3 {
			continue
		}
		ids := make([]int, len(poly))
		for i, p := range poly {
			ids[i] = addVertex(p[0], p[1])
		}
		for i := 1; i+1 < len(ids); i++ {
			a, bidx, cidx := ids[0], ids[i], ids[i+1]
			if a == bidx || bidx == cidx || a == cidx {
				continue
			}
			if triangleArea(mesh.Vertices[a], mesh.Vertices[bidx], mesh.Vertices[cidx]) == 0 {
				continue
			}
			mesh.Triangles = append(mesh.Triangles, Triangle{A: a, B: bidx, C: cidx})
		}
	}
	for i := range mesh.Triangles {
		t := &mesh.Triangles[i]
		t.HeightErrorUM = triangleError(mesh.Vertices, *t, height)
		t.Boundary = triangleTouchesBoundary(b.Rect, mesh.Vertices, *t)
		if t.HeightErrorUM > mesh.HeightErrorMaxUM {
			mesh.HeightErrorMaxUM = t.HeightErrorUM
		}
		if t.Boundary {
			if t.HeightErrorUM > mesh.BoundaryErrorMaxUM {
				mesh.BoundaryErrorMaxUM = t.HeightErrorUM
			}
			if t.HeightErrorUM > b.ToleranceUM {
				mesh.Undersampled = true
			}
		}
	}
	return mesh, nil
}

func sortedKeysBetween(m map[int64]bool, lo, hi int64) []int64 {
	if m == nil {
		return nil
	}
	out := make([]int64, 0, len(m))
	for v := range m {
		if v >= lo && v <= hi {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func dedupePolygon(in [][2]int64) [][2]int64 {
	out := make([][2]int64, 0, len(in))
	for _, p := range in {
		if len(out) > 0 && out[len(out)-1] == p {
			continue
		}
		out = append(out, p)
	}
	if len(out) > 1 && out[0] == out[len(out)-1] {
		out = out[:len(out)-1]
	}
	return out
}

func triangleArea(a, b, c Vertex) int64 {
	return (b.X-a.X)*(c.Y-a.Y) - (b.Y-a.Y)*(c.X-a.X)
}

func triangleTouchesBoundary(rect Rect, verts []Vertex, t Triangle) bool {
	tri := []Vertex{verts[t.A], verts[t.B], verts[t.C]}
	for i := 0; i < 3; i++ {
		p, q := tri[i], tri[(i+1)%3]
		if p.X == q.X && (p.X == rect.MinX || p.X == rect.MaxX) {
			return true
		}
		if p.Y == q.Y && (p.Y == rect.MinY || p.Y == rect.MaxY) {
			return true
		}
	}
	return false
}

func triangleError(verts []Vertex, t Triangle, height MeshHeightFunc) int64 {
	a, b, c := verts[t.A], verts[t.B], verts[t.C]
	type sample struct{ x, y int64 }
	samples := []sample{
		{x: (a.X + b.X + c.X) / 3, y: (a.Y + b.Y + c.Y) / 3},
		{x: (a.X + b.X) / 2, y: (a.Y + b.Y) / 2},
		{x: (b.X + c.X) / 2, y: (b.Y + c.Y) / 2},
		{x: (c.X + a.X) / 2, y: (c.Y + a.Y) / 2},
	}
	var maxErr int64
	for _, p := range samples {
		pred := planeHeight(a, b, c, float64(p.x), float64(p.y))
		truth := float64(height(p.x, p.y))
		d := math.Abs(truth - pred)
		um := int64(math.Round(d * 1000))
		if um > maxErr {
			maxErr = um
		}
	}
	return maxErr
}

func planeHeight(a, b, c Vertex, x, y float64) float64 {
	det := (float64(b.Y)-float64(c.Y))*(float64(a.X)-float64(c.X)) +
		(float64(c.X)-float64(b.X))*(float64(a.Y)-float64(c.Y))
	if math.Abs(det) < 1e-9 {
		return float64(a.HeightMM)
	}
	l1 := ((float64(b.Y)-float64(c.Y))*(x-float64(c.X)) +
		(float64(c.X)-float64(b.X))*(y-float64(c.Y))) / det
	l2 := ((float64(c.Y)-float64(a.Y))*(x-float64(c.X)) +
		(float64(a.X)-float64(c.X))*(y-float64(c.Y))) / det
	l3 := 1 - l1 - l2
	return l1*float64(a.HeightMM) + l2*float64(b.HeightMM) + l3*float64(c.HeightMM)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
