package repair

import (
	"fmt"
	"math"
	"sort"

	"meshforge/internal/errs"
	"meshforge/internal/mesh"
)

type FillOptions struct {
	PreferConvexFan bool
}

type LoopFill struct {
	ID               string  `json:"id"`
	Vertices         int     `json:"vertices"`
	Convex           bool    `json:"convex"`
	Method           string  `json:"method"`
	NormalContinuity float64 `json:"normal_continuity"`
	AddedFaces       int     `json:"added_faces"`
	AddedArea        float64 `json:"added_area"`
}

type FillReport struct {
	BoundaryLoopsInput int        `json:"boundary_loops_input"`
	LoopsFilled        int        `json:"loops_filled"`
	AddedFaces         int        `json:"added_faces"`
	AddedArea          float64    `json:"added_area"`
	OpenChains         int        `json:"open_chains"`
	LoopDetails        []LoopFill `json:"loop_details"`
	ClosedAfter        bool       `json:"closed_after"`
}

func FillHoles(in mesh.Mesh, opt FillOptions) (mesh.Mesh, FillReport, *errs.CodedError) {
	analysis := mesh.BoundaryLoops(in)
	report := FillReport{
		BoundaryLoopsInput: len(analysis.Loops),
		OpenChains:         analysis.OpenChains,
	}
	if analysis.OpenChains > 0 {
		e := errs.New(errs.CodeTolo,
			"cannot fill %d open boundary chain(s); weld/canonicalization left degenerate boundaries",
			analysis.OpenChains)
		e.SuggestedTolerance = suggestTolerance(in) * 0.5
		return mesh.Mesh{}, report, e
	}
	if len(analysis.Loops) == 0 {
		report.ClosedAfter = true
		return in.Clone(), report, nil
	}

	topoBefore := mesh.BuildTopology(in)
	nmBefore := 0
	for _, e := range topoBefore.Edges {
		if len(e.Uses) > 2 {
			nmBefore++
		}
	}
	out := in.Clone()
	for li, loop := range analysis.Loops {
		tris, detail, err := triangulateLoop(out, loop, opt)
		if err != nil {
			err.Stage = "fill"
			return mesh.Mesh{}, report, err
		}
		detail.ID = fmt.Sprintf("L%d", li+1)
		baseIndex := len(out.Faces)
		for _, t := range tris {
			if t.A == t.B || t.B == t.C || t.C == t.A {
				e := errs.New(errs.CodeTolo,
					"hole %s triangulation produced a repeated-index face", detail.ID)
				e.SuggestedTolerance = suggestTolerance(in) * 0.5
				return mesh.Mesh{}, report, errs.WithFace(e, baseIndex)
			}
			out.Faces = append(out.Faces, t)
		}
		for i := 0; i < len(tris); i++ {
			detail.AddedArea += out.FaceArea(baseIndex + i)
		}
		detail.AddedFaces = len(tris)
		report.AddedFaces += len(tris)
		report.AddedArea += detail.AddedArea
		report.LoopDetails = append(report.LoopDetails, detail)
		report.LoopsFilled++
	}

	topoAfter := mesh.BuildTopology(out)
	nmAfter := 0
	for _, e := range topoAfter.SortedEdges() {
		if len(e.Uses) > 2 {
			nmAfter++
			e2 := errs.New(errs.CodeTolo,
				"hole filling introduced non-manifold edge (%d,%d)", e.A, e.B)
			return mesh.Mesh{}, report, e2
		}
	}
	if nmAfter > nmBefore {
		return mesh.Mesh{}, report, errs.New(errs.CodeTolo,
			"hole filling increased non-manifold edges from %d to %d", nmBefore, nmAfter)
	}
	afterBoundary := mesh.BoundaryLoops(out)
	if len(afterBoundary.Loops) != 0 || afterBoundary.OpenChains != 0 {
		e := errs.New(errs.CodeTolo,
			"filled region is not closed: %d loops, %d chains remain",
			len(afterBoundary.Loops), afterBoundary.OpenChains)
		e.SuggestedTolerance = suggestTolerance(in) * 0.5
		return mesh.Mesh{}, report, e
	}
	report.ClosedAfter = true
	return out, report, nil
}

type vec2 struct{ x, y float64 }

func triangulateLoop(m mesh.Mesh, loop mesh.BoundaryLoop, opt FillOptions) ([]mesh.Face, LoopFill, *errs.CodedError) {
	detail := LoopFill{Vertices: len(loop.Vertices)}
	if len(loop.Vertices) < 3 {
		e := errs.New(errs.CodeTolo, "boundary loop has fewer than 3 vertices")
		e.SuggestedTolerance = suggestTolerance(m) * 0.5
		return nil, detail, e
	}
	ordered := make([]int, len(loop.Vertices))
	for i := range loop.Vertices {
		ordered[i] = loop.Vertices[len(loop.Vertices)-1-i]
	}
	pts := make([]mesh.Vec3, len(ordered))
	for i, vi := range ordered {
		pts[i] = m.Vertices[vi]
	}
	normal := newellNormal(pts).Normalize()
	proj, _ := projectPoints(pts, normal)
	convex := isConvex(proj)
	detail.Convex = convex

	var tris []mesh.Face
	if len(ordered) == 3 {
		tris = []mesh.Face{{A: ordered[0], B: ordered[1], C: ordered[2]}}
		detail.Method = "single-triangle"
	} else if convex && opt.PreferConvexFan {
		for i := 1; i < len(ordered)-1; i++ {
			tris = append(tris, mesh.Face{A: ordered[0], B: ordered[i], C: ordered[i+1]})
		}
		detail.Method = "convex-fan"
	} else {
		tris = earClip(ordered, proj)
		detail.Method = "ear-clipping"
		if convex {
			detail.Method = "ear-clipping-convex"
		}
	}
	detail.NormalContinuity = normalContinuity(m, loop, ordered, tris)
	return tris, detail, nil
}

func newellNormal(pts []mesh.Vec3) mesh.Vec3 {
	var n mesh.Vec3
	for i := range pts {
		a, b := pts[i], pts[(i+1)%len(pts)]
		n.X += (a.Y - b.Y) * (a.Z + b.Z)
		n.Y += (a.Z - b.Z) * (a.X + b.X)
		n.Z += (a.X - b.X) * (a.Y + b.Y)
	}
	return n
}

func projectPoints(pts []mesh.Vec3, normal mesh.Vec3) ([]vec2, int) {
	ax, ay := mesh.Vec3{X: 1}, mesh.Vec3{Y: 1}
	drop := 0
	n := normal
	axn, ayn, azn := math.Abs(n.X), math.Abs(n.Y), math.Abs(n.Z)
	if axn >= ayn && axn >= azn {
		ax, ay = mesh.Vec3{Y: 1}, mesh.Vec3{Z: 1}
		drop = 0
	} else if ayn >= axn && ayn >= azn {
		ax, ay = mesh.Vec3{X: 1}, mesh.Vec3{Z: 1}
		drop = 1
	} else {
		ax, ay = mesh.Vec3{X: 1}, mesh.Vec3{Y: 1}
		drop = 2
	}
	out := make([]vec2, len(pts))
	for i, p := range pts {
		out[i] = vec2{x: p.Dot(ax), y: p.Dot(ay)}
	}
	return out, drop
}

func signedArea2(p []vec2) float64 {
	var a float64
	for i := range p {
		j := (i + 1) % len(p)
		a += p[i].x*p[j].y - p[j].x*p[i].y
	}
	return a * 0.5
}

func isConvex(p []vec2) bool {
	if len(p) < 3 {
		return false
	}
	sign := 1.0
	if signedArea2(p) < 0 {
		sign = -1
	}
	for i := range p {
		a, b, c := p[i], p[(i+1)%len(p)], p[(i+2)%len(p)]
		if cross2(b.x-a.x, b.y-a.y, c.x-b.x, c.y-b.y)*sign <= 1e-14 {
			return false
		}
	}
	return true
}

func cross2(ax, ay, bx, by float64) float64 { return ax*by - ay*bx }

func earClip(indices []int, p []vec2) []mesh.Face {
	n := len(indices)
	if n < 3 {
		return nil
	}
	ccw := signedArea2(p) >= 0
	sign := 1.0
	if !ccw {
		sign = -1
	}
	remaining := make([]int, n)
	copy(remaining, indices)
	pts := append([]vec2(nil), p...)
	var out []mesh.Face
	guard := 0
	for len(remaining) > 3 && guard < n*n*2 {
		guard++
		found := false
		for i := 0; i < len(remaining); i++ {
			ia := (i + len(remaining) - 1) % len(remaining)
			ic := (i + 1) % len(remaining)
			a, b, c := pts[ia], pts[i], pts[ic]
			cr := cross2(b.x-a.x, b.y-a.y, c.x-b.x, c.y-b.y)
			if cr*sign <= 1e-14 {
				continue
			}
			contains := false
			for j := range remaining {
				if j == ia || j == i || j == ic {
					continue
				}
				if pointInTriangle(pts[j], a, b, c, sign) {
					contains = true
					break
				}
			}
			if contains {
				continue
			}
			out = append(out, mesh.Face{A: remaining[ia], B: remaining[i], C: remaining[ic]})
			remaining = append(remaining[:i], remaining[i+1:]...)
			pts = append(pts[:i], pts[i+1:]...)
			found = true
			break
		}
		if !found {
			break
		}
	}
	if len(remaining) == 3 {
		out = append(out, mesh.Face{A: remaining[0], B: remaining[1], C: remaining[2]})
		return out
	}
	// Deterministic fallback for numerically hostile polygons. Orientation
	// repair and closure checks still validate the result.
	for i := 1; i < len(indices)-1; i++ {
		out = append(out, mesh.Face{A: indices[0], B: indices[i], C: indices[i+1]})
	}
	return out
}

func pointInTriangle(p, a, b, c vec2, sign float64) bool {
	c1 := cross2(b.x-a.x, b.y-a.y, p.x-a.x, p.y-a.y) * sign
	c2 := cross2(c.x-b.x, c.y-b.y, p.x-b.x, p.y-b.y) * sign
	c3 := cross2(a.x-c.x, a.y-c.y, p.x-c.x, p.y-c.y) * sign
	return c1 >= -1e-14 && c2 >= -1e-14 && c3 >= -1e-14
}

func normalContinuity(m mesh.Mesh, loop mesh.BoundaryLoop, ordered []int, tris []mesh.Face) float64 {
	if len(tris) == 0 {
		return 0
	}
	fillNormal := mesh.Vec3{}
	for _, t := range tris {
		a, b, c := m.Vertices[t.A], m.Vertices[t.B], m.Vertices[t.C]
		fillNormal = fillNormal.Add(b.Sub(a).Cross(c.Sub(a)))
	}
	fillNormal = fillNormal.Normalize()
	topo := mesh.BuildTopology(m)
	adj := mesh.Vec3{}
	count := 0
	loopSet := make(map[int]bool, len(loop.Vertices))
	for _, vi := range loop.Vertices {
		loopSet[vi] = true
	}
	for _, e := range topo.SortedEdges() {
		if len(e.Uses) != 1 || !loopSet[e.A] || !loopSet[e.B] {
			continue
		}
		adj = adj.Add(m.FaceNormal(e.Uses[0].Face))
		count++
	}
	if count == 0 {
		return 0
	}
	adj = adj.Normalize()
	return fillNormal.Dot(adj)
}

func sortLoopDetails(a []LoopFill) {
	sort.Slice(a, func(i, j int) bool { return a[i].ID < a[j].ID })
}
