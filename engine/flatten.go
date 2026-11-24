package engine

import (
	"fmt"
	"math"
)

type FlattenedPath struct {
	ID           string `json:"id"`
	Points       []Vec2 `json:"points"`
	SegmentCount int    `json:"segmentCount"`
	Bounds       Rect   `json:"bounds"`
	Tolerance    int64  `json:"tolerance"`
}

// FlattenContour converts all curves into a closed polyline.
func FlattenContour(segments []Segment, tolerance int64) ([]Vec2, error) {
	if tolerance < 1 {
		return nil, &Error{Code: CodePara, Path: "tolerance", Field: "tolerance", Message: "tolerance must be positive"}
	}
	if len(segments) == 0 {
		return nil, &Error{Code: CodeSelf, Path: "segments", Message: "empty contour"}
	}
	points := make([]Vec2, 0, len(segments)*8)
	points = append(points, segments[0].Start)
	for i, seg := range segments {
		if err := flattenSegment(seg, tolerance, 0, 18, &points); err != nil {
			return nil, &Error{Code: CodeSelf, Path: fmt.Sprintf("segments[%d]", i), Message: err.Error()}
		}
	}
	if len(points) > 1 && points[len(points)-1] == points[0] {
		points = points[:len(points)-1]
	}
	return points, nil
}

func flattenSegment(seg Segment, tolerance int64, depth, maxDepth int, out *[]Vec2) error {
	flat := false
	switch seg.Kind {
	case Quadratic:
		flat = pointLineDistance(seg.C1, seg.Start, seg.End) <= float64(tolerance)
	case Cubic:
		d1 := pointLineDistance(seg.C1, seg.Start, seg.End)
		d2 := pointLineDistance(seg.C2, seg.Start, seg.End)
		flat = math.Max(d1, d2) <= float64(tolerance)
	default:
		return fmt.Errorf("unsupported segment kind %q", seg.Kind)
	}
	if flat || depth >= maxDepth {
		*out = append(*out, seg.End)
		return nil
	}
	left, right := splitSegment(seg)
	if err := flattenSegment(left, tolerance, depth+1, maxDepth, out); err != nil {
		return err
	}
	return flattenSegment(right, tolerance, depth+1, maxDepth, out)
}

func splitSegment(seg Segment) (Segment, Segment) {
	mid := func(a, b Vec2) Vec2 {
		return Vec2{X: (a.X + b.X) / 2, Y: (a.Y + b.Y) / 2}
	}
	if seg.Kind == Quadratic {
		p01 := mid(seg.Start, seg.C1)
		p12 := mid(seg.C1, seg.End)
		p012 := mid(p01, p12)
		return Segment{Kind: Quadratic, Start: seg.Start, C1: p01, End: p012},
			Segment{Kind: Quadratic, Start: p012, C1: p12, End: seg.End}
	}
	p01 := mid(seg.Start, seg.C1)
	p12 := mid(seg.C1, seg.C2)
	p23 := mid(seg.C2, seg.End)
	p012 := mid(p01, p12)
	p123 := mid(p12, p23)
	p0123 := mid(p012, p123)
	return Segment{Kind: Cubic, Start: seg.Start, C1: p01, C2: p012, End: p0123},
		Segment{Kind: Cubic, Start: p0123, C1: p123, C2: p23, End: seg.End}
}

func pointLineDistance(p, a, b Vec2) float64 {
	dx := float64(b.X - a.X)
	dy := float64(b.Y - a.Y)
	if dx == 0 && dy == 0 {
		return math.Hypot(float64(p.X-a.X), float64(p.Y-a.Y))
	}
	cross := math.Abs(float64(p.X-a.X)*dy - float64(p.Y-a.Y)*dx)
	return cross / math.Hypot(dx, dy)
}

// FlattenAt evaluates and flattens every path at an internal timestamp.
func FlattenAt(scene *Scene, time int64) ([]FlattenedPath, error) {
	if scene == nil {
		return nil, &Error{Code: CodeSchema, Path: "$", Message: "nil scene"}
	}
	if err := ValidateParameters(scene); err != nil {
		return nil, err
	}
	out := make([]FlattenedPath, 0, len(scene.Paths))
	for i := range scene.Paths {
		p := &scene.Paths[i]
		segs, err := EvalPathAt(p, time, scene.Interpolation)
		if err != nil {
			return nil, err
		}
		pts, err := FlattenContour(segs, scene.Tolerance)
		if err != nil {
			return nil, err
		}
		r := BoundsOfPoints(pts)
		out = append(out, FlattenedPath{
			ID:           p.ID,
			Points:       pts,
			SegmentCount: len(pts),
			Bounds:       r,
			Tolerance:    scene.Tolerance,
		})
	}
	return out, nil
}

// PolylineArea returns twice the signed area, rounded to an integer squared Q10 unit.
func PolylineArea(points []Vec2) int64 {
	if len(points) < 3 {
		return 0
	}
	var twice int64
	for i := range points {
		a, b := points[i], points[(i+1)%len(points)]
		twice += a.X*b.Y - b.X*a.Y
	}
	return divRound(twice, 2, RoundNearest)
}

func polylinePerimeter(points []Vec2) float64 {
	if len(points) < 2 {
		return 0
	}
	var length float64
	for i := range points {
		a, b := points[i], points[(i+1)%len(points)]
		length += math.Hypot(float64(b.X-a.X), float64(b.Y-a.Y))
	}
	return length
}
