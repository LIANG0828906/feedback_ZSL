package vectorcore

import "fmt"

// BuildStrokeBand creates overlapping closed rings for segment quads and
// vertex joins. Rings are normalized to the same winding direction so the
// scanline nonzero rule computes their union.
func BuildStrokeBand(poly Polyline, width int64, join JoinMode) ([]Polyline, error) {
	if width <= 0 {
		return nil, &Error{Code: CodeParam, Op: "stroke", Field: "strokeWidth", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("must be positive")}
	}
	if join != JoinRound && join != JoinBevel && join != JoinMiter {
		return nil, &Error{Code: CodeParam, Op: "stroke", Field: "strokeJoin", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("unsupported join %q", join)}
	}
	pts := poly.Points
	if len(pts) > 1 && pts[0] == pts[len(pts)-1] {
		pts = pts[:len(pts)-1]
	}
	if len(pts) < 2 {
		return nil, nil
	}
	half := width / 2
	if half < 1 {
		half = 1
	}
	type normal struct {
		x, y int64
	}
	norms := make([]normal, len(pts))
	for i := range pts {
		a, b := pts[i], pts[(i+1)%len(pts)]
		dx, dy := b.X-a.X, b.Y-a.Y
		if dx == 0 && dy == 0 {
			continue
		}
		length := distance(a, b)
		if length == 0 {
			continue
		}
		norms[i] = normal{x: divRoundInt(-dy*half, length), y: divRoundInt(dx*half, length)}
	}
	var rings []Polyline
	for i := range pts {
		a, b := pts[i], pts[(i+1)%len(pts)]
		n := norms[i]
		if n.x == 0 && n.y == 0 {
			continue
		}
		ring := []Point{
			{X: a.X + n.x, Y: a.Y + n.y},
			{X: b.X + n.x, Y: b.Y + n.y},
			{X: b.X - n.x, Y: b.Y - n.y},
			{X: a.X - n.x, Y: a.Y - n.y},
		}
		rings = append(rings, closedRing(ring))
	}
	for i := range pts {
		prev := (i - 1 + len(pts)) % len(pts)
		np, nn := norms[prev], norms[i]
		if (np.x == 0 && np.y == 0) || (nn.x == 0 && nn.y == 0) {
			continue
		}
		p := pts[i]
		switch join {
		case JoinRound:
			rings = append(rings, circleRing(p, half, 8))
		case JoinBevel:
			rings = append(rings, closedRing([]Point{
				p, {X: p.X + np.x, Y: p.Y + np.y}, {X: p.X + nn.x, Y: p.Y + nn.y},
			}))
			rings = append(rings, closedRing([]Point{
				p, {X: p.X - np.x, Y: p.Y - np.y}, {X: p.X - nn.x, Y: p.Y - nn.y},
			}))
		case JoinMiter:
			rings = append(rings, miterRings(p, np, nn, half)...)
		}
	}
	return rings, nil
}

func closedRing(points []Point) Polyline {
	cp := append([]Point(nil), points...)
	if len(cp) > 1 && cp[0] == cp[len(cp)-1] {
		cp = cp[:len(cp)-1]
	}
	area := signedPolygonArea(cp)
	if area < 0 {
		for i, j := 0, len(cp)-1; i < j; i, j = i+1, j-1 {
			cp[i], cp[j] = cp[j], cp[i]
		}
	}
	return Polyline{Points: cp, Closed: true}
}

func signedPolygonArea(points []Point) int64 {
	var sum int64
	for i := range points {
		j := (i + 1) % len(points)
		sum += points[i].X*points[j].Y - points[j].X*points[i].Y
	}
	return sum
}

func circleRing(center Point, radius int64, divisions int) Polyline {
	// A deterministic 16-direction table supports 4, 8 and 16-gons without
	// trigonometry and keeps all coordinates integral.
	unit := [][2]int64{
		{1024, 0}, {946, 392}, {724, 724}, {392, 946},
		{0, 1024}, {-392, 946}, {-724, 724}, {-946, 392},
		{-1024, 0}, {-946, -392}, {-724, -724}, {-392, -946},
		{0, -1024}, {392, -946}, {724, -724}, {946, -392},
	}
	if divisions != 4 && divisions != 8 && divisions != 16 {
		divisions = 8
	}
	pts := make([]Point, 0, divisions)
	step := 16 / divisions
	for i := 0; i < divisions; i++ {
		rx := radius * unit[i*step][0] / 1024
		ry := radius * unit[i*step][1] / 1024
		pts = append(pts, Point{X: center.X + rx, Y: center.Y + ry})
	}
	return closedRing(pts)
}

func miterRings(p Point, np, nn struct{ x, y int64 }, half int64) []Polyline {
	var rings []Polyline
	for _, sign := range []int64{1, -1} {
		a := Point{X: p.X + sign*np.x, Y: p.Y + sign*np.y}
		b := Point{X: p.X + sign*nn.x, Y: p.Y + sign*nn.y}
		// Intersect a + t*(np) and b + u*(nn).
		rx, ry := a.X-p.X, a.Y-p.Y
		sx, sy := b.X-p.X, b.Y-p.Y
		den := np.x*nn.y - np.y*nn.x
		if den == 0 {
			rings = append(rings, closedRing([]Point{p, a, b}))
			continue
		}
		tNum := (sx-rx)*nn.y - (sy-ry)*nn.x
		t := divRoundInt(tNum, den)
		mx, my := p.X+rx+t*np.x, p.Y+ry+t*np.y
		dx, dy := mx-p.X, my-p.Y
		if dx*dx+dy*dy > (4*half)*(4*half) {
			rings = append(rings, closedRing([]Point{p, a, b}))
			continue
		}
		rings = append(rings, closedRing([]Point{p, a, {X: mx, Y: my}, b}))
	}
	return rings
}
