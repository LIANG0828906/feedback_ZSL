package terrain

import (
	"fmt"
	"math"
	"sort"
)

type dVertex struct {
	gx, gy int
	x, z   float64
	y      float64
	super  bool
}

type BuildOptions struct {
	ParameterVersion string
	ForcedPoints     []GridPoint
}

func BuildBlockMesh(block Block, opts BuildOptions) (*Mesh, error) {
	if block.Grid.Width < 2 || block.Grid.Height < 2 {
		bn := block.BlockNo
		return nil, NewSchemaError(fmt.Sprintf("blocks[%d].grid", block.BlockNo), &bn, "grid must be at least 2x2")
	}
	paramVer := opts.ParameterVersion
	if paramVer == "" {
		paramVer = DefaultParamVer
	}

	required := uniquePoints(append(append([]GridPoint{}, block.RequiredPoints...), opts.ForcedPoints...))
	required = append(required, GridPoint{0, 0}, GridPoint{block.Grid.Width - 1, 0}, GridPoint{0, block.Grid.Height - 1}, GridPoint{block.Grid.Width - 1, block.Grid.Height - 1})
	required = uniquePoints(required)
	sortPoints(required)

	verts, tris, idByPoint := newDelaunay(block)
	for _, p := range required {
		insertActualPoint(block, p, &verts, &tris, idByPoint)
	}

	selected := map[GridPoint]bool{}
	for _, p := range required {
		selected[p] = true
	}
	for len(selected) < block.SampleBudget {
		bestP, bestErr, found := worstCandidate(block, verts, tris, selected)
		if !found || bestErr <= block.ErrorTolerance {
			break
		}
		insertActualPoint(block, bestP, &verts, &tris, idByPoint)
		selected[bestP] = true
	}

	return outputMesh(block, verts, tris, paramVer)
}

func uniquePoints(points []GridPoint) []GridPoint {
	seen := map[GridPoint]bool{}
	out := make([]GridPoint, 0, len(points))
	for _, p := range points {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

func sortPoints(points []GridPoint) {
	sort.Slice(points, func(i, j int) bool {
		if points[i].Y != points[j].Y {
			return points[i].Y < points[j].Y
		}
		return points[i].X < points[j].X
	})
}

func newDelaunay(block Block) ([]dVertex, [][3]int, map[GridPoint]int) {
	w, h := block.Grid.Width, block.Grid.Height
	maxDim := float64(max(w, h))
	midX := float64(w-1) / 2
	midZ := float64(h-1) / 2
	superSize := 20*maxDim + 10
	verts := []dVertex{
		{gx: -1, gy: -1, x: midX - superSize, z: -maxDim - 10, super: true},
		{gx: -1, gy: -1, x: midX, z: midZ + superSize, super: true},
		{gx: -1, gy: -1, x: midX + superSize, z: -maxDim - 10, super: true},
	}
	t := [3]int{0, 1, 2}
	if orientD(verts[t[0]], verts[t[1]], verts[t[2]]) < 0 {
		t[1], t[2] = t[2], t[1]
	}
	idByPoint := map[GridPoint]int{}
	return verts, [][3]int{t}, idByPoint
}

func insertActualPoint(block Block, p GridPoint, verts *[]dVertex, tris *[][3]int, idByPoint map[GridPoint]int) int {
	if id, ok := idByPoint[p]; ok {
		return id
	}
	id := len(*verts)
	*verts = append(*verts, dVertex{
		gx: p.X, gy: p.Y,
		x: float64(p.X), z: float64(p.Y),
		y: gridHeight(block.Grid, p.X, p.Y),
	})
	idByPoint[p] = id
	*tris = bowyerInsert(*verts, *tris, id)
	return id
}

func bowyerInsert(verts []dVertex, tris [][3]int, pID int) [][3]int {
	bad := map[int]bool{}
	for i, tri := range tris {
		if inCircumcircle(verts[tri[0]], verts[tri[1]], verts[tri[2]], verts[pID]) {
			bad[i] = true
		}
	}
	edgeCount := map[[2]int]int{}
	for ti := range bad {
		for e := 0; e < 3; e++ {
			a := tris[ti][e]
			b := tris[ti][(e+1)%3]
			key := [2]int{min(a, b), max(a, b)}
			edgeCount[key]++
		}
	}
	out := make([][3]int, 0, len(tris)-len(bad)+len(edgeCount))
	for i, tri := range tris {
		if !bad[i] {
			out = append(out, tri)
		}
	}
	for edge, count := range edgeCount {
		if count != 1 {
			continue
		}
		tri := [3]int{edge[0], edge[1], pID}
		if orientD(verts[tri[0]], verts[tri[1]], verts[tri[2]]) < 0 {
			tri[1], tri[2] = tri[2], tri[1]
		}
		if math.Abs(orientD(verts[tri[0]], verts[tri[1]], verts[tri[2]])) > 1e-10 {
			out = append(out, tri)
		}
	}
	return out
}

func orientD(a, b, c dVertex) float64 {
	return (b.x-a.x)*(c.z-a.z) - (b.z-a.z)*(c.x-a.x)
}

func inCircumcircle(a, b, c, p dVertex) bool {
	if orientD(a, b, c) < 0 {
		b, c = c, b
	}
	ax, az := a.x-p.x, a.z-p.z
	bx, bz := b.x-p.x, b.z-p.z
	cx, cz := c.x-p.x, c.z-p.z
	det := (ax*ax+az*az)*(bx*cz-bz*cx) -
		(bx*bx+bz*bz)*(ax*cz-az*cx) +
		(cx*cx+cz*cz)*(ax*bz-az*bx)
	return det >= -1e-7
}

func worstCandidate(block Block, verts []dVertex, tris [][3]int, selected map[GridPoint]bool) (GridPoint, float64, bool) {
	bestP := GridPoint{}
	bestErr := -1.0
	found := false
	for y := 0; y < block.Grid.Height; y++ {
		for x := 0; x < block.Grid.Width; x++ {
			p := GridPoint{X: x, Y: y}
			if selected[p] || isBoundaryPoint(p, block.Grid) {
				continue
			}
			err, ok := interpolationErrorAt(block, p, verts, tris)
			if !ok {
				continue
			}
			if err > bestErr+1e-12 || (math.Abs(err-bestErr) <= 1e-12 && (!found || y < bestP.Y || (y == bestP.Y && x < bestP.X))) {
				bestErr = err
				bestP = p
				found = true
			}
		}
	}
	return bestP, math.Max(0, bestErr), found
}

func isBoundaryPoint(p GridPoint, g Grid) bool {
	return p.X == 0 || p.Y == 0 || p.X == g.Width-1 || p.Y == g.Height-1
}

func interpolationErrorAt(block Block, p GridPoint, verts []dVertex, tris [][3]int) (float64, bool) {
	query := dVertex{x: float64(p.X), z: float64(p.Y)}
	for _, tri := range tris {
		if verts[tri[0]].super || verts[tri[1]].super || verts[tri[2]].super {
			continue
		}
		wa, wb, wc, ok := barycentric(query, verts[tri[0]], verts[tri[1]], verts[tri[2]])
		if !ok {
			continue
		}
		interp := wa*verts[tri[0]].y + wb*verts[tri[1]].y + wc*verts[tri[2]].y
		return math.Abs(gridHeight(block.Grid, p.X, p.Y) - interp), true
	}
	return 0, false
}

func barycentric(p, a, b, c dVertex) (float64, float64, float64, bool) {
	den := (b.z-c.z)*(a.x-c.x) + (c.x-b.x)*(a.z-c.z)
	if math.Abs(den) < 1e-12 {
		return 0, 0, 0, false
	}
	wa := ((b.z-c.z)*(p.x-c.x) + (c.x-b.x)*(p.z-c.z)) / den
	wb := ((c.z-a.z)*(p.x-c.x) + (a.x-c.x)*(p.z-c.z)) / den
	wc := 1 - wa - wb
	const eps = 1e-8
	if wa < -eps || wb < -eps || wc < -eps {
		return 0, 0, 0, false
	}
	return wa, wb, wc, true
}

func outputMesh(block Block, verts []dVertex, tris [][3]int, paramVer string) (*Mesh, error) {
	usedOld := map[int]bool{}
	realTris := make([][3]int, 0, len(tris))
	for _, tri := range tris {
		if verts[tri[0]].super || verts[tri[1]].super || verts[tri[2]].super {
			continue
		}
		realTris = append(realTris, tri)
		for _, id := range tri {
			usedOld[id] = true
		}
	}

	oldIDs := make([]int, 0, len(usedOld))
	for id := range usedOld {
		oldIDs = append(oldIDs, id)
	}
	sort.Slice(oldIDs, func(i, j int) bool {
		a, b := verts[oldIDs[i]], verts[oldIDs[j]]
		if a.gy != b.gy {
			return a.gy < b.gy
		}
		return a.gx < b.gx
	})

	newIndex := map[int]int{}
	outVerts := make([]Vertex, 0, len(oldIDs))
	for newIdx, oldID := range oldIDs {
		dv := verts[oldID]
		newIndex[oldID] = newIdx
		outVerts = append(outVerts, Vertex{
			Index: newIdx, GridX: dv.gx, GridY: dv.gy,
			X: block.OriginX + dv.x*block.Grid.CellSize,
			Y: dv.y,
			Z: block.OriginZ + dv.z*block.Grid.CellSize,
		})
	}

	outTris := make([]Triangle, 0, len(realTris))
	for _, oldTri := range realTris {
		t := [3]int{newIndex[oldTri[0]], newIndex[oldTri[1]], newIndex[oldTri[2]]}
		t = rotateSmallestFirst(t)
		a, b, c := outVerts[t[0]], outVerts[t[1]], outVerts[t[2]]
		signed := (b.X-a.X)*(c.Z-a.Z) - (b.Z-a.Z)*(c.X-a.X)
		if signed < 0 {
			t[1], t[2] = t[2], t[1]
		}
		outTris = append(outTris, Triangle{Vertices: t})
	}
	sort.Slice(outTris, func(i, j int) bool {
		for k := 0; k < 3; k++ {
			if outTris[i].Vertices[k] != outTris[j].Vertices[k] {
				return outTris[i].Vertices[k] < outTris[j].Vertices[k]
			}
		}
		return false
	})
	for i := range outTris {
		outTris[i].Index = i
	}

	mesh := &Mesh{
		BlockNo: block.BlockNo, AlgorithmVer: AlgorithmVersion, ParameterVer: paramVer,
		Vertices: outVerts, Triangles: outTris, Budget: block.SampleBudget, Tolerance: block.ErrorTolerance,
	}
	errorsOut, maxErr := estimateTriangleErrors(block, mesh)
	mesh.TriangleErrors = errorsOut
	mesh.MaxHeightError = maxErr
	return mesh, nil
}

func rotateSmallestFirst(t [3]int) [3]int {
	minIdx := 0
	for i := 1; i < 3; i++ {
		if t[i] < t[minIdx] {
			minIdx = i
		}
	}
	return [3]int{t[minIdx], t[(minIdx+1)%3], t[(minIdx+2)%3]}
}

func estimateTriangleErrors(block Block, mesh *Mesh) ([]TriangleError, float64) {
	out := make([]TriangleError, len(mesh.Triangles))
	maxAll := 0.0
	for ti, tri := range mesh.Triangles {
		out[ti].TriangleIndex = ti
		a := mesh.Vertices[tri.Vertices[0]]
		b := mesh.Vertices[tri.Vertices[1]]
		c := mesh.Vertices[tri.Vertices[2]]
		da := dVertex{x: float64(a.GridX), z: float64(a.GridY), y: a.Y}
		db := dVertex{x: float64(b.GridX), z: float64(b.GridY), y: b.Y}
		dc := dVertex{x: float64(c.GridX), z: float64(c.GridY), y: c.Y}
		best := 0.0
		bestP := GridPoint{}
		sampled := 0
		for y := 0; y < block.Grid.Height; y++ {
			for x := 0; x < block.Grid.Width; x++ {
				p := dVertex{x: float64(x), z: float64(y)}
				wa, wb, wc, ok := barycentric(p, da, db, dc)
				if !ok {
					continue
				}
				sampled++
				interp := wa*da.y + wb*db.y + wc*dc.y
				err := math.Abs(gridHeight(block.Grid, x, y) - interp)
				if err > best {
					best = err
					bestP = GridPoint{X: x, Y: y}
				}
			}
		}
		out[ti].MaxAbsError = round9(best)
		out[ti].At = bestP
		out[ti].SampledPoints = sampled
		maxAll = math.Max(maxAll, best)
	}
	return out, round9(maxAll)
}
