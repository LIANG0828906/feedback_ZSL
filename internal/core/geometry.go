package core

import (
	"math"
	"sort"
)

func ValidBox(box Box) bool {
	for _, p := range box {
		if !p.Valid() {
			return false
		}
	}
	area := PolygonArea(box[:])
	if area < 4 {
		return false
	}
	// A degenerate quadrilateral has a zero-length edge.
	for i := 0; i < 4; i++ {
		a := box[i]
		b := box[(i+1)%4]
		if math.Hypot(a.X-b.X, a.Y-b.Y) < 1 {
			return false
		}
	}
	return true
}

func BoxBounds(box Box) Rect {
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	for _, p := range box {
		minX = math.Min(minX, p.X)
		minY = math.Min(minY, p.Y)
		maxX = math.Max(maxX, p.X)
		maxY = math.Max(maxY, p.Y)
	}
	x0, y0 := int(math.Floor(minX)), int(math.Floor(minY))
	x1, y1 := int(math.Ceil(maxX)), int(math.Ceil(maxY))
	return Rect{X: x0, Y: y0, Width: maxInt(0, x1-x0), Height: maxInt(0, y1-y0)}
}

func AxisBox(r Rect) Box {
	return Box{
		{X: float64(r.X), Y: float64(r.Y)},
		{X: float64(r.Right()), Y: float64(r.Y)},
		{X: float64(r.Right()), Y: float64(r.Bottom())},
		{X: float64(r.X), Y: float64(r.Bottom())},
	}
}

func RotatedRectBox(cx, cy, width, height, angle float64) Box {
	u := Point{X: math.Cos(angle), Y: math.Sin(angle)}
	v := Point{X: -math.Sin(angle), Y: math.Cos(angle)}
	hw, hh := width/2, height/2
	return Box{
		{X: cx - u.X*hw - v.X*hh, Y: cy - u.Y*hw - v.Y*hh},
		{X: cx + u.X*hw - v.X*hh, Y: cy + u.Y*hw - v.Y*hh},
		{X: cx + u.X*hw + v.X*hh, Y: cy + u.Y*hw + v.Y*hh},
		{X: cx - u.X*hw + v.X*hh, Y: cy - u.Y*hw + v.Y*hh},
	}
}

// ProjectBox returns a tight oriented bounding box around points. The angle is
// the line direction, with local Y pointing down-right of that direction.
func ProjectBox(points []Point, angle float64) Box {
	if len(points) == 0 {
		return Box{}
	}
	u := Point{X: math.Cos(angle), Y: math.Sin(angle)}
	v := Point{X: -math.Sin(angle), Y: math.Cos(angle)}
	minU, maxU := math.Inf(1), math.Inf(-1)
	minV, maxV := math.Inf(1), math.Inf(-1)
	for _, p := range points {
		pu := p.X*u.X + p.Y*u.Y
		pv := p.X*v.X + p.Y*v.Y
		minU, maxU = math.Min(minU, pu), math.Max(maxU, pu)
		minV, maxV = math.Min(minV, pv), math.Max(maxV, pv)
	}
	centerU := (minU + maxU) / 2
	centerV := (minV + maxV) / 2
	cx := centerU*u.X + centerV*v.X
	cy := centerU*u.Y + centerV*v.Y
	hw, hh := (maxU-minU)/2, (maxV-minV)/2
	return Box{
		{X: cx - u.X*hw - v.X*hh, Y: cy - u.Y*hw - v.Y*hh},
		{X: cx + u.X*hw - v.X*hh, Y: cy + u.Y*hw - v.Y*hh},
		{X: cx + u.X*hw + v.X*hh, Y: cy + u.Y*hw + v.Y*hh},
		{X: cx - u.X*hw + v.X*hh, Y: cy - u.Y*hw + v.Y*hh},
	}
}

func BoxCenter(box Box) Point {
	var c Point
	for _, p := range box {
		c.X += p.X / 4
		c.Y += p.Y / 4
	}
	return c
}

func PolygonArea(poly []Point) float64 {
	if len(poly) < 3 {
		return 0
	}
	sum := 0.0
	for i := range poly {
		j := (i + 1) % len(poly)
		sum += poly[i].X*poly[j].Y - poly[j].X*poly[i].Y
	}
	return math.Abs(sum) / 2
}

func BoxArea(box Box) float64 { return PolygonArea(box[:]) }

func BoxIoU(a, b Box) float64 {
	inter := polygonIntersection(a[:], b[:])
	areaInter := PolygonArea(inter)
	union := BoxArea(a) + BoxArea(b) - areaInter
	if union <= 0 {
		return 0
	}
	return areaInter / union
}

func BoxIntersectionArea(a, b Box) float64 {
	return PolygonArea(polygonIntersection(a[:], b[:]))
}

// polygonIntersection clips one convex polygon by each edge of another.
// Boxes in this project are convex and ordered consistently.
func polygonIntersection(subject, clip []Point) []Point {
	output := append([]Point(nil), subject...)
	for i := 0; i < len(clip) && len(output) > 0; i++ {
		a := clip[i]
		b := clip[(i+1)%len(clip)]
		input := output
		output = nil
		s := input[len(input)-1]
		for _, e := range input {
			eInside := insideEdge(e, a, b)
			sInside := insideEdge(s, a, b)
			if eInside {
				if !sInside {
					output = append(output, lineIntersection(s, e, a, b))
				}
				output = append(output, e)
			} else if sInside {
				output = append(output, lineIntersection(s, e, a, b))
			}
			s = e
		}
	}
	return output
}

func insideEdge(p, a, b Point) bool {
	// Box vertices are clockwise in screen coordinates, so a point is inside
	// when it is on the right side of each directed edge.
	return (b.X-a.X)*(p.Y-a.Y)-(b.Y-a.Y)*(p.X-a.X) >= -1e-9
}

func lineIntersection(p1, p2, p3, p4 Point) Point {
	x1, y1 := p1.X, p1.Y
	x2, y2 := p2.X, p2.Y
	x3, y3 := p3.X, p3.Y
	x4, y4 := p4.X, p4.Y
	den := (x1-x2)*(y3-y4) - (y1-y2)*(x3-x4)
	if math.Abs(den) < 1e-12 {
		return p2
	}
	t := ((x1-x3)*(y3-y4) - (y1-y3)*(x3-x4)) / den
	return Point{X: x1 + t*(x2-x1), Y: y1 + t*(y2-y1)}
}

func BoxUnionBox(boxes []Box) Box {
	var points []Point
	for _, box := range boxes {
		points = append(points, box[:]...)
	}
	if len(points) == 0 {
		return Box{}
	}
	return ProjectBox(points, 0)
}

func Clamp01(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return math.Max(0, math.Min(1, v))
}

func SortedUniqueInts(values []int) []int {
	if len(values) == 0 {
		return nil
	}
	copyValues := append([]int(nil), values...)
	sort.Ints(copyValues)
	out := copyValues[:0]
	for _, value := range copyValues {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}
