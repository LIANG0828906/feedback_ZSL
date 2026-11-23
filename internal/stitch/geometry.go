package stitch

import (
	"math"

	"stitchworkbench/internal/model"
)

type poly []model.Point

func quad(w, h int, t model.Matrix3) []model.Point {
	in := []model.Point{
		{X: 0, Y: 0},
		{X: float64(w), Y: 0},
		{X: float64(w), Y: float64(h)},
		{X: 0, Y: float64(h)},
	}
	out := make([]model.Point, 4)
	for i, p := range in {
		out[i].X, out[i].Y = t.Apply(p.X, p.Y)
	}
	return out
}

func polygonArea(p []model.Point) float64 {
	if len(p) < 3 {
		return 0
	}
	s := 0.0
	for i := range p {
		j := (i + 1) % len(p)
		s += p[i].X*p[j].Y - p[j].X*p[i].Y
	}
	return math.Abs(s) / 2
}

func clipPolygon(subject, clip []model.Point) []model.Point {
	out := append([]model.Point(nil), subject...)
	if len(out) == 0 {
		return nil
	}
	ccw := signedArea(clip) > 0
	for i := range clip {
		a := clip[i]
		b := clip[(i+1)%len(clip)]
		in := make([]model.Point, 0, len(out)+2)
		for j := range out {
			cur := out[j]
			prev := out[(j+len(out)-1)%len(out)]
			curIn := inside(cur, a, b, ccw)
			prevIn := inside(prev, a, b, ccw)
			if curIn {
				if !prevIn {
					if p, ok := lineIntersect(prev, cur, a, b); ok {
						in = append(in, p)
					}
				}
				in = append(in, cur)
			} else if prevIn {
				if p, ok := lineIntersect(prev, cur, a, b); ok {
					in = append(in, p)
				}
			}
		}
		out = in
		if len(out) == 0 {
			break
		}
	}
	return out
}

func signedArea(p []model.Point) float64 {
	s := 0.0
	for i := range p {
		j := (i + 1) % len(p)
		s += p[i].X*p[j].Y - p[j].X*p[i].Y
	}
	return s / 2
}

func inside(p, a, b model.Point, ccw bool) bool {
	cross := (b.X-a.X)*(p.Y-a.Y) - (b.Y-a.Y)*(p.X-a.X)
	if ccw {
		return cross >= -1e-9
	}
	return cross <= 1e-9
}

func lineIntersect(p1, p2, p3, p4 model.Point) (model.Point, bool) {
	d1x, d1y := p2.X-p1.X, p2.Y-p1.Y
	d2x, d2y := p4.X-p3.X, p4.Y-p3.Y
	den := d1x*d2y - d1y*d2x
	if math.Abs(den) < 1e-12 {
		return model.Point{}, false
	}
	t := ((p3.X-p1.X)*d2y - (p3.Y-p1.Y)*d2x) / den
	return model.Point{X: p1.X + t*d1x, Y: p1.Y + t*d1y}, true
}

func canvasBounds(images []*imageWork) (model.Canvas, model.Point) {
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	for _, im := range images {
		if !im.accepted {
			continue
		}
		for _, p := range quad(im.image.Width, im.image.Height, im.transform) {
			if p.X < minX {
				minX = p.X
			}
			if p.Y < minY {
				minY = p.Y
			}
			if p.X > maxX {
				maxX = p.X
			}
			if p.Y > maxY {
				maxY = p.Y
			}
		}
	}
	if math.IsInf(minX, 1) {
		return model.Canvas{}, model.Point{}
	}
	return model.Canvas{
		Width:  int(math.Ceil(maxX) - math.Floor(minX)),
		Height: int(math.Ceil(maxY) - math.Floor(minY)),
	}, model.Point{X: math.Floor(minX), Y: math.Floor(minY)}
}

func overlapCoverage(im *imageWork, all []*imageWork) float64 {
	src := quad(im.image.Width, im.image.Height, im.transform)
	a0 := polygonArea(src)
	if a0 <= 1e-9 {
		return 0
	}
	overlap := 0.0
	for _, other := range all {
		if other == nil || other.index == im.index || !other.accepted {
			continue
		}
		inter := clipPolygon(src, quad(other.image.Width, other.image.Height, other.transform))
		overlap += polygonArea(inter)
	}
	if overlap > a0 {
		overlap = a0
	}
	return overlap / a0
}
