package terrain

import (
	"fmt"
	"math"
	"sort"
)

func edgePosition(p GridPoint, side string) int {
	if side == SideEast || side == SideWest {
		return p.Y
	}
	return p.X
}

func sidePoint(g Grid, ox, oz float64, side string, pos int) Vertex {
	var x, y int
	switch side {
	case SideNorth:
		x, y = pos, 0
	case SideSouth:
		x, y = pos, g.Height-1
	case SideWest:
		x, y = 0, pos
	case SideEast:
		x, y = g.Width-1, pos
	}
	return Vertex{GridX: x, GridY: y, X: ox + float64(x)*g.CellSize, Y: gridHeight(g, x, y), Z: oz + float64(y)*g.CellSize}
}

func inwardGridDirection(side string) (int, int) {
	switch side {
	case SideEast:
		return -1, 0
	case SideWest:
		return 1, 0
	case SideSouth:
		return 0, -1
	default:
		return 0, 1
	}
}

func RefineBoundary1D(g Grid, side string, tolerance float64, base []int) []int {
	n := edgeLength(g, side)
	selected := map[int]bool{0: true, n - 1: true}
	for _, p := range base {
		if p >= 0 && p < n {
			selected[p] = true
		}
	}
	for {
		positions := make([]int, 0, len(selected))
		for p := range selected {
			positions = append(positions, p)
		}
		sort.Ints(positions)
		bestP := -1
		bestErr := 0.0
		for ii := 0; ii+1 < len(positions); ii++ {
			a, b := positions[ii], positions[ii+1]
			if b <= a+1 {
				continue
			}
			va := boundaryHeightAt(g, side, a)
			vb := boundaryHeightAt(g, side, b)
			for p := a + 1; p < b; p++ {
				want := boundaryHeightAt(g, side, p)
				interp := va + (vb-va)*float64(p-a)/float64(b-a)
				err := math.Abs(want - interp)
				if err > bestErr+1e-12 || (math.Abs(err-bestErr) <= 1e-12 && (bestP < 0 || p < bestP)) {
					bestErr = err
					bestP = p
				}
			}
		}
		if bestP < 0 || bestErr <= tolerance {
			break
		}
		selected[bestP] = true
	}
	out := make([]int, 0, len(selected))
	for p := range selected {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

func boundaryHeightAt(g Grid, side string, pos int) float64 {
	if side == SideEast {
		return gridHeight(g, g.Width-1, pos)
	}
	if side == SideWest {
		return gridHeight(g, 0, pos)
	}
	if side == SideSouth {
		return gridHeight(g, pos, g.Height-1)
	}
	return gridHeight(g, pos, 0)
}

type boundaryPair struct {
	I, J     int
	Residual float64
}

func pairBoundarySides(left, right BoundarySide, buffer float64) ([]boundaryPair, bool, float64, error) {
	if buffer <= 0 {
		buffer = 1e-6
	}
	n := min(len(left.Points), len(right.Points))
	directMax := 0.0
	for i := 0; i < n; i++ {
		directMax = math.Max(directMax, distance3(left.Points[i], right.Points[i]))
	}

	pairs := make([]boundaryPair, 0, len(left.Points))
	nearestMax := 0.0
	for i, lp := range left.Points {
		bestJ, bestD := -1, math.Inf(1)
		for j, rp := range right.Points {
			d := distance3(lp, rp)
			if d < bestD {
				bestD, bestJ = d, j
			}
		}
		if bestJ < 0 || bestD > buffer {
			angle, _ := maxDirectFold(left, right)
			reason := fmt.Sprintf("point %d has nearest-side distance %.6f > buffer %.6f", i, bestD, buffer)
			return nil, false, directMax, &AlignError{BlockA: left.BlockNo, BlockB: right.BlockNo, MeasuredAngle: angle, Reason: reason}
		}
		nearestMax = math.Max(nearestMax, bestD)
		pairs = append(pairs, boundaryPair{I: i, J: bestJ, Residual: bestD})
	}

	orderMismatch := false
	lastJ := -1
	for _, p := range pairs {
		if p.J <= lastJ {
			orderMismatch = true
			break
		}
		lastJ = p.J
	}
	alignmentEps := math.Max(1e-6, buffer*0.01)
	if directMax > alignmentEps {
		orderMismatch = true
	}
	_ = nearestMax
	return pairs, orderMismatch, directMax, nil
}

func distance3(a BoundaryPoint, b BoundaryPoint) float64 {
	dx, dy, dz := a.X-b.X, a.Y-b.Y, a.Z-b.Z
	return math.Sqrt(dx*dx + dy*dy + dz*dz)
}

func maxDirectFold(left, right BoundarySide) (float64, *AngleLocation) {
	if len(left.SegmentNormals) > 0 && len(right.SegmentNormals) > 0 {
		n := min(len(left.SegmentNormals), len(right.SegmentNormals))
		best := 0.0
		var loc *AngleLocation
		for i := 0; i < n; i++ {
			angle := normalAngle(left.SegmentNormals[i], right.SegmentNormals[i])
			if angle > best {
				best = angle
				a, b := left.Points[i], left.Points[min(i+1, len(left.Points)-1)]
				mid := [3]float64{(a.X + b.X) / 2, (a.Y + b.Y) / 2, (a.Z + b.Z) / 2}
				loc = &AngleLocation{T: (a.T + b.T) / 2, At: mid}
			}
		}
		return best, loc
	}
	n := min(len(left.Tangents), len(right.Tangents))
	if n == 0 {
		n = min(len(left.Points), len(right.Points))
	}
	best := 0.0
	var loc *AngleLocation
	for i := 0; i < n; i++ {
		var ta, tb [3]float64
		if i < len(left.Tangents) {
			ta = left.Tangents[i]
		}
		if i < len(right.Tangents) {
			tb = right.Tangents[i]
		}
		angle := foldAngle(ta, tb)
		if angle > best {
			best = angle
			var p BoundaryPoint
			if i < len(left.Points) {
				p = left.Points[i]
			}
			loc = &AngleLocation{T: p.T, At: [3]float64{p.X, p.Y, p.Z}}
		}
	}
	return best, loc
}

func normalAngle(a, b [3]float64) float64 {
	na := math.Sqrt(a[0]*a[0] + a[1]*a[1] + a[2]*a[2])
	nb := math.Sqrt(b[0]*b[0] + b[1]*b[1] + b[2]*b[2])
	if na < 1e-12 || nb < 1e-12 {
		return 0
	}
	dot := (a[0]*b[0] + a[1]*b[1] + a[2]*b[2]) / (na * nb)
	dot = math.Max(-1, math.Min(1, dot))
	return math.Acos(dot) * 180 / math.Pi
}

func foldAngle(a, b [3]float64) float64 {
	na := math.Sqrt(a[0]*a[0] + a[1]*a[1] + a[2]*a[2])
	nb := math.Sqrt(b[0]*b[0] + b[1]*b[1] + b[2]*b[2])
	if na < 1e-12 || nb < 1e-12 {
		return 0
	}
	dot := (a[0]*b[0] + a[1]*b[1] + a[2]*b[2]) / (na * nb)
	dot = math.Max(-1, math.Min(1, dot))
	included := math.Acos(dot) * 180 / math.Pi
	return math.Abs(180 - included)
}

func AttributeBoundary(c FaultCase) (*AttributionReport, error) {
	rep := &AttributionReport{
		CaseID: c.ID, BoundaryID: c.BoundaryID, BlockA: c.BlockA, BlockB: c.BlockB,
		LimitDeg: c.FoldLimitDeg, MaxHeightErrorA: c.Left.MaxHeightError, MaxHeightErrorB: c.Right.MaxHeightError,
		Evidence: []string{},
	}
	angle, angleLoc := maxDirectFold(c.Left, c.Right)
	rep.MaxFoldAngleDeg = round9(angle)
	rep.MaxAngleLocation = angleLoc
	if rep.MaxAngleLocation != nil {
		rep.MaxAngleLocation.BlockA = c.BlockA
		rep.MaxAngleLocation.BlockB = c.BlockB
		rep.MaxAngleLocation.HeightErrorA = c.Left.MaxHeightError
		rep.MaxAngleLocation.HeightErrorB = c.Right.MaxHeightError
	}

	_, mismatch, residual, err := pairBoundarySides(c.Left, c.Right, c.AlignmentBuffer)
	rep.MaxPairResidual = round9(residual)
	if alignErr, ok := err.(*AlignError); ok {
		rep.Category = CategoryAlignUnavailable
		rep.ErrorCode = CodeAlign
		rep.Aligned = false
		rep.Evidence = append(rep.Evidence, alignErr.Reason, fmt.Sprintf("measured_fold_angle=%.6f", alignErr.MeasuredAngle))
		return rep, alignErr
	}
	if err != nil {
		return rep, err
	}

	rep.OrderMismatch = mismatch
	rep.Aligned = !mismatch
	angleOver := angle > c.FoldLimitDeg
	tolA := c.HeightToleranceA
	if tolA == 0 {
		tolA = c.HeightTolerance
	}
	tolB := c.HeightToleranceB
	if tolB == 0 {
		tolB = c.HeightTolerance
	}
	underA := c.Left.MaxHeightError > tolA
	underB := c.Right.MaxHeightError > tolB
	under := underA || underB

	if mismatch {
		rep.Evidence = append(rep.Evidence, fmt.Sprintf("boundary vertex order/direct pairing inconsistent; max direct residual=%.6f", residual))
	}
	if underA {
		rep.Evidence = append(rep.Evidence, fmt.Sprintf("block %d boundary height error %.6f exceeds tolerance %.6f", c.BlockA, c.Left.MaxHeightError, c.HeightTolerance))
	}
	if underB {
		rep.Evidence = append(rep.Evidence, fmt.Sprintf("block %d boundary height error %.6f exceeds tolerance %.6f", c.BlockB, c.Right.MaxHeightError, c.HeightTolerance))
	}
	if angleOver {
		rep.Evidence = append(rep.Evidence, fmt.Sprintf("fold angle %.6f exceeds limit %.6f", angle, c.FoldLimitDeg))
	}

	switch {
	case mismatch && under && angleOver:
		rep.Category = CategoryMixed
	case mismatch:
		rep.Category = CategoryAlignment
		if !angleOver {
			rep.Evidence = append(rep.Evidence, "alignment sequence differs even though tangent angle is within limit")
		}
	case under && angleOver:
		rep.Category = CategoryUndersample
		rep.Evidence = append(rep.Evidence, "paired boundary positions/order are consistent; angle fault is explained by depth/height error")
	case angleOver:
		rep.Category = CategoryNaturalFold
		rep.Evidence = append(rep.Evidence, "angle exceeds limit but recorded height error is within tolerance")
	default:
		rep.Category = CategoryOK
	}
	return rep, nil
}

func round9(v float64) float64 { return math.Round(v*1e9) / 1e9 }

func ExtractBoundarySide(mesh *Mesh, block Block, side string) BoundarySide {
	out := BoundarySide{BlockNo: block.BlockNo, Side: side, MaxHeightError: 0}
	if mesh == nil {
		return out
	}
	boundaryVertices := map[int]bool{}
	for _, v := range mesh.Vertices {
		pos := edgePosition(GridPoint{X: v.GridX, Y: v.GridY}, side)
		want := sidePoint(block.Grid, block.OriginX, block.OriginZ, side, pos)
		if v.GridX == want.GridX && v.GridY == want.GridY {
			boundaryVertices[v.Index] = true
		}
	}
	indices := make([]int, 0, len(boundaryVertices))
	for idx := range boundaryVertices {
		indices = append(indices, idx)
	}
	sort.Slice(indices, func(i, j int) bool {
		a, b := mesh.Vertices[indices[i]], mesh.Vertices[indices[j]]
		return edgePosition(GridPoint{X: a.GridX, Y: a.GridY}, side) < edgePosition(GridPoint{X: b.GridX, Y: b.GridY}, side)
	})

	incident := map[int][]int{}
	for ti, tri := range mesh.Triangles {
		for _, vi := range tri.Vertices {
			incident[vi] = append(incident[vi], ti)
		}
	}
	errorByTri := map[int]float64{}
	for _, te := range mesh.TriangleErrors {
		errorByTri[te.TriangleIndex] = te.MaxAbsError
	}
	errorTris := map[int]bool{}

	idxToOrder := map[int]int{}
	for order, vi := range indices {
		idxToOrder[vi] = order
		v := mesh.Vertices[vi]
		pos := edgePosition(GridPoint{X: v.GridX, Y: v.GridY}, side)
		edgeLen := edgeLength(block.Grid, side)
		out.Points = append(out.Points, BoundaryPoint{
			T: float64(pos) / float64(edgeLen-1), GridX: v.GridX, GridY: v.GridY,
			X: v.X, Y: v.Y, Z: v.Z,
		})
		out.Tangents = append(out.Tangents, inwardTangent(mesh, block, side, vi, incident[vi]))
		for _, ti := range incident[vi] {
			errorTris[ti] = true
		}
	}
	for i := 0; i+1 < len(indices); i++ {
		vi, vj := indices[i], indices[i+1]
		sharedTri := -1
		for _, ti := range incident[vi] {
			for _, k := range mesh.Triangles[ti].Vertices {
				if k == vj {
					sharedTri = ti
					break
				}
			}
			if sharedTri >= 0 {
				break
			}
		}
		if sharedTri < 0 {
			continue
		}
		tri := mesh.Triangles[sharedTri]
		normal := worldNormal(mesh.Vertices[tri.Vertices[0]], mesh.Vertices[tri.Vertices[1]], mesh.Vertices[tri.Vertices[2]])
		if normal[1] < 0 {
			normal = [3]float64{-normal[0], -normal[1], -normal[2]}
		}
		out.SegmentNormals = append(out.SegmentNormals, normal)
	}
	for ti := range errorTris {
		out.MaxHeightError = math.Max(out.MaxHeightError, errorByTri[ti])
	}
	return out
}

func worldNormal(a, b, c Vertex) [3]float64 {
	ux, uy, uz := b.X-a.X, b.Y-a.Y, b.Z-a.Z
	vx, vy, vz := c.X-a.X, c.Y-a.Y, c.Z-a.Z
	nx := uy*vz - uz*vy
	ny := uz*vx - ux*vz
	nz := ux*vy - uy*vx
	l := math.Hypot(nx, math.Hypot(ny, nz))
	if l < 1e-12 {
		return [3]float64{}
	}
	return [3]float64{nx / l, ny / l, nz / l}
}

func inwardTangent(mesh *Mesh, block Block, side string, vertexIndex int, tris []int) [3]float64 {
	v := mesh.Vertices[vertexIndex]
	gxDir, gyDir := inwardGridDirection(side)
	inward := [2]float64{float64(gxDir) * block.Grid.CellSize, float64(gyDir) * block.Grid.CellSize}
	bestDot := math.Inf(-1)
	var best [3]float64
	found := false
	for _, ti := range tris {
		tri := mesh.Triangles[ti]
		for _, other := range tri.Vertices {
			if other == vertexIndex {
				continue
			}
			q := mesh.Vertices[other]
			vec := [3]float64{q.X - v.X, q.Y - v.Y, q.Z - v.Z}
			horizontal := math.Hypot(vec[0], vec[2])
			if horizontal < 1e-9 {
				continue
			}
			d := (vec[0]*inward[0] + vec[2]*inward[1]) / horizontal
			if d > bestDot {
				bestDot = d
				best = vec
				found = true
			}
		}
	}
	if !found {
		return [3]float64{inward[0], 0, inward[1]}
	}
	return best
}

func BuildBoundaryReport(b BoundarySharedView, left, right BoundarySide, limit, toleranceA, toleranceB, alignmentBuffer float64) *BoundaryReport {
	fc := FaultCase{
		BoundaryID: b.ID, BlockA: b.BlockA, BlockB: b.BlockB,
		Left: left, Right: right, FoldLimitDeg: limit,
		HeightToleranceA: toleranceA, HeightToleranceB: toleranceB, AlignmentBuffer: alignmentBuffer,
	}
	attribution, err := AttributeBoundary(fc)
	rep := &BoundaryReport{AttributionReport: *attribution, VertexCountA: len(left.Points), VertexCountB: len(right.Points)}
	if err != nil {
		rep.ErrorCode = CodeAlign
	}
	return rep
}

type BoundarySharedView struct {
	ID     string
	BlockA int
	BlockB int
}
