package geom

import (
	"math"

	"combatjudge/internal/model"
)

const Epsilon = 1e-7

type Vec = [3]float64

type Segment struct {
	A Vec
	B Vec
}

type Contact struct {
	CenterDistance  float64
	SurfaceGap      float64
	TOI             float64
	ClosestTOI      float64
	Surface         string
	ClosestWeapon   Vec
	ClosestCollider Vec
}

func FVec(v model.Vec3) Vec {
	return Vec{float64(v.X) / float64(model.InternalScale), float64(v.Y) / float64(model.InternalScale), float64(v.Z) / float64(model.InternalScale)}
}

func SegmentFromModel(a, b model.Vec3) Segment {
	return Segment{A: FVec(a), B: FVec(b)}
}

func sub(a, b Vec) Vec             { return Vec{a[0] - b[0], a[1] - b[1], a[2] - b[2]} }
func add(a, b Vec) Vec             { return Vec{a[0] + b[0], a[1] + b[1], a[2] + b[2]} }
func scale(a Vec, s float64) Vec   { return Vec{a[0] * s, a[1] * s, a[2] * s} }
func dot(a, b Vec) float64         { return a[0]*b[0] + a[1]*b[1] + a[2]*b[2] }
func norm(a Vec) float64           { return math.Sqrt(dot(a, a)) }
func lerp(a, b Vec, t float64) Vec { return add(a, scale(sub(b, a), t)) }

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

func clampVec(v, lo, hi Vec) Vec {
	for i := range v {
		v[i] = math.Max(lo[i], math.Min(hi[i], v[i]))
	}
	return v
}

// SegmentSegmentClosest returns deterministic closest points and parameters.
// It evaluates the unconstrained solution and all four boundary features.
func SegmentSegmentClosest(p0, p1, q0, q1 Vec) (s, t float64, cp, cq Vec, dist float64) {
	u := sub(p1, p0)
	v := sub(q1, q0)
	w0 := sub(p0, q0)
	a := dot(u, u)
	b := dot(u, v)
	c := dot(v, v)
	d := dot(u, w0)
	e := dot(v, w0)
	den := a*c - b*b

	type candidate struct{ s, t float64 }
	candidates := []candidate{{0, 0}, {0, 1}, {1, 0}, {1, 1}}
	if den > Epsilon && a > Epsilon && c > Epsilon {
		sn := b*e - c*d
		tn := a*e - b*d
		if sn >= -Epsilon && sn <= den+Epsilon && tn >= -Epsilon && tn <= den+Epsilon {
			candidates = append(candidates, candidate{clamp01(sn / den), clamp01(tn / den)})
		}
	}
	if c > Epsilon {
		candidates = append(candidates, candidate{0, clamp01(e / c)})
		wAtS1 := add(w0, u)
		candidates = append(candidates, candidate{1, clamp01(dot(v, wAtS1) / c)})
	}
	if a > Epsilon {
		candidates = append(candidates, candidate{clamp01(-d / a), 0})
		candidates = append(candidates, candidate{clamp01((b - d) / a), 1})
	}

	best := math.Inf(1)
	for _, cand := range candidates {
		cp = lerp(p0, p1, cand.s)
		cq = lerp(q0, q1, cand.t)
		dd := norm(sub(cp, cq))
		if dd < best-1e-12 || (math.Abs(dd-best) <= 1e-12 && (cand.s < s || (cand.s == s && cand.t < t))) {
			best = dd
			s, t = cand.s, cand.t
		}
	}
	cp = lerp(p0, p1, s)
	cq = lerp(q0, q1, t)
	return s, t, cp, cq, best
}

type boxFeatureCandidate struct {
	t     float64
	p, q  Vec
	dist2 float64
}

func segmentAABB(seg Segment, mn, mx Vec) (centerDist, toi float64, surface string, cp, cq Vec) {
	candidates := []boxFeatureCandidate{}
	considerPoint := func(t float64) {
		t = clamp01(t)
		p := lerp(seg.A, seg.B, t)
		q := clampVec(p, mn, mx)
		d := sub(p, q)
		candidates = append(candidates, boxFeatureCandidate{t: t, p: p, q: q, dist2: dot(d, d)})
	}
	considerPoint(0)
	considerPoint(1)
	delta := sub(seg.B, seg.A)
	for axis := 0; axis < 3; axis++ {
		if math.Abs(delta[axis]) > Epsilon {
			for _, face := range []float64{mn[axis], mx[axis]} {
				t := (face - seg.A[axis]) / delta[axis]
				if t >= -Epsilon && t <= 1+Epsilon {
					considerPoint(t)
				}
			}
		}
	}

	edges := [12][2]Vec{
		{{mn[0], mn[1], mn[2]}, {mx[0], mn[1], mn[2]}},
		{{mn[0], mx[1], mn[2]}, {mx[0], mx[1], mn[2]}},
		{{mn[0], mn[1], mx[2]}, {mx[0], mn[1], mx[2]}},
		{{mn[0], mx[1], mx[2]}, {mx[0], mx[1], mx[2]}},
		{{mn[0], mn[1], mn[2]}, {mn[0], mx[1], mn[2]}},
		{{mx[0], mn[1], mn[2]}, {mx[0], mx[1], mn[2]}},
		{{mn[0], mn[1], mx[2]}, {mn[0], mx[1], mx[2]}},
		{{mx[0], mn[1], mx[2]}, {mx[0], mx[1], mx[2]}},
		{{mn[0], mn[1], mn[2]}, {mn[0], mn[1], mx[2]}},
		{{mx[0], mn[1], mn[2]}, {mx[0], mn[1], mx[2]}},
		{{mn[0], mx[1], mn[2]}, {mn[0], mx[1], mx[2]}},
		{{mx[0], mx[1], mn[2]}, {mx[0], mx[1], mx[2]}},
	}
	for _, edge := range edges {
		s, _, p, q, dist := SegmentSegmentClosest(seg.A, seg.B, edge[0], edge[1])
		candidates = append(candidates, boxFeatureCandidate{t: s, p: p, q: q, dist2: dist * dist})
	}

	bestIdx := 0
	for i := 1; i < len(candidates); i++ {
		if candidates[i].dist2 < candidates[bestIdx].dist2-1e-12 {
			bestIdx = i
		}
	}
	best := candidates[bestIdx]
	return math.Sqrt(best.dist2), best.t, aabbFeature(best.q, mn, mx), best.p, best.q
}

func aabbFeature(q, mn, mx Vec) string {
	const eps = 1e-6
	axisName := []string{"X", "Y", "Z"}
	boundary := []int{}
	sign := []string{}
	for i := 0; i < 3; i++ {
		if math.Abs(q[i]-mn[i]) <= eps {
			boundary = append(boundary, i)
			sign = append(sign, "-")
		} else if math.Abs(q[i]-mx[i]) <= eps {
			boundary = append(boundary, i)
			sign = append(sign, "+")
		}
	}
	switch len(boundary) {
	case 0:
		return "aabb:interior"
	case 1:
		return "aabb:face:" + sign[0] + axisName[boundary[0]]
	case 2:
		return "aabb:edge:" + sign[0] + axisName[boundary[0]] + sign[1] + axisName[boundary[1]]
	default:
		return "aabb:corner"
	}
}

func capsuleSurface(t float64) string {
	switch {
	case t <= 0.02:
		return "capsule:cap_start"
	case t >= 0.98:
		return "capsule:cap_end"
	default:
		return "capsule:shaft"
	}
}

func Analyze(weapon Segment, weaponRadiusQ int64, c model.Collider) Contact {
	weaponR := float64(weaponRadiusQ) / float64(model.InternalScale)
	var centerDist, closestT float64
	var closestSurface string
	var pw, pc Vec
	if c.Kind == model.KindAABB {
		mn, mx := FVec(c.Min), FVec(c.Max)
		centerDist, closestT, closestSurface, pw, pc = segmentAABB(weapon, mn, mx)
	}
	colliderR := float64(c.Radius) / float64(model.InternalScale)
	if c.Kind == model.KindCapsule {
		p0, p1 := FVec(c.P0), FVec(c.P1)
		var t float64
		closestT, t, pw, pc, centerDist = SegmentSegmentClosest(weapon.A, weapon.B, p0, p1)
		closestSurface = capsuleSurface(t)
	}
	threshold := weaponR + colliderR
	gap := math.Max(0, centerDist-threshold)
	contact := Contact{CenterDistance: centerDist, SurfaceGap: gap, TOI: closestT, ClosestTOI: closestT, Surface: closestSurface, ClosestWeapon: pw, ClosestCollider: pc}
	if centerDist <= threshold+Epsilon {
		entryT := firstContactTime(weapon, c, threshold, closestT)
		entryPoint := lerp(weapon.A, weapon.B, entryT)
		q, u, pointDist := closestPointOnCollider(entryPoint, c)
		contact.TOI = entryT
		contact.Surface = closestSurfaceAt(c, q, u)
		contact.ClosestWeapon = entryPoint
		contact.ClosestCollider = q
		contact.SurfaceGap = math.Max(0, pointDist-threshold)
	}
	return contact
}

func closestPointOnCollider(p Vec, c model.Collider) (q Vec, u float64, dist float64) {
	if c.Kind == model.KindAABB {
		q = clampVec(p, FVec(c.Min), FVec(c.Max))
		return q, 0, norm(sub(p, q))
	}
	a, b := FVec(c.P0), FVec(c.P1)
	d := sub(b, a)
	length2 := dot(d, d)
	if length2 <= Epsilon {
		q, u = a, 0
	} else {
		u = clamp01(dot(sub(p, a), d) / length2)
		q = lerp(a, b, u)
	}
	return q, u, norm(sub(p, q))
}

func closestSurfaceAt(c model.Collider, q Vec, u float64) string {
	if c.Kind == model.KindAABB {
		return aabbFeature(q, FVec(c.Min), FVec(c.Max))
	}
	return capsuleSurface(u)
}

func firstContactTime(weapon Segment, c model.Collider, threshold, knownTrueT float64) float64 {
	_, _, distAt0 := closestPointOnCollider(weapon.A, c)
	if distAt0 <= threshold+Epsilon {
		return 0
	}
	lo, hi := 0.0, clamp01(knownTrueT)
	if hi <= Epsilon {
		hi = 1
	}
	for i := 0; i < 48; i++ {
		mid := (lo + hi) / 2
		p := lerp(weapon.A, weapon.B, mid)
		_, _, dist := closestPointOnCollider(p, c)
		if dist <= threshold+Epsilon {
			hi = mid
		} else {
			lo = mid
		}
	}
	return hi
}

func QuantizeDistance(gap float64) int64 {
	if gap <= Epsilon {
		return 0
	}
	return int64(math.Floor(gap*float64(model.InternalScale) + 1e-9))
}

func BoundsFromSegment(seg Segment) (Vec, Vec) {
	mn := Vec{math.Min(seg.A[0], seg.B[0]), math.Min(seg.A[1], seg.B[1]), math.Min(seg.A[2], seg.B[2])}
	mx := Vec{math.Max(seg.A[0], seg.B[0]), math.Max(seg.A[1], seg.B[1]), math.Max(seg.A[2], seg.B[2])}
	return mn, mx
}

func ExpandedBoundsIntersect(aMin, aMax, bMin, bMax Vec) bool {
	for i := 0; i < 3; i++ {
		if aMax[i] < bMin[i]-Epsilon || bMax[i] < aMin[i]-Epsilon {
			return false
		}
	}
	return true
}

func ColliderBounds(c model.Collider) (Vec, Vec) {
	if c.Kind == model.KindAABB {
		return FVec(c.Min), FVec(c.Max)
	}
	mn := Vec{math.Min(float64(c.P0.X), float64(c.P1.X)) / 1e6, math.Min(float64(c.P0.Y), float64(c.P1.Y)) / 1e6, math.Min(float64(c.P0.Z), float64(c.P1.Z)) / 1e6}
	mx := Vec{math.Max(float64(c.P0.X), float64(c.P1.X)) / 1e6, math.Max(float64(c.P0.Y), float64(c.P1.Y)) / 1e6, math.Max(float64(c.P0.Z), float64(c.P1.Z)) / 1e6}
	r := float64(c.Radius) / 1e6
	for i := range mn {
		mn[i] -= r
		mx[i] += r
	}
	return mn, mx
}

func CandidateForFrame(seg Segment, weaponRadiusQ int64, c model.Collider) bool {
	mn, mx := BoundsFromSegment(seg)
	r := float64(weaponRadiusQ) / 1e6
	for i := range mn {
		mn[i] -= r
		mx[i] += r
	}
	cMin, cMax := ColliderBounds(c)
	return ExpandedBoundsIntersect(mn, mx, cMin, cMax)
}
