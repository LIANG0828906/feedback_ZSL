package terrainbake

import (
	"fmt"
	"math"
	"sort"
)

type pixelKey struct {
	blockID string
	x       int
	y       int
}

type Point struct {
	X int
	Y int
}

type seamPair struct {
	seamID    string
	index     int
	keyA      pixelKey
	keyB      pixelKey
	blockA    string
	blockB    string
	threshold float64
	maxIter   int
}

type seamResolution struct {
	Overrides  map[pixelKey]Vec3
	Stats      map[string]SeamStats
	PairAngles map[string]float64
}

func edgePoint(b *Block, e Edge, index int) Point {
	switch e {
	case EdgeLeft:
		return Point{X: 0, Y: index}
	case EdgeRight:
		return Point{X: b.Width - 1, Y: index}
	case EdgeTop:
		return Point{X: index, Y: 0}
	default:
		return Point{X: index, Y: b.Height - 1}
	}
}

func edgeIndexB(p *Project, s *Seam, indexA int) int {
	if !s.ReverseB {
		return indexA
	}
	b, _ := FindBlock(p, s.BlockB)
	return edgeLength(b, s.EdgeB) - 1 - indexA
}

func buildSeamPairs(p *Project) ([]seamPair, error) {
	seams := make([]Seam, len(p.Seams))
	copy(seams, p.Seams)
	sort.Slice(seams, func(i, j int) bool { return seams[i].ID < seams[j].ID })
	var pairs []seamPair
	for i := range seams {
		s := &seams[i]
		a, okA := FindBlock(p, s.BlockA)
		b, okB := FindBlock(p, s.BlockB)
		if !okA || !okB {
			return nil, &Error{Code: CodeSchema, Object: "seam:" + s.ID, Field: "block_a", SeamID: s.ID, Message: "seam references a missing block"}
		}
		n := edgeLength(a, s.EdgeA)
		if n != edgeLength(b, s.EdgeB) {
			return nil, &Error{Code: CodeSchema, Object: "seam:" + s.ID, Field: "edge_b", SeamID: s.ID, Message: "seam edge lengths differ"}
		}
		for idx := 0; idx < n; idx++ {
			pa := edgePoint(a, s.EdgeA, idx)
			pb := edgePoint(b, s.EdgeB, edgeIndexB(p, s, idx))
			pairs = append(pairs, seamPair{
				seamID: s.ID, index: idx,
				keyA:   pixelKey{s.BlockA, pa.X, pa.Y},
				keyB:   pixelKey{s.BlockB, pb.X, pb.Y},
				blockA: s.BlockA, blockB: s.BlockB,
				threshold: s.MaxAngleDeg, maxIter: s.MaxIterations,
			})
		}
	}
	return pairs, nil
}

// resolveSeams performs deterministic, synchronous relaxation on every seam
// pair in the supplied raw epoch. Both blocks of a pair see the same sequence
// of vectors, so either block can independently persist its side without a
// hidden "last writer wins" dependency.
func resolveSeams(p *Project, raw map[string]*RawBlock) (*seamResolution, error) {
	return resolveSeamsEdge(p, func(k pixelKey) (Vec3, bool) {
		r, ok := raw[k.blockID]
		if !ok || k.x < 0 || k.x >= r.Width || k.y < 0 || k.y >= r.Height {
			return Vec3{}, false
		}
		return r.Normals[k.y*r.Width+k.x], true
	})
}

// resolveSeamsEdge is the memory-bounded form used by the scheduler. It only
// asks for edge pixels, so raw full-block buffers can stay in temp spool files.
func resolveSeamsEdge(p *Project, rawAt func(pixelKey) (Vec3, bool)) (*seamResolution, error) {
	pairs, err := buildSeamPairs(p)
	if err != nil {
		return nil, err
	}
	res := &seamResolution{
		Overrides:  make(map[pixelKey]Vec3),
		Stats:      make(map[string]SeamStats),
		PairAngles: make(map[string]float64),
	}
	cur := make(map[pixelKey]Vec3)
	active := make([]seamPair, 0, len(pairs))
	maxRounds := 0
	seenBlock := make(map[string]bool)
	for _, pair := range pairs {
		a, okA := rawAt(pair.keyA)
		b, okB := rawAt(pair.keyB)
		if !okA || !okB {
			continue
		}
		if _, ok := cur[pair.keyA]; !ok {
			cur[pair.keyA] = a
		}
		if _, ok := cur[pair.keyB]; !ok {
			cur[pair.keyB] = b
		}
		active = append(active, pair)
		seenBlock[pair.blockA] = true
		seenBlock[pair.blockB] = true
		if pair.maxIter > maxRounds {
			maxRounds = pair.maxIter
		}
	}
	for id := range seenBlock {
		res.Stats[id] = SeamStats{BlockID: id}
	}

	const step = 0.25
	for round := 1; round <= maxRounds; round++ {
		for _, pair := range active {
			if round > pair.maxIter {
				continue
			}
			a, b := cur[pair.keyA], cur[pair.keyB]
			ang := angleDeg(a, b)
			if ang <= pair.threshold {
				continue
			}
			if isOpposite(a, b) {
				return nil, seamAngleError(pair, ang, "normals are opposite; no deterministic continuity direction exists")
			}
			na := rotateToward(a, b, step)
			nb := rotateToward(b, a, step)
			var ok bool
			na, ok = normalize(na)
			if !ok {
				return nil, seamAngleError(pair, ang, "seam relaxation produced a zero-length normal")
			}
			nb, ok = normalize(nb)
			if !ok {
				return nil, seamAngleError(pair, ang, "seam relaxation produced a zero-length normal")
			}
			cur[pair.keyA], cur[pair.keyB] = na, nb
		}
	}

	for _, pair := range active {
		a, b := cur[pair.keyA], cur[pair.keyB]
		ang := angleDeg(a, b)
		res.PairAngles[seamPairKey(pair)] = ang
		sa := res.Stats[pair.blockA]
		sb := res.Stats[pair.blockB]
		sa.Constrained++
		sb.Constrained++
		if ang > sa.MaxAngleDeg {
			sa.MaxAngleDeg = ang
		}
		if ang > sb.MaxAngleDeg {
			sb.MaxAngleDeg = ang
		}
		if ang > pair.threshold {
			sa.Violations++
			sb.Violations++
		}
		res.Stats[pair.blockA] = sa
		res.Stats[pair.blockB] = sb
		res.Overrides[pair.keyA] = a
		res.Overrides[pair.keyB] = b
		if ang > pair.threshold {
			return nil, seamAngleError(pair, ang, "seam angle still exceeds threshold after iteration limit")
		}
	}
	for id := range seenBlock {
		if _, ok := res.Stats[id]; !ok {
			res.Stats[id] = SeamStats{BlockID: id}
		}
	}
	return res, nil
}

func seamPairKey(p seamPair) string {
	return fmt.Sprintf("%s#%d", p.seamID, p.index)
}

func isOpposite(a, b Vec3) bool {
	cx := a.Y*b.Z - a.Z*b.Y
	cy := a.Z*b.X - a.X*b.Z
	cz := a.X*b.Y - a.Y*b.X
	dot := a.X*b.X + a.Y*b.Y + a.Z*b.Z
	return dot < 0 && math.Sqrt(cx*cx+cy*cy+cz*cz) < 1e-12
}

func rotateToward(from, toward Vec3, t float64) Vec3 {
	dot := math.Max(-1, math.Min(1, from.X*toward.X+from.Y*toward.Y+from.Z*toward.Z))
	ang := math.Acos(dot)
	if ang < 1e-14 {
		return from
	}
	cx := from.Y*toward.Z - from.Z*toward.Y
	cy := from.Z*toward.X - from.X*toward.Z
	cz := from.X*toward.Y - from.Y*toward.X
	axis, ok := normalize(Vec3{cx, cy, cz})
	if !ok {
		return from
	}
	r := ang * t
	c, s := math.Cos(r), math.Sin(r)
	dotAxis := axis.X*from.X + axis.Y*from.Y + axis.Z*from.Z
	out := Vec3{
		X: from.X*c + (axis.Y*from.Z-axis.Z*from.Y)*s + axis.X*dotAxis*(1-c),
		Y: from.Y*c + (axis.Z*from.X-axis.X*from.Z)*s + axis.Y*dotAxis*(1-c),
		Z: from.Z*c + (axis.X*from.Y-axis.Y*from.X)*s + axis.Z*dotAxis*(1-c),
	}
	out, ok = normalize(out)
	if !ok {
		return from
	}
	return out
}

func seamAngleError(pair seamPair, angle float64, msg string) *Error {
	return &Error{
		Code: CodeSeam, Object: "seam:" + pair.seamID, Field: "max_angle_deg",
		BlockA: pair.blockA, BlockB: pair.blockB, SeamID: pair.seamID,
		Angle: angle, Limit: pair.threshold, Message: msg,
		Details: map[string]any{"edge_index": pair.index},
	}
}
