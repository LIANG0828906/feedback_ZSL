package morph

import (
	"math"
)

// EvalSegment evaluates a quadratic or cubic Bezier at t in [0,1].
func EvalSegment(s Segment, t float64) Vec2 {
	if s.Kind == "C" && s.P3 != nil {
		u := 1 - t
		a, b, c, d := u*u*u, 3*u*u*t, 3*u*t*t, t*t*t
		return Vec2{
			X: a*s.P0.X + b*s.P1.X + c*s.P2.X + d*s.P3.X,
			Y: a*s.P0.Y + b*s.P1.Y + c*s.P2.Y + d*s.P3.Y,
		}
	}
	u := 1 - t
	a, b, c := u*u, 2*u*t, t*t
	return Vec2{
		X: a*s.P0.X + b*s.P1.X + c*s.P2.X,
		Y: a*s.P0.Y + b*s.P1.Y + c*s.P2.Y,
	}
}

// SegmentBBox returns the tight axis-aligned bounds of a Bezier segment.
func SegmentBBox(s Segment) BBox {
	b := EmptyBBox().Include(s.P0).Include(s.End())
	pts := s.Points()
	for axis := 0; axis < 2; axis++ {
		for _, t := range bezierExtrema(pts, axis) {
			if t > 0 && t < 1 {
				b = b.Include(EvalSegment(s, t))
			}
		}
	}
	return b
}

func bezierExtrema(pts []Vec2, axis int) []float64 {
	coord := func(i int) float64 {
		if axis == 0 {
			return pts[i].X
		}
		return pts[i].Y
	}
	// Derivative of a degree-n Bezier in power basis:
	// n * sum_{i=0}^{n-1} (P[i+1]-P[i]) B_{i,n-1}(t).
	n := len(pts) - 1
	if n == 2 {
		a := coord(0) - 2*coord(1) + coord(2)
		b := -2*coord(0) + 2*coord(1)
		if math.Abs(a) < 1e-14 {
			if math.Abs(b) > 1e-14 {
				return []float64{-b / (2 * a)}
			}
			return nil
		}
		// d/dt = 2[(1-t)(P1-P0) + t(P2-P1)]
		d0 := coord(1) - coord(0)
		d1 := coord(2) - coord(1)
		den := d0 - d1
		if math.Abs(den) < 1e-14 {
			return nil
		}
		return []float64{d0 / den}
	}
	if n == 3 {
		// d/dt is quadratic: a t^2 + b t + c.
		p0, p1, p2, p3 := coord(0), coord(1), coord(2), coord(3)
		a := -p0 + 3*p1 - 3*p2 + p3
		b := 2 * (p0 - 2*p1 + p2)
		c := p1 - p0
		if math.Abs(a) < 1e-14 {
			if math.Abs(b) < 1e-14 {
				return nil
			}
			return []float64{-c / b}
		}
		disc := b*b - 4*a*c
		if disc < 0 {
			return nil
		}
		sqrt := math.Sqrt(disc)
		return []float64{(-b - sqrt) / (2 * a), (-b + sqrt) / (2 * a)}
	}
	return nil
}

// SegmentArea returns the signed Green's-theorem contribution of one segment.
func SegmentArea(s Segment) float64 {
	xs := bezierPowerCoeffs(s, 0)
	ys := bezierPowerCoeffs(s, 1)
	dx := polyDerivative(xs)
	dy := polyDerivative(ys)
	// x*dy - y*dx
	p := polyMul(xs, dy)
	q := polyMul(ys, dx)
	degree := maxInt(len(p), len(q))
	integral := 0.0
	for i := 0; i < degree; i++ {
		var a, b float64
		if i < len(p) {
			a = p[i]
		}
		if i < len(q) {
			b = q[i]
		}
		integral += (a - b) / float64(i+1)
	}
	return 0.5 * integral
}

func bezierPowerCoeffs(s Segment, axis int) []float64 {
	pts := s.Points()
	c := func(i int) float64 {
		if axis == 0 {
			return pts[i].X
		}
		return pts[i].Y
	}
	if s.Kind == "C" && s.P3 != nil {
		p0, p1, p2, p3 := c(0), c(1), c(2), c(3)
		return []float64{
			p0,
			3 * (p1 - p0),
			3 * (p0 - 2*p1 + p2),
			-p0 + 3*p1 - 3*p2 + p3,
		}
	}
	p0, p1, p2 := c(0), c(1), c(2)
	return []float64{p0, 2 * (p1 - p0), p0 - 2*p1 + p2}
}

func polyDerivative(p []float64) []float64 {
	if len(p) <= 1 {
		return []float64{0}
	}
	out := make([]float64, len(p)-1)
	for i := 1; i < len(p); i++ {
		out[i-1] = p[i] * float64(i)
	}
	return out
}

func polyMul(a, b []float64) []float64 {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	out := make([]float64, len(a)+len(b)-1)
	for i, av := range a {
		for j, bv := range b {
			out[i+j] += av * bv
		}
	}
	return out
}

// ContourArea returns the signed contour area. For a closed CCW contour the
// result is positive.
func ContourArea(segments []Segment) float64 {
	a := 0.0
	for _, s := range segments {
		a += SegmentArea(s)
	}
	return a
}

// SegmentsBBox computes the exact bounds of all Bezier segments.
func SegmentsBBox(segments []Segment) BBox {
	b := EmptyBBox()
	for _, s := range segments {
		b = b.Union(SegmentBBox(s))
	}
	return b
}

func PolylineSignedArea(pts []Vec2) float64 {
	if len(pts) < 3 {
		return 0
	}
	a := 0.0
	for i := range pts {
		j := (i + 1) % len(pts)
		a += pts[i].X*pts[j].Y - pts[j].X*pts[i].Y
	}
	return 0.5 * a
}

func PolylineLength(pts []Vec2) float64 {
	l := 0.0
	for i := 0; i+1 < len(pts); i++ {
		l += pts[i].Distance(pts[i+1])
	}
	return l
}

func Quantize(v float64, quantum float64) float64 {
	if quantum <= 0 {
		return v
	}
	return math.Round(v/quantum) * quantum
}

func QuantizeVec(v Vec2, quantum float64) Vec2 {
	return Vec2{Quantize(v.X, quantum), Quantize(v.Y, quantum)}
}
