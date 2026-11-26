package maskqa

import (
	"fmt"
	"math"
	"sort"
)

type RadiusPlan struct {
	Segments  []Segment        `json:"segments"`
	Decisions []RadiusDecision `json:"decisions"`
	Field     *IntField        `json:"field"`
	SegmentAt []string         `json:"segment_at"`
	MicroAt   []int64          `json:"micro_at"`
}

func BuildRadiusPlan(s *Sample, edgeBand *Mask, p Params) (*RadiusPlan, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	segments := ExtractBoundarySegments(s.Rough, p.CurvatureWindow)
	for i := range segments {
		segments[i].BlockIDs = blocksForPoints(segments[i].Points, s.Blocks)
	}
	decisions, err := MapSegmentRadii(segments, p)
	if err != nil {
		return nil, err
	}
	decisionByID := map[string]RadiusDecision{}
	radiusByID := map[string]int{}
	for _, d := range decisions {
		decisionByID[d.SegmentID] = d
		radiusByID[d.SegmentID] = d.DilateRadius
	}

	field := NewIntField(s.Width, s.Height)
	segmentAt := make([]string, s.Width*s.Height)
	microAt := make([]int64, s.Width*s.Height)
	type flatPoint struct {
		seg string
		p   imagePoint
	}
	var points []flatPoint
	for _, seg := range segments {
		for _, pt := range seg.Points {
			points = append(points, flatPoint{seg.ID, pt})
		}
	}
	for y := 0; y < s.Height; y++ {
		for x := 0; x < s.Width; x++ {
			bestDist := math.MaxInt64
			bestSeg := ""
			for _, fp := range points {
				dx := x - fp.p.x
				dy := y - fp.p.y
				d := dx*dx + dy*dy
				if d < bestDist {
					bestDist = d
					bestSeg = fp.seg
				}
			}
			if bestSeg == "" {
				bestSeg = "seg-001"
			}
			idx := y*s.Width + x
			segmentAt[idx] = bestSeg
			field.Values[idx] = radiusByID[bestSeg]
			if field.Values[idx] == 0 {
				field.Values[idx] = p.RadiusMin
			}
			microAt[idx] = decisionByID[bestSeg].TruncationMicro
		}
	}
	_ = edgeBand
	return &RadiusPlan{Segments: segments, Decisions: decisions, Field: field, SegmentAt: segmentAt, MicroAt: microAt}, nil
}

func ComputeRadiusBlock(plan *RadiusPlan, block BlockCoord, p Params) (*RadiusOutput, error) {
	out := &RadiusOutput{Block: block, Field: make([]byte, block.Width*block.Height)}
	decisionSeen := map[string]bool{}
	for ly := 0; ly < block.Height; ly++ {
		for lx := 0; lx < block.Width; lx++ {
			x, y := block.OriginX+lx, block.OriginY+ly
			globalIdx := y*plan.Field.Width + x
			localIdx := ly*block.Width + lx
			r := plan.Field.Values[globalIdx]
			out.Field[localIdx] = byte(r)
			segID := plan.SegmentAt[globalIdx]
			micro := plan.MicroAt[globalIdx]
			trace := PixelTrace{
				X: x, Y: y, BlockX: block.IndexX, BlockY: block.IndexY,
				SegmentID: segID, Radius: r, RadiusMicro: micro,
				Truncations: []Truncation{{
					Level: "radius", X: x, Y: y, Operation: "curvature_quantile_and_integer_radius_map",
					Remainder: micro, Denominator: p.TraceScale, ContributionMicro: micro,
					TruncatedAt: "curvature_q_div_and_linear_radius_floor",
				}},
			}
			out.Traces = append(out.Traces, trace)
			_ = localIdx
			if d, ok := lookupDecision(plan.Decisions, segID); ok && !decisionSeen[d.SegmentID] {
				d.BlockID = block.ID()
				d.BlockX = block.IndexX
				d.BlockY = block.IndexY
				out.Decisions = append(out.Decisions, d)
				decisionSeen[d.SegmentID] = true
			}
		}
	}
	sort.Slice(out.Decisions, func(i, j int) bool { return out.Decisions[i].SegmentID < out.Decisions[j].SegmentID })
	return out, nil
}

func lookupDecision(ds []RadiusDecision, id string) (RadiusDecision, bool) {
	for _, d := range ds {
		if d.SegmentID == id {
			return d, true
		}
	}
	return RadiusDecision{}, false
}

func blocksForPoints(points []imagePoint, blocks []BlockCoord) []string {
	seen := map[string]bool{}
	var out []string
	for _, pt := range points {
		for _, b := range blocks {
			r := Rect{X: b.OriginX, Y: b.OriginY, Width: b.Width, Height: b.Height}
			if r.Contains(pt.x, pt.y) && !seen[b.ID()] {
				seen[b.ID()] = true
				out = append(out, b.ID())
			}
		}
	}
	sort.Strings(out)
	return out
}

func ExtractBoundarySegments(m *Mask, window int) []Segment {
	boundary := NewMask(m.Width, m.Height)
	for y := 0; y < m.Height; y++ {
		for x := 0; x < m.Width; x++ {
			if m.At(x, y) == 1 && (m.At(x-1, y) == 0 || m.At(x+1, y) == 0 || m.At(x, y-1) == 0 || m.At(x, y+1) == 0) {
				boundary.Set(x, y, 1)
			}
		}
	}
	visited := NewMask(m.Width, m.Height)
	neighbors := []imagePoint{{0, -1}, {1, -1}, {1, 0}, {1, 1}, {0, 1}, {-1, 1}, {-1, 0}, {-1, -1}}
	var paths [][]imagePoint
	for y := 0; y < m.Height; y++ {
		for x := 0; x < m.Width; x++ {
			if boundary.At(x, y) == 0 || visited.At(x, y) == 1 {
				continue
			}
			start := imagePoint{x, y}
			path := []imagePoint{start}
			inPath := map[imagePoint]bool{start: true}
			prev := imagePoint{-100000, -100000}
			cur := start
			closed := false
			for step := 0; step < m.Width*m.Height*4; step++ {
				var candidates []imagePoint
				for _, d := range neighbors {
					cand := imagePoint{cur.x + d.x, cur.y + d.y}
					if cand == prev {
						continue
					}
					if cand == start && len(path) >= 3 {
						candidates = append(candidates, cand)
						continue
					}
					if boundary.At(cand.x, cand.y) == 1 && visited.At(cand.x, cand.y) == 0 && !inPath[cand] {
						candidates = append(candidates, cand)
					}
				}
				if len(candidates) == 0 {
					break
				}
				next := candidates[0]
				if step > 0 {
					bestAngle := math.MaxFloat64
					inX := float64(cur.x - prev.x)
					inY := float64(cur.y - prev.y)
					for _, cand := range candidates {
						outX := float64(cand.x - cur.x)
						outY := float64(cand.y - cur.y)
						dot := inX*outX + inY*outY
						cross := math.Abs(inX*outY - inY*outX)
						angle := math.Atan2(cross, dot)
						if cand == start {
							angle -= 0.0001
						}
						if angle < bestAngle {
							bestAngle = angle
							next = cand
						}
					}
				}
				if next == start {
					closed = true
					break
				}
				path = append(path, next)
				inPath[next] = true
				prev, cur = cur, next
			}
			for _, pt := range path {
				visited.Set(pt.x, pt.y, 1)
			}
			if len(path) > 0 {
				paths = append(paths, path)
			}
			_ = closed
		}
	}

	arcLen := 2*window + 1
	if arcLen < 3 {
		arcLen = 3
	}
	var segments []Segment
	segNo := 1
	for _, path := range paths {
		if len(path) <= arcLen+2 {
			segments = append(segments, appendGeometrySegment(fmt.Sprintf("seg-%03d", segNo), path))
			segNo++
			continue
		}
		for start := 0; start < len(path); start += arcLen {
			end := minInt(len(path), start+arcLen)
			if len(path)-end > 0 && len(path)-end < 3 {
				end = len(path)
			}
			if end-start < 2 {
				continue
			}
			segments = append(segments, appendGeometrySegment(fmt.Sprintf("seg-%03d", segNo), path[start:end]))
			segNo++
		}
	}
	sort.Slice(segments, func(i, j int) bool {
		if segments[i].StartY != segments[j].StartY {
			return segments[i].StartY < segments[j].StartY
		}
		return segments[i].StartX < segments[j].StartX
	})
	for i := range segments {
		segments[i].ID = fmt.Sprintf("seg-%03d", i+1)
	}
	return segments
}

func appendGeometrySegment(id string, pts []imagePoint) Segment {
	seg := Segment{ID: id, Points: append([]imagePoint(nil), pts...)}
	if len(pts) > 0 {
		seg.StartX, seg.StartY = pts[0].x, pts[0].y
	}
	curvQ, radQ, curvFloat := curvatureOfPoints(pts, DefaultParams())
	seg.CurvatureQ = curvQ
	seg.CurvatureRadiusQ = radQ
	seg.Curvature = curvFloat
	return seg
}

func curvatureOfPoints(pts []imagePoint, p Params) (int64, int64, float64) {
	if len(pts) < 3 {
		curvQ := p.FixedScale / int64(maxInt(1, p.RadiusMin))
		return curvQ, p.FixedScale * int64(p.RadiusMin), float64(curvQ) / float64(p.FixedScale)
	}
	triples := [][3]imagePoint{{pts[0], pts[len(pts)/2], pts[len(pts)-1]}}
	if len(pts) >= 5 {
		w := minInt(DefaultParams().CurvatureWindow, len(pts)/2-1)
		if w >= 1 {
			triples = append(triples, [3]imagePoint{pts[0], pts[w], pts[2*w]})
		}
	}
	for _, t := range triples {
		p0, p1, p2 := t[0], t[1], t[2]
		area2 := absInt64(int64(p1.x-p0.x)*int64(p2.y-p0.y) - int64(p1.y-p0.y)*int64(p2.x-p0.x))
		d01 := distSq64(p0, p1)
		d12 := distSq64(p1, p2)
		d02 := distSq64(p0, p2)
		den := isqrt64(d01 * d12 * d02)
		if den == 0 {
			continue
		}
		num := 2 * area2 * p.FixedScale
		curvQ := num / den
		if curvQ == 0 {
			return 0, -1, 0
		}
		radQ := p.FixedScale * p.FixedScale / curvQ
		return curvQ, radQ, float64(curvQ) / float64(p.FixedScale)
	}
	curvQ := p.FixedScale / int64(maxInt(1, p.RadiusMin))
	return curvQ, p.FixedScale * int64(p.RadiusMin), float64(curvQ) / float64(p.FixedScale)
}

func distSq64(a, b imagePoint) int64 {
	dx := int64(a.X() - b.X())
	dy := int64(a.Y() - b.Y())
	return dx*dx + dy*dy
}

func (p imagePoint) X() int { return p.x }
func (p imagePoint) Y() int { return p.y }

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func MapSegmentRadii(segments []Segment, p Params) ([]RadiusDecision, error) {
	var maxCurv int64
	for _, s := range segments {
		if s.CurvatureQ < 0 {
			return nil, &QAError{Code: ERadius, Field: "curvature", ObjectID: s.ID, Message: "negative fixed-point curvature", Actual: s.Curvature}
		}
		if s.CurvatureQ > maxCurv {
			maxCurv = s.CurvatureQ
		}
	}
	out := make([]RadiusDecision, 0, len(segments))
	rankOrder := make([]int, len(segments))
	for i := range segments {
		rankOrder[i] = i
	}
	sort.SliceStable(rankOrder, func(i, j int) bool {
		a, b := segments[rankOrder[i]], segments[rankOrder[j]]
		if a.CurvatureQ != b.CurvatureQ {
			return a.CurvatureQ < b.CurvatureQ
		}
		return a.ID < b.ID
	})
	binByIndex := make([]int, len(segments))
	for rank, idx := range rankOrder {
		if len(segments) == 1 {
			binByIndex[idx] = 0
		} else {
			binByIndex[idx] = minInt(p.CurvatureBins-1, rank*p.CurvatureBins/len(segments))
		}
	}
	for i, s := range segments {
		d := RadiusDecision{SegmentID: s.ID, PointCount: len(s.Points), Curvature: s.Curvature, CurvatureQ: s.CurvatureQ, CurvatureRadiusQ: s.CurvatureRadiusQ}
		if len(segments) == 0 {
			return out, nil
		}
		d.Bin = binByIndex[i]
		span := p.RadiusMax - p.RadiusMin
		radius := p.RadiusMax
		var mapMicro int64
		if p.CurvatureBins <= 1 || span == 0 {
			radius = p.RadiusMin + span/2
			if span == 0 {
				radius = p.RadiusMin
			}
		} else {
			num := int64(d.Bin * span)
			den := int64(p.CurvatureBins - 1)
			decrement := (num + den - 1) / den
			rem := num % den
			radius = p.RadiusMax - int(decrement)
			mapMicro = rem * p.TraceScale / den
		}
		if radius < p.RadiusMin || radius > p.RadiusMax {
			return nil, &QAError{Code: ERadius, Field: "radius", ObjectID: s.ID, Message: "mapped radius outside declared band", Actual: radius, Min: p.RadiusMin, Max: p.RadiusMax, Detail: map[string]any{"curvature": s.Curvature}}
		}
		d.ErodeRadius = radius
		d.DilateRadius = radius
		d.TruncationMicro = mapMicro
		d.MappingBasis = fmt.Sprintf("bin=%d/%d max_curvature_q=%d radius=%d monotonic=high-curvature-smaller", d.Bin+1, p.CurvatureBins, maxCurv, radius)
		out = append(out, d)
	}
	return out, nil
}

func MapRadiusByCurvature(curvature float64, segmentID string, p Params) (int, int, error) {
	if math.IsNaN(curvature) || math.IsInf(curvature, 0) || curvature < 0 {
		return 0, 0, &QAError{Code: ERadius, Field: "curvature", ObjectID: segmentID, Message: "measured curvature cannot map to integer radius", Actual: curvature}
	}
	q := int64(curvature * float64(p.FixedScale))
	seg := Segment{ID: segmentID, CurvatureQ: q, Curvature: curvature}
	ds, err := MapSegmentRadii([]Segment{seg}, p)
	if err != nil {
		return 0, 0, err
	}
	return ds[0].ErodeRadius, ds[0].DilateRadius, nil
}
