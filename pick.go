package editorpick

import (
	"math/big"
	"sort"
)

type RayQuery struct {
	ID     string     `json:"id"`
	Screen [2]float64 `json:"screen"`
	Origin Vec3       `json:"origin"`
	Dir    Vec3       `json:"direction"`
}

type Ambiguity struct {
	Code          string `json:"code"`
	QueryID       string `json:"query_id,omitempty"`
	NodeID        string `json:"node_id"`
	GeometryIndex int    `json:"geometry_index"`
	Kind          string `json:"kind"`
	Reason        string `json:"reason"`
	Epsilon       Fixed  `json:"epsilon"`
	Margin        Fixed  `json:"margin,omitempty"`
	Point         Vec3   `json:"point"`
	Lower         Vec3   `json:"lower"`
	Upper         Vec3   `json:"upper"`
}

func (a *Ambiguity) Error() string {
	return (&DomainError{Code: EAmbiguous, NodeID: a.NodeID, QueryID: a.QueryID, Message: a.Reason}).Error()
}

func (a *Ambiguity) DomainError() *DomainError {
	return &DomainError{Code: EAmbiguous, NodeID: a.NodeID, QueryID: a.QueryID, Message: a.Reason}
}

type RayHit struct {
	NodeID        string `json:"node_id"`
	GeometryIndex int    `json:"geometry_index"`
	Point         Vec3   `json:"point"`
	Distance      Fixed  `json:"distance"`
	Parameter     string `json:"parameter_rational,omitempty"`
	ErrorBound    Fixed  `json:"error_bound"`
}

type RayResult struct {
	QueryID   string     `json:"query_id"`
	Status    string     `json:"status"`
	Hit       *RayHit    `json:"hit,omitempty"`
	Ambiguity *Ambiguity `json:"ambiguity,omitempty"`
	Visited   []string   `json:"visited_entries,omitempty"`
}

type localPick struct {
	status      string
	reason      string
	interval    rootInterval
	point       [3]*big.Rat
	epsilon     int64
	boundUnits  int64
	marginUnits int64
}

type candidate struct {
	nodeID        string
	geometryIndex int
	status        string
	worldT        *big.Rat
	tLower        *big.Rat
	tUpper        *big.Rat
	worldPoint    Vec3
	distance      Fixed
	boundUnits    int64
	local         localPick
}

func (e *Engine) Raycast(q RayQuery) (*RayResult, error) {
	if q.Dir.X == 0 && q.Dir.Y == 0 && q.Dir.Z == 0 {
		return nil, &DomainError{Code: ESchema, QueryID: q.ID, Message: "ray direction must not be zero"}
	}
	e.mu.RLock()
	defer e.mu.RUnlock()

	result := &RayResult{QueryID: q.ID, Status: "miss", Visited: []string{}}
	var candidates []candidate
	for _, rootID := range e.scene.Roots {
		e.raycastNodeLocked(rootID, q, result, &candidates)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		ti, tj := candidates[i].worldT, candidates[j].worldT
		if ti == nil || tj == nil {
			return candidates[i].nodeID < candidates[j].nodeID
		}
		if c := ti.Cmp(tj); c != 0 {
			return c < 0
		}
		return candidates[i].nodeID < candidates[j].nodeID
	})

	var bestDefinite *candidate
	for i := range candidates {
		c := &candidates[i]
		if c.status == "hit" {
			bestDefinite = c
			break
		}
	}
	var firstAmbiguous *candidate
	for i := range candidates {
		c := &candidates[i]
		if c.status != EAmbiguous {
			continue
		}
		if bestDefinite != nil {
			// Ignore an ambiguity only when its whole possible parameter
			// interval starts after the definite hit's upper interval.
			if c.tLower != nil && bestDefinite.tUpper != nil && c.tLower.Cmp(bestDefinite.tUpper) > 0 {
				continue
			}
		}
		firstAmbiguous = c
		break
	}
	if firstAmbiguous != nil {
		amb := e.buildAmbiguityLocked(q, *firstAmbiguous)
		result.Status = EAmbiguous
		result.Ambiguity = amb
		return result, nil
	}
	if bestDefinite != nil {
		hit := &RayHit{
			NodeID:        bestDefinite.nodeID,
			GeometryIndex: bestDefinite.geometryIndex,
			Point:         bestDefinite.worldPoint,
			Distance:      bestDefinite.distance,
			ErrorBound:    Fixed(bestDefinite.boundUnits),
		}
		if bestDefinite.worldT != nil {
			hit.Parameter = bestDefinite.worldT.RatString()
		}
		result.Status = "hit"
		result.Hit = hit
		return result, nil
	}
	return result, nil
}

func (e *Engine) raycastNodeLocked(nodeID string, q RayQuery, result *RayResult, candidates *[]candidate) {
	entry := e.index[nodeID]
	d := e.derived[nodeID]
	node := e.scene.NodeByID[nodeID]
	if entry == nil || d == nil || node == nil {
		return
	}
	result.Visited = append(result.Visited, nodeID)
	if entry.HasBounds {
		if _, _, ok := rayAABBInterval(q.Origin, q.Dir, entry.SubtreeBounds); !ok {
			return
		}
	}

	if node.GeometryIndex >= 0 {
		g := e.scene.Geometries[node.GeometryIndex]
		localO, err := d.Inverse.TransformPoint(q.Origin)
		if err == nil {
			localD, errDir := transformDirection(d.Inverse, q.Dir)
			if errDir == nil {
				eps := pickEpsilonUnits(d, g.LocalAABB, localO, localD)
				pick := classifyGeometry(g, localO, localD, eps)
				if pick.status != "miss" {
					if cand, ok := e.makeCandidateLocked(q, nodeID, node.GeometryIndex, pick); ok {
						*candidates = append(*candidates, cand)
					}
				}
			}
		}
	}
	for _, childID := range e.scene.Children[nodeID] {
		e.raycastNodeLocked(childID, q, result, candidates)
	}
}

func transformDirection(m Mat4, v Vec3) (Vec3, error) {
	homogeneous := [3]int64{int64(v.X), int64(v.Y), int64(v.Z)}
	var out Vec3
	values := [3]*Fixed{&out.X, &out.Y, &out.Z}
	for row := 0; row < 3; row++ {
		var numerator big.Int
		for col := 0; col < 3; col++ {
			var term big.Int
			term.Mul(big.NewInt(int64(m[row*4+col])), big.NewInt(homogeneous[col]))
			numerator.Add(&numerator, &term)
		}
		q, ok := roundedQuo(&numerator, FixedScale)
		if !ok {
			return Vec3{}, errOverflow
		}
		*values[row] = Fixed(q)
	}
	return out, nil
}

func pickEpsilonUnits(d *Derived, localBounds AABB, o, dir Vec3) int64 {
	ext := localBounds.Extents()
	maxSize := maxFixed(absFixed(ext.X), maxFixed(absFixed(ext.Y), absFixed(ext.Z)))
	originDist := upperSqrtBig(squaresBig(o.X, o.Y, o.Z))
	return DeclaredPickErrorBound(d.Depth, d.MaxMatrixErrorUnits, maxSize, Fixed(originDist)).Units
}

// ErrorBound is the conservative fixed-point picking envelope.
type ErrorBound struct {
	Depth          int   `json:"depth"`
	Step           Fixed `json:"step"`
	ChainError     Fixed `json:"chain_error"`
	GeometrySize   Fixed `json:"geometry_size"`
	RayDistance    Fixed `json:"ray_distance"`
	MagnitudeUnits int64 `json:"magnitude_units"`
	Units          int64 `json:"units"`
	Bound          Fixed `json:"bound"`
}

// DeclaredPickErrorBound returns
//
//	U = Echain/δ + 2 + (depth+1) + 2 + 2*ceil((L+D)/S)
//
// where δ=1/FixedScale is one fixed ULP, L is the largest local geometry
// extent and D is the local ray-origin distance. The returned Bound is U ULPs.
// The extra constants cover matrix-product rounding and ray/edge amplification.
func DeclaredPickErrorBound(depth int, chainErrorUnits int64, geometrySize, rayDistance Fixed) ErrorBound {
	size := absFixed(geometrySize)
	distance := absFixed(rayDistance)
	numerator := big.NewInt(int64(size) + int64(distance))
	magnitude := new(big.Int).Add(numerator, big.NewInt(FixedScale-1))
	magnitude.Div(magnitude, big.NewInt(FixedScale))
	mag := int64(0)
	if magnitude.IsInt64() {
		mag = magnitude.Int64()
	}
	units := chainErrorUnits + 2 + int64(depth+1) + 2 + 2*mag
	return ErrorBound{
		Depth: depth, Step: Fixed(1), ChainError: Fixed(chainErrorUnits),
		GeometrySize: geometrySize, RayDistance: rayDistance,
		MagnitudeUnits: mag, Units: units, Bound: Fixed(units),
	}
}

func classifyGeometry(g Geometry, o, dir Vec3, eps int64) localPick {
	switch g.Kind {
	case GeometryAABB:
		return classifyAABB(o, dir, g.LocalAABB, eps)
	case GeometrySphere:
		return classifySphere(o, dir, g.Center, g.Radius, eps)
	case GeometryMesh:
		return classifyMesh(g, o, dir, eps)
	default:
		return localPick{status: "miss"}
	}
}

func classifyAABB(o, dir Vec3, b AABB, eps int64) localPick {
	lo, _, exactOK := rayAABBInterval(o, dir, b)
	inflated := b.Expand(Fixed(eps))
	inflatedLo, _, inflatedOK := rayAABBInterval(o, dir, inflated)
	if !inflatedOK {
		return localPick{status: "miss", epsilon: eps}
	}
	eroded := AABB{
		Min: Vec3{b.Min.X + Fixed(eps), b.Min.Y + Fixed(eps), b.Min.Z + Fixed(eps)},
		Max: Vec3{b.Max.X - Fixed(eps), b.Max.Y - Fixed(eps), b.Max.Z - Fixed(eps)},
	}
	erodedOK := eroded.Valid()
	var erodedLo *big.Rat
	if erodedOK {
		erodedLo, _, erodedOK = rayAABBInterval(o, dir, eroded)
	}
	t := lo
	if !exactOK || t == nil {
		t = inflatedLo
	}
	if erodedOK && erodedLo != nil {
		// The definite interval has the same entry point as the exact box;
		// keep the exact t when available.
		_ = erodedLo
	}
	if t == nil {
		t = big.NewRat(0, 1)
	}
	pick := localPick{interval: rootInterval{lo: t, hi: t}, point: ratPointAt(o, dir, t), epsilon: eps, boundUnits: eps*2 + 2}
	if erodedOK {
		pick.status = "hit"
		pick.reason = "ray crosses AABB outside declared error boundary"
	} else {
		pick.status = EAmbiguous
		pick.reason = "ray is within the fixed-point/chain error boundary of an AABB face or edge"
	}
	return pick
}

func classifySphere(o, dir, center Vec3, radius Fixed, eps int64) localPick {
	exact, exactOK := raySphereFirstInterval(o, dir, center, radius)
	inflated, inflatedOK := raySphereFirstInterval(o, dir, center, radius+Fixed(eps))
	if !inflatedOK {
		return localPick{status: "miss", epsilon: eps}
	}
	definite := false
	if radius > Fixed(eps) {
		if _, ok := raySphereFirstInterval(o, dir, center, radius-Fixed(eps)); ok {
			definite = true
		}
	}
	mid := intervalMidpoint(exact)
	if !exactOK {
		mid = intervalMidpoint(inflated)
	}
	pick := localPick{
		interval:   exact,
		point:      ratPointAt(o, dir, mid),
		epsilon:    eps,
		boundUnits: eps*2 + 2,
	}
	if definite {
		pick.status = "hit"
		pick.reason = "ray crosses sphere outside declared error boundary"
	} else {
		pick.status = EAmbiguous
		pick.reason = "ray is within the fixed-point/chain error boundary of the sphere surface"
	}
	return pick
}

func intervalMidpoint(in rootInterval) *big.Rat {
	if in.lo == nil {
		return big.NewRat(0, 1)
	}
	if in.hi == nil {
		return new(big.Rat).Set(in.lo)
	}
	return new(big.Rat).Mul(new(big.Rat).Add(in.lo, in.hi), big.NewRat(1, 2))
}

func ratPointAt(o, d Vec3, t *big.Rat) [3]*big.Rat {
	or := ratVec(o)
	dr := ratVec(d)
	return [3]*big.Rat{
		new(big.Rat).Add(or[0], new(big.Rat).Mul(t, dr[0])),
		new(big.Rat).Add(or[1], new(big.Rat).Mul(t, dr[1])),
		new(big.Rat).Add(or[2], new(big.Rat).Mul(t, dr[2])),
	}
}

func classifyMesh(g Geometry, o, dir Vec3, eps int64) localPick {
	var picks []localPick
	for _, tri := range g.Triangles {
		a := g.Vertices[tri[0]]
		b := g.Vertices[tri[1]]
		c := g.Vertices[tri[2]]
		ev := rayTriangleEval(o, dir, a, b, c)
		altLower := triangleAltitudeLower(a, b, c)
		slide, parallelFar := triangleSlide(a, b, c, dir, eps)
		required := new(big.Rat).Mul(big.NewRat(eps, 1), big.NewRat(4, 1))
		if slide != nil {
			required = new(big.Rat).Mul(big.NewRat(eps, 1), new(big.Rat).Add(big.NewRat(2, 1), slide))
		}
		requiredUnits, _ := ceilRat(required)
		if ev.parallel {
			if parallelFar {
				continue
			}
			picks = append(picks, localPick{
				status: EAmbiguous, reason: "parallel ray is within error boundary of triangle plane",
				point: ratPointAt(o, dir, big.NewRat(0, 1)), epsilon: eps, boundUnits: requiredUnits + eps,
			})
			continue
		}
		if ev.t == nil || ev.t.Sign() < 0 {
			continue
		}
		lambda0 := new(big.Rat).Sub(big.NewRat(1, 1), new(big.Rat).Add(ev.u, ev.v))
		marginRat := new(big.Rat).Mul(minRat(lambda0, ev.u, ev.v), altLower)
		if ev.hit {
			pick := localPick{
				interval: rootInterval{lo: ev.t, hi: ev.t}, point: ratPointAt(o, dir, ev.t),
				epsilon: eps, boundUnits: requiredUnits + 2, marginUnits: ratCeil(marginRat),
			}
			if marginRat.Cmp(required) > 0 {
				pick.status = "hit"
				pick.reason = "ray crosses triangle interior outside declared error boundary"
			} else {
				pick.status = EAmbiguous
				pick.reason = "hit is too close to a triangle edge under the declared error boundary"
			}
			picks = append(picks, pick)
			continue
		}
		worstOutside := maxRat(big.NewRat(0, 1), new(big.Rat).Neg(lambda0), new(big.Rat).Neg(ev.u), new(big.Rat).Neg(ev.v))
		outsideDistance := new(big.Rat).Mul(worstOutside, altLower)
		pick := localPick{
			interval: rootInterval{lo: ev.t, hi: ev.t}, point: ratPointAt(o, dir, ev.t),
			epsilon: eps, boundUnits: requiredUnits + 2, marginUnits: ratCeil(outsideDistance),
		}
		if outsideDistance.Cmp(required) > 0 {
			pick.status = "miss"
		} else {
			pick.status = EAmbiguous
			pick.reason = "ray misses triangle only within the declared edge/error boundary"
			picks = append(picks, pick)
		}
	}
	if len(picks) == 0 {
		return localPick{status: "miss", epsilon: eps}
	}
	sort.SliceStable(picks, func(i, j int) bool {
		return picks[i].interval.lo.Cmp(picks[j].interval.lo) < 0
	})
	first := picks[0]
	for _, p := range picks {
		if p.status == EAmbiguous {
			first = p
			break
		}
		if p.status == "hit" {
			first = p
			break
		}
	}
	return first
}

func minRat(values ...*big.Rat) *big.Rat {
	out := values[0]
	for _, v := range values[1:] {
		if v.Cmp(out) < 0 {
			out = v
		}
	}
	return out
}

func maxRat(values ...*big.Rat) *big.Rat {
	out := values[0]
	for _, v := range values[1:] {
		if v.Cmp(out) > 0 {
			out = v
		}
	}
	return out
}

func ceilRat(r *big.Rat) (int64, bool) {
	q, ok := ceilQuo(r.Num(), r.Denom().Int64())
	return q, ok
}

func ratCeil(r *big.Rat) int64 {
	v, _ := ceilRat(r)
	return v
}

type bigVec3 [3]*big.Int

func bigVecFromFixed(v Vec3) bigVec3 {
	return bigVec3{big.NewInt(int64(v.X)), big.NewInt(int64(v.Y)), big.NewInt(int64(v.Z))}
}

func bigSub(a, b bigVec3) bigVec3 {
	return bigVec3{new(big.Int).Sub(a[0], b[0]), new(big.Int).Sub(a[1], b[1]), new(big.Int).Sub(a[2], b[2])}
}

func bigDot(a, b bigVec3) *big.Int {
	return new(big.Int).Add(new(big.Int).Mul(a[0], b[0]), new(big.Int).Add(new(big.Int).Mul(a[1], b[1]), new(big.Int).Mul(a[2], b[2])))
}

func bigCross(a, b bigVec3) bigVec3 {
	return bigVec3{
		new(big.Int).Sub(new(big.Int).Mul(a[1], b[2]), new(big.Int).Mul(a[2], b[1])),
		new(big.Int).Sub(new(big.Int).Mul(a[2], b[0]), new(big.Int).Mul(a[0], b[2])),
		new(big.Int).Sub(new(big.Int).Mul(a[0], b[1]), new(big.Int).Mul(a[1], b[0])),
	}
}

func bigNormSq(a bigVec3) *big.Int { return bigDot(a, a) }

func triangleAltitudeLower(a, b, c Vec3) *big.Rat {
	av, bv, cv := bigVecFromFixed(a), bigVecFromFixed(b), bigVecFromFixed(c)
	e1 := bigSub(bv, av)
	e2 := bigSub(cv, av)
	n := bigCross(e1, e2)
	area2 := new(big.Int).Sqrt(bigNormSq(n))
	maxEdge := upperSqrtBig(bigNormSq(e1))
	if e3 := bigSub(cv, bv); upperSqrtBig(bigNormSq(e3)) > maxEdge {
		maxEdge = upperSqrtBig(bigNormSq(e3))
	}
	if upperSqrtBig(bigNormSq(e2)) > maxEdge {
		maxEdge = upperSqrtBig(bigNormSq(e2))
	}
	if maxEdge == 0 {
		return big.NewRat(0, 1)
	}
	return new(big.Rat).SetFrac(area2, big.NewInt(maxEdge))
}

func triangleSlide(a, b, c, dir Vec3, eps int64) (*big.Rat, bool) {
	av, bv, cv, dv := bigVecFromFixed(a), bigVecFromFixed(b), bigVecFromFixed(c), bigVecFromFixed(dir)
	e1 := bigSub(bv, av)
	e2 := bigSub(cv, av)
	n := bigCross(e1, e2)
	nd := new(big.Int).Abs(bigDot(n, dv))
	if nd.Sign() == 0 {
		planeNum := new(big.Int).Abs(bigDot(n, bigSub(bigVecFromFixed(oZero()), av)))
		_ = planeNum
		// Parallel classification is handled by triangleParallelFar below via
		// the caller's ray origin; keep this conservative path non-far.
		return nil, false
	}
	dLen := upperSqrtBig(bigNormSq(dv))
	nLen := upperSqrtBig(bigNormSq(n))
	numerator := new(big.Int).Mul(big.NewInt(dLen), big.NewInt(nLen))
	return new(big.Rat).SetFrac(numerator, nd), false
}

func oZero() Vec3 { return Vec3{} }
