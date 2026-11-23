package spatialpick

import (
	"math"
	"sort"
)

type Ray struct {
	Origin    Vec3
	Direction Vec3
}

func (c Camera) PixelScale() (float64, bool) {
	if !finite(c.WorldHeight) || c.WorldHeight <= 0 || !finite(c.ViewportHeight) || c.ViewportHeight <= 0 {
		return 0, false
	}
	return c.ViewportHeight / c.WorldHeight, true
}

func (c Camera) ScreenRay(screen Vec2) (Ray, bool) {
	ppw, ok := c.PixelScale()
	if !ok || !finite(screen.X) || !finite(screen.Y) ||
		screen.X < 0 || screen.X > c.ViewportWidth ||
		screen.Y < 0 || screen.Y > c.ViewportHeight {
		return Ray{}, false
	}
	wx := (screen.X - c.ViewportWidth/2) / ppw
	wy := (c.ViewportHeight/2 - screen.Y) / ppw
	origin := c.Eye.Add(c.Right.Mul(wx)).Add(c.Up.Mul(wy))
	dir, ok := c.Forward.Normalize()
	if !ok {
		return Ray{}, false
	}
	return Ray{Origin: origin, Direction: dir}, true
}

func (c Camera) Project(p Vec3) (Vec2, bool) {
	ppw, ok := c.PixelScale()
	if !ok || !p.IsFinite() || !c.Eye.IsFinite() {
		return Vec2{}, false
	}
	d := p.Sub(c.Eye)
	return Vec2{
		X: c.ViewportWidth/2 + d.Dot(c.Right)*ppw,
		Y: c.ViewportHeight/2 - d.Dot(c.Up)*ppw,
	}, true
}

func (idx *Index) PickRay(req RayQuery, camera Camera) (RayPickResult, error) {
	res := RayPickResult{QueryID: req.ID}
	ray, ok := camera.ScreenRay(req.Screen)
	if !ok {
		return res, &CodedError{Code: CodeParameter, Op: "pick_ray", QueryID: req.ID, Field: "screen", Message: "screen coordinate is outside the camera viewport or camera is invalid"}
	}
	candidates := idx.rayCandidates(ray)
	var first *PickHit
	for _, ei := range candidates {
		e := &idx.Entries[ei]
		if e.Failed {
			return res, &CodedError{Code: CodeStale, Op: "pick_ray", QueryID: req.ID, NodeID: e.NodeID, GeometryID: e.GeometryID, ExpectedVersion: e.Version, HasExpectedVersion: true, Message: "index entry is marked failed/stale"}
		}
		hit, t, point, ambigReasons, bound := intersectIndexEntry(e, ray)
		res.ErrorBound = math.Max(res.ErrorBound, bound)
		gh := GeometryHit{NodeID: e.NodeID, GeometryID: e.GeometryID, Hit: hit, Bound: bound}
		if hit {
			gh.T, gh.Point = t, point
			candidate := PickHit{NodeID: e.NodeID, GeometryID: e.GeometryID, T: t, Point: point, Depth: e.Depth}
			if first == nil || candidate.T < first.T || (candidate.T == first.T && candidate.NodeID < first.NodeID) ||
				(candidate.T == first.T && candidate.NodeID == first.NodeID && candidate.GeometryID < first.GeometryID) {
				c := candidate
				first = &c
			}
		}
		res.Geometries = append(res.Geometries, gh)
		for _, reason := range ambigReasons {
			res.Ambiguities = append(res.Ambiguities, Ambiguity{
				QueryID: req.ID, NodeID: e.NodeID, GeometryID: e.GeometryID,
				Depth: e.Depth, Reason: reason, Bound: bound,
			})
		}
	}
	res.First = first
	sort.Slice(res.Geometries, func(i, j int) bool {
		if res.Geometries[i].NodeID != res.Geometries[j].NodeID {
			return res.Geometries[i].NodeID < res.Geometries[j].NodeID
		}
		return res.Geometries[i].GeometryID < res.Geometries[j].GeometryID
	})
	sortAmbiguities(res.Ambiguities)
	if len(res.Ambiguities) > 0 {
		return res, ambiguityError("pick_ray", req.ID, res.Ambiguities)
	}
	return res, nil
}

func (idx *Index) SelectBox(req BoxQuery, camera Camera) (BoxSelectResult, error) {
	res := BoxSelectResult{QueryID: req.ID}
	if !finite(req.Rect.X0) || !finite(req.Rect.Y0) || !finite(req.Rect.X1) || !finite(req.Rect.Y1) ||
		req.Rect.X0 < 0 || req.Rect.X1 < 0 || req.Rect.Y0 < 0 || req.Rect.Y1 < 0 ||
		req.Rect.X0 > camera.ViewportWidth || req.Rect.X1 > camera.ViewportWidth ||
		req.Rect.Y0 > camera.ViewportHeight || req.Rect.Y1 > camera.ViewportHeight {
		return res, &CodedError{Code: CodeParameter, Op: "select_box", QueryID: req.ID, Field: "rect", Message: "selection rectangle is outside the camera viewport"}
	}
	rect := req.Rect.Normalized()
	ppw, ok := camera.PixelScale()
	if !ok {
		return res, &CodedError{Code: CodeParameter, Op: "select_box", QueryID: req.ID, Field: "camera", Message: "invalid camera"}
	}
	screenEps := idx.MaxErrorBound*ppw + math.Pow10(-9)*ppw
	if screenEps < 1e-7 {
		screenEps = 1e-7
	}
	res.ErrorBound = idx.MaxErrorBound
	candidates := idx.boxCandidates(rect, camera, screenEps)
	hitSet := make(map[string]struct{})
	for _, ei := range candidates {
		e := &idx.Entries[ei]
		if e.Failed {
			return res, &CodedError{Code: CodeStale, Op: "select_box", QueryID: req.ID, NodeID: e.NodeID, GeometryID: e.GeometryID, ExpectedVersion: e.Version, HasExpectedVersion: true, Message: "index entry is marked failed/stale"}
		}
		b := projectedAABB(e.AABB, camera)
		hit, ambig, reason, overlap := projectedOverlap(b, rect, screenEps)
		if hit {
			hitSet[e.NodeID] = struct{}{}
		}
		if ambig {
			res.Ambiguities = append(res.Ambiguities, Ambiguity{
				QueryID: req.ID, NodeID: e.NodeID, GeometryID: e.GeometryID,
				Depth: e.Depth, Reason: reason, Bound: overlap,
			})
		}
	}
	res.Hits = sortedStringsFromSet(hitSet)
	sortAmbiguities(res.Ambiguities)
	if len(res.Ambiguities) > 0 {
		return res, ambiguityError("select_box", req.ID, res.Ambiguities)
	}
	return res, nil
}

func (idx *Index) ContainsPoint(req PointQuery, camera Camera) (PointSelectResult, error) {
	res := PointSelectResult{QueryID: req.ID}
	if !req.Point.IsFinite() {
		return res, &CodedError{Code: CodeParameter, Op: "contains_point", QueryID: req.ID, Field: "point", Message: "point must be finite"}
	}
	res.ErrorBound = idx.MaxErrorBound
	candidates := idx.pointCandidates(req.Point)
	hitSet := make(map[string]struct{})
	for _, ei := range candidates {
		e := &idx.Entries[ei]
		if e.Failed {
			return res, &CodedError{Code: CodeStale, Op: "contains_point", QueryID: req.ID, NodeID: e.NodeID, GeometryID: e.GeometryID, ExpectedVersion: e.Version, HasExpectedVersion: true, Message: "index entry is marked failed/stale"}
		}
		inside, ambig, reason, margin := pointInEntry(e, req.Point)
		if inside {
			hitSet[e.NodeID] = struct{}{}
		}
		if ambig {
			res.Ambiguities = append(res.Ambiguities, Ambiguity{
				QueryID: req.ID, NodeID: e.NodeID, GeometryID: e.GeometryID,
				Depth: e.Depth, Reason: reason, Bound: margin,
			})
		}
	}
	res.Hits = sortedStringsFromSet(hitSet)
	sortAmbiguities(res.Ambiguities)
	if len(res.Ambiguities) > 0 {
		return res, ambiguityError("contains_point", req.ID, res.Ambiguities)
	}
	return res, nil
}

func ambiguityError(op, queryID string, ambiguities []Ambiguity) *CodedError {
	e := &CodedError{Code: CodeAmbiguous, Op: op, QueryID: queryID, Ambiguities: ambiguities, Message: "decision lies inside the declared fixed-point error bound"}
	if len(ambiguities) > 0 {
		e.NodeID = ambiguities[0].NodeID
		e.GeometryID = ambiguities[0].GeometryID
		e.Depth = ambiguities[0].Depth
	}
	return e
}

func sortAmbiguities(a []Ambiguity) {
	sort.Slice(a, func(i, j int) bool {
		if a[i].NodeID != a[j].NodeID {
			return a[i].NodeID < a[j].NodeID
		}
		if a[i].GeometryID != a[j].GeometryID {
			return a[i].GeometryID < a[j].GeometryID
		}
		return a[i].Reason < a[j].Reason
	})
}

func (idx *Index) rayCandidates(ray Ray) []int {
	var out []int
	pad := idx.MaxErrorBound + 1e-9
	var walk func(*BVHNode)
	walk = func(n *BVHNode) {
		if n == nil || !rayAABBBool(ray, n.Bounds.Expanded(pad)) {
			return
		}
		if n.Leaf {
			out = append(out, n.Entry)
			return
		}
		walk(n.Left)
		walk(n.Right)
	}
	walk(idx.Root)
	sort.Ints(out)
	return out
}

func (idx *Index) boxCandidates(rect Rect, camera Camera, eps float64) []int {
	var out []int
	var walk func(*BVHNode)
	walk = func(n *BVHNode) {
		if n == nil {
			return
		}
		b := projectedAABB(n.Bounds, camera)
		if b.X1 < rect.X0-eps || b.X0 > rect.X1+eps || b.Y1 < rect.Y0-eps || b.Y0 > rect.Y1+eps {
			return
		}
		if n.Leaf {
			out = append(out, n.Entry)
			return
		}
		walk(n.Left)
		walk(n.Right)
	}
	walk(idx.Root)
	sort.Ints(out)
	return out
}

func (idx *Index) pointCandidates(p Vec3) []int {
	var out []int
	pad := idx.MaxErrorBound + 1e-9
	var walk func(*BVHNode)
	walk = func(n *BVHNode) {
		if n == nil {
			return
		}
		q := AABB{Min: p, Max: p}
		if !n.Bounds.Expanded(pad).Overlaps(q) {
			return
		}
		if n.Leaf {
			out = append(out, n.Entry)
			return
		}
		walk(n.Left)
		walk(n.Right)
	}
	walk(idx.Root)
	sort.Ints(out)
	return out
}

func rayAABBBool(ray Ray, b AABB) bool {
	tmin, tmax := math.Inf(-1), math.Inf(1)
	for axis := 0; axis < 3; axis++ {
		o := ray.Origin.Component(axis)
		d := ray.Direction.Component(axis)
		lo := b.Min.Component(axis)
		hi := b.Max.Component(axis)
		if math.Abs(d) < 1e-15 {
			if o < lo || o > hi {
				return false
			}
			continue
		}
		t1, t2 := (lo-o)/d, (hi-o)/d
		if t1 > t2 {
			t1, t2 = t2, t1
		}
		tmin = math.Max(tmin, t1)
		tmax = math.Min(tmax, t2)
		if tmin > tmax {
			return false
		}
	}
	return tmax >= 0
}

func intersectIndexEntry(e *IndexEntry, ray Ray) (bool, float64, Vec3, []string, float64) {
	bound := e.GeometryError + e.Quantum*float64(e.Depth+1)
	bound = math.Max(bound, e.GeometryError)
	o := e.WorldInvFloat.TransformPoint(ray.Origin)
	d := e.WorldInvFloat.TransformVector(ray.Direction)
	switch e.Geometry.Kind {
	case GeometryAABB:
		hit, t, ambig := intersectAABB(o, d, e.Geometry.Min, e.Geometry.Max, ray, e, bound)
		return hit, t, ray.Origin.Add(ray.Direction.Mul(t)), ambig, bound
	case GeometrySphere:
		hit, t, ambig := intersectSphere(o, d, e.Geometry.Center, e.Geometry.Radius, ray, e, bound)
		return hit, t, ray.Origin.Add(ray.Direction.Mul(t)), ambig, bound
	case GeometryMesh:
		hit, t, ambig := intersectMesh(o, d, e.Geometry, ray, e, bound)
		return hit, t, ray.Origin.Add(ray.Direction.Mul(t)), ambig, bound
	default:
		return false, 0, Vec3{}, nil, bound
	}
}

func intersectAABB(o, d, min, max Vec3, worldRay Ray, e *IndexEntry, bound float64) (bool, float64, []string) {
	tmin, tmax := math.Inf(-1), math.Inf(1)
	enterAxis := -1
	for axis := 0; axis < 3; axis++ {
		oa, da := o.Component(axis), d.Component(axis)
		lo, hi := min.Component(axis), max.Component(axis)
		if math.Abs(da) < 1e-15 {
			if oa < lo || oa > hi {
				return false, 0, nil
			}
			continue
		}
		t1, t2 := (lo-oa)/da, (hi-oa)/da
		if t1 > t2 {
			t1, t2 = t2, t1
		}
		if t1 > tmin {
			tmin, enterAxis = t1, axis
		}
		tmax = math.Min(tmax, t2)
		if tmin > tmax {
			return false, 0, nil
		}
	}
	if tmax < 0 {
		return false, 0, nil
	}
	_ = enterAxis
	t := math.Max(tmin, 0)
	var ambig []string
	dlen := worldRay.Direction.Length()
	inside := o.X >= min.X && o.X <= max.X && o.Y >= min.Y && o.Y <= max.Y && o.Z >= min.Z && o.Z <= max.Z
	if (tmax-tmin)*dlen <= bound {
		ambig = append(ambig, "grazing_aabb")
	}
	if !inside && t*dlen <= bound {
		ambig = append(ambig, "ray_starts_on_aabb_surface")
	}
	minScale := minWorldScale(e.WorldFloat)
	if dist := distanceToAABB(o, min, max); dist*minScale <= bound {
		ambig = append(ambig, "ray_origin_near_aabb_boundary")
	}
	return true, t, ambig
}

func intersectSphere(o, d, center Vec3, radius float64, worldRay Ray, e *IndexEntry, bound float64) (bool, float64, []string) {
	oc := o.Sub(center)
	a := d.Dot(d)
	if a < 1e-24 {
		return false, 0, nil
	}
	b := 2 * oc.Dot(d)
	c := oc.Dot(oc) - radius*radius
	disc := b*b - 4*a*c
	minScale := minWorldScale(e.WorldFloat)
	rayDistance := math.Sqrt(math.Max(0, oc.Dot(oc)-(oc.Dot(d)*oc.Dot(d))/a))
	if disc < 0 {
		if math.Abs(rayDistance-radius)*minScale <= bound {
			return false, 0, []string{"sphere_tangency_boundary"}
		}
		return false, 0, nil
	}
	sq := math.Sqrt(math.Max(0, disc))
	t0 := (-b - sq) / (2 * a)
	t1 := (-b + sq) / (2 * a)
	if t0 > t1 {
		t0, t1 = t1, t0
	}
	if t1 < 0 {
		return false, 0, nil
	}
	t := math.Max(t0, 0)
	var ambig []string
	dlen := worldRay.Direction.Length()
	if math.Abs(rayDistance-radius)*minScale <= bound {
		ambig = append(ambig, "sphere_tangency_boundary")
	}
	if (t1-t0)*dlen <= bound {
		ambig = append(ambig, "sphere_tangency_boundary")
	}
	originDist := math.Abs(o.Sub(center).Length()-radius) * minScale
	if originDist <= bound {
		ambig = append(ambig, "ray_origin_near_sphere_surface")
	}
	return true, t, dedupeStrings(ambig)
}

func intersectMesh(o, d Vec3, g Geometry, worldRay Ray, e *IndexEntry, bound float64) (bool, float64, []string) {
	bestT := math.Inf(1)
	var ambig []string
	minScale := minWorldScale(e.WorldFloat)
	localEps := bound / math.Max(minScale, 1e-12)
	for _, tri := range g.Triangles {
		if tri[0] < 0 || tri[0] >= len(g.Vertices) || tri[1] < 0 || tri[1] >= len(g.Vertices) || tri[2] < 0 || tri[2] >= len(g.Vertices) {
			continue
		}
		v0 := g.Vertices[tri[0]]
		edge1 := g.Vertices[tri[1]].Sub(v0)
		edge2 := g.Vertices[tri[2]].Sub(v0)
		pvec := d.Cross(edge2)
		det := edge1.Dot(pvec)
		if math.Abs(det) < 1e-18 {
			continue
		}
		invDet := 1 / det
		tvec := o.Sub(v0)
		u := tvec.Dot(pvec) * invDet
		if u < -localEps || u > 1+localEps {
			continue
		}
		qvec := tvec.Cross(edge1)
		v := d.Dot(qvec) * invDet
		if v < -localEps || u+v > 1+localEps {
			continue
		}
		t := edge2.Dot(qvec) * invDet
		if t < 0 {
			continue
		}
		if t < bestT {
			bestT = t
		}
		if math.Abs(u) <= localEps || math.Abs(v) <= localEps || math.Abs(1-u-v) <= localEps {
			ambig = append(ambig, "mesh_triangle_edge")
		}
		if t*worldRay.Direction.Length() <= bound {
			ambig = append(ambig, "ray_starts_on_mesh_surface")
		}
	}
	if math.IsInf(bestT, 1) {
		return false, 0, nil
	}
	return true, bestT, dedupeStrings(ambig)
}

func pointInEntry(e *IndexEntry, p Vec3) (bool, bool, string, float64) {
	bound := e.GeometryError + e.Quantum*float64(e.Depth+1)
	lp := e.WorldInvFloat.TransformPoint(p)
	minScale := minWorldScale(e.WorldFloat)
	switch e.Geometry.Kind {
	case GeometryAABB:
		return pointInAABB(lp, e.Geometry.Min, e.Geometry.Max, minScale, bound)
	case GeometrySphere:
		margin := (lp.Sub(e.Geometry.Center).Length() - e.Geometry.Radius) * minScale
		if math.Abs(margin) <= bound {
			return margin < 0, true, "point_near_sphere_boundary", math.Abs(margin)
		}
		return margin < 0, false, "", 0
	case GeometryMesh:
		lb := GeometryLocalBounds(e.Geometry)
		return pointInAABB(lp, lb.Min, lb.Max, minScale, bound)
	default:
		return false, false, "", 0
	}
}

func pointInAABB(p, min, max Vec3, scale, bound float64) (bool, bool, string, float64) {
	inside := p.X >= min.X && p.X <= max.X && p.Y >= min.Y && p.Y <= max.Y && p.Z >= min.Z && p.Z <= max.Z
	if inside {
		margin := math.Min(
			math.Min(p.X-min.X, max.X-p.X),
			math.Min(math.Min(p.Y-min.Y, max.Y-p.Y), math.Min(p.Z-min.Z, max.Z-p.Z)),
		) * scale
		if margin <= bound {
			return true, true, "point_near_aabb_boundary", margin
		}
		return true, false, "", 0
	}
	gap := math.Max(math.Max(min.X-p.X, p.X-max.X), math.Max(math.Max(min.Y-p.Y, p.Y-max.Y), math.Max(min.Z-p.Z, p.Z-max.Z)))
	gap *= scale
	if gap <= bound {
		return false, true, "point_near_aabb_boundary", gap
	}
	return false, false, "", 0
}

func distanceToAABB(p, min, max Vec3) float64 {
	dx := math.Max(math.Max(min.X-p.X, 0), p.X-max.X)
	dy := math.Max(math.Max(min.Y-p.Y, 0), p.Y-max.Y)
	dz := math.Max(math.Max(min.Z-p.Z, 0), p.Z-max.Z)
	if dx == 0 && dy == 0 && dz == 0 {
		return math.Min(math.Min(p.X-min.X, max.X-p.X), math.Min(math.Min(p.Y-min.Y, max.Y-p.Y), math.Min(p.Z-min.Z, max.Z-p.Z)))
	}
	return math.Sqrt(dx*dx + dy*dy + dz*dz)
}

func minWorldScale(m Mat4) float64 {
	sx := V3(m[0], m[4], m[8]).Length()
	sy := V3(m[1], m[5], m[9]).Length()
	sz := V3(m[2], m[6], m[10]).Length()
	return math.Max(1e-12, math.Min(sx, math.Min(sy, sz)))
}

func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return in
	}
	sort.Strings(in)
	out := in[:0]
	for _, s := range in {
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
}

func projectedAABB(a AABB, camera Camera) Rect {
	corners := []Vec3{
		{a.Min.X, a.Min.Y, a.Min.Z},
		{a.Max.X, a.Min.Y, a.Min.Z},
		{a.Min.X, a.Max.Y, a.Min.Z},
		{a.Max.X, a.Max.Y, a.Min.Z},
		{a.Min.X, a.Min.Y, a.Max.Z},
		{a.Max.X, a.Min.Y, a.Max.Z},
		{a.Min.X, a.Max.Y, a.Max.Z},
		{a.Max.X, a.Max.Y, a.Max.Z},
	}
	out := Rect{X0: math.Inf(1), Y0: math.Inf(1), X1: math.Inf(-1), Y1: math.Inf(-1)}
	for _, c := range corners {
		p, ok := camera.Project(c)
		if !ok {
			continue
		}
		out.X0 = math.Min(out.X0, p.X)
		out.Y0 = math.Min(out.Y0, p.Y)
		out.X1 = math.Max(out.X1, p.X)
		out.Y1 = math.Max(out.Y1, p.Y)
	}
	return out
}

func projectedOverlap(b, r Rect, eps float64) (bool, bool, string, float64) {
	ox := math.Min(b.X1, r.X1) - math.Max(b.X0, r.X0)
	oy := math.Min(b.Y1, r.Y1) - math.Max(b.Y0, r.Y0)
	if ox < -eps || oy < -eps {
		return false, false, "", 0
	}
	hit := ox >= 0 && oy >= 0
	margin := math.Min(ox, oy)
	if hit && margin <= eps {
		return true, true, "frame_overlap_at_error_bound", margin
	}
	if !hit {
		gap := math.Max(-ox, -oy)
		if gap <= eps {
			return false, true, "frame_gap_at_error_bound", gap
		}
	}
	return hit, false, "", 0
}
