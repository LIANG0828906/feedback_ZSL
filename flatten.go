package vectorcore

import (
	"fmt"
	"math/big"
)

type Polyline struct {
	Points []Point `json:"points"`
	Closed bool    `json:"closed"`
}

func (p Polyline) Clone() Polyline {
	return Polyline{Points: append([]Point(nil), p.Points...), Closed: p.Closed}
}

// FlattenContour returns a closed polyline. Segment counts are derived from
// the control polygon length and tolerance, so tightening tolerance cannot
// reduce the number of emitted segments.
func FlattenContour(contour Contour, tolerance int64) (Polyline, error) {
	if tolerance < 1 || tolerance > 1<<20 {
		return Polyline{}, &Error{Code: CodeParam, Op: "flatten", Field: "tolerance", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("must be in [1,%d]", 1<<20)}
	}
	if len(contour.Segments) == 0 {
		return Polyline{}, &Error{Code: CodeSchema, Op: "flatten", Field: "contour.segments", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("contour has no segments")}
	}
	points := []Point{contour.Start}
	cur := contour.Start
	for si, seg := range contour.Segments {
		if seg.Kind != SegmentQuad && seg.Kind != SegmentCubic {
			return Polyline{}, &Error{Code: CodeSchema, Op: "flatten", Field: fmt.Sprintf("contour.segments[%d].kind", si), Keyframe: -1, TimeIndex: -1}
		}
		length := segmentControlLength(cur, seg)
		ratio := uint64((length + tolerance - 1) / tolerance)
		divisions := isqrt64(ratio)
		if uint64(divisions*divisions) < ratio {
			divisions++
		}
		if divisions < 1 {
			divisions = 1
		}
		if divisions > 1024 {
			divisions = 1024
		}
		for i := int64(1); i <= divisions; i++ {
			points = append(points, evalSegment(seg, cur, int(i), int(divisions)))
		}
		cur = seg.To
	}
	if len(points) == 0 || points[len(points)-1] != contour.Start {
		points = append(points, contour.Start)
	}
	return Polyline{Points: points, Closed: true}, nil
}

func segmentControlLength(start Point, seg Segment) int64 {
	if seg.Kind == SegmentQuad {
		return distance(start, seg.C1) + distance(seg.C1, seg.To)
	}
	return distance(start, seg.C1) + distance(seg.C1, seg.C2) + distance(seg.C2, seg.To)
}

func distance(a, b Point) int64 {
	dx := b.X - a.X
	dy := b.Y - a.Y
	if dx < 0 {
		dx = -dx
	}
	if dy < 0 {
		dy = -dy
	}
	return isqrt64(uint64(dx*dx + dy*dy))
}

func isqrt64(v uint64) int64 {
	if v == 0 {
		return 0
	}
	x := v
	y := (x + 1) / 2
	for y < x {
		x = y
		y = (x + v/x) / 2
	}
	if x > uint64(^uint64(0)>>1) {
		return int64(^uint64(0) >> 1)
	}
	return int64(x)
}

// PolygonArea returns the absolute signed area of a polygonal contour.
func PolygonArea(points []Point) (int64, error) {
	if len(points) < 3 {
		return 0, nil
	}
	sum := new(big.Int)
	for i := range points {
		j := (i + 1) % len(points)
		ax, ay := big.NewInt(points[i].X), big.NewInt(points[i].Y)
		bx, by := big.NewInt(points[j].X), big.NewInt(points[j].Y)
		sum.Add(sum, new(big.Int).Sub(new(big.Int).Mul(ax, by), new(big.Int).Mul(bx, ay)))
	}
	num := new(big.Int).Abs(sum)
	return ratToInt64(new(big.Rat).SetFrac(num, big.NewInt(2)))
}

// ContourAreaExact integrates x*dy - y*dx over the Bezier segments using
// exact rational polynomial arithmetic.
func ContourAreaExact(contour Contour) (int64, error) {
	if len(contour.Segments) == 0 {
		return 0, nil
	}
	total := new(big.Rat)
	start := contour.Start
	for _, seg := range contour.Segments {
		x, y := bezierPolys(start, seg)
		dx := derivativePoly(x)
		dy := derivativePoly(y)
		first := polyMul(x, dy)
		second := polyMul(y, dx)
		for i := 0; i < len(first) || i < len(second); i++ {
			var c *big.Rat
			if i < len(first) {
				c = new(big.Rat).Set(first[i])
			} else {
				c = new(big.Rat)
			}
			if i < len(second) {
				c.Sub(c, second[i])
			}
			c.Quo(c, big.NewRat(int64(i+1), 1))
			total.Add(total, c)
		}
		start = seg.To
	}
	if start != contour.Start {
		cross := new(big.Int).Sub(
			new(big.Int).Mul(big.NewInt(start.X), big.NewInt(contour.Start.Y)),
			new(big.Int).Mul(big.NewInt(contour.Start.X), big.NewInt(start.Y)),
		)
		total.Add(total, new(big.Rat).SetInt(cross))
	}
	total.Abs(total)
	total.Quo(total, big.NewRat(2, 1))
	return ratToInt64(total)
}

func bezierPolys(start Point, seg Segment) ([]*big.Rat, []*big.Rat) {
	p0x, p0y := big.NewRat(start.X, 1), big.NewRat(start.Y, 1)
	if seg.Kind == SegmentQuad {
		c1x, c1y := big.NewRat(seg.C1.X, 1), big.NewRat(seg.C1.Y, 1)
		p1x, p1y := big.NewRat(seg.To.X, 1), big.NewRat(seg.To.Y, 1)
		// x(t) = (P0 - 2C + P1)t^2 + 2(C-P0)t + P0
		ax := new(big.Rat).Add(p0x, p1x)
		ax.Sub(ax, new(big.Rat).Mul(c1x, big.NewRat(2, 1)))
		ay := new(big.Rat).Add(p0y, p1y)
		ay.Sub(ay, new(big.Rat).Mul(c1y, big.NewRat(2, 1)))
		bx := new(big.Rat).Sub(c1x, p0x)
		bx.Mul(bx, big.NewRat(2, 1))
		by := new(big.Rat).Sub(c1y, p0y)
		by.Mul(by, big.NewRat(2, 1))
		return []*big.Rat{new(big.Rat).Set(p0x), bx, ax}, []*big.Rat{new(big.Rat).Set(p0y), by, ay}
	}
	c1x, c1y := big.NewRat(seg.C1.X, 1), big.NewRat(seg.C1.Y, 1)
	c2x, c2y := big.NewRat(seg.C2.X, 1), big.NewRat(seg.C2.Y, 1)
	p1x, p1y := big.NewRat(seg.To.X, 1), big.NewRat(seg.To.Y, 1)
	// x(t) = (P1 - 3C2 + 3C1 - P0)t^3 + 3(P0 - 2C1 + C2)t^2 + 3(C1-P0)t + P0
	ax := new(big.Rat).Sub(p1x, new(big.Rat).Mul(c2x, big.NewRat(3, 1)))
	ax.Add(ax, new(big.Rat).Mul(c1x, big.NewRat(3, 1)))
	ax.Sub(ax, p0x)
	ay := new(big.Rat).Sub(p1y, new(big.Rat).Mul(c2y, big.NewRat(3, 1)))
	ay.Add(ay, new(big.Rat).Mul(c1y, big.NewRat(3, 1)))
	ay.Sub(ay, p0y)
	bx := new(big.Rat).Add(p0x, new(big.Rat).Mul(c2x, big.NewRat(1, 1)))
	bx.Sub(bx, new(big.Rat).Mul(c1x, big.NewRat(2, 1)))
	bx.Mul(bx, big.NewRat(3, 1))
	by := new(big.Rat).Add(p0y, new(big.Rat).Mul(c2y, big.NewRat(1, 1)))
	by.Sub(by, new(big.Rat).Mul(c1y, big.NewRat(2, 1)))
	by.Mul(by, big.NewRat(3, 1))
	cx := new(big.Rat).Sub(c1x, p0x)
	cx.Mul(cx, big.NewRat(3, 1))
	cy := new(big.Rat).Sub(c1y, p0y)
	cy.Mul(cy, big.NewRat(3, 1))
	return []*big.Rat{new(big.Rat).Set(p0x), cx, bx, ax}, []*big.Rat{new(big.Rat).Set(p0y), cy, by, ay}
}

func derivativePoly(p []*big.Rat) []*big.Rat {
	if len(p) <= 1 {
		return []*big.Rat{new(big.Rat)}
	}
	out := make([]*big.Rat, len(p)-1)
	for i := 1; i < len(p); i++ {
		out[i-1] = new(big.Rat).Mul(p[i], big.NewRat(int64(i), 1))
	}
	return out
}

func polyMul(a, b []*big.Rat) []*big.Rat {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	out := make([]*big.Rat, len(a)+len(b)-1)
	for i := range out {
		out[i] = new(big.Rat)
	}
	for i, av := range a {
		for j, bv := range b {
			out[i+j].Add(out[i+j], new(big.Rat).Mul(av, bv))
		}
	}
	return out
}

func ratToInt64(r *big.Rat) (int64, error) {
	num := new(big.Int).Set(r.Num())
	den := new(big.Int).Set(r.Denom())
	if den.Sign() < 0 {
		num.Neg(num)
		den.Neg(den)
	}
	q, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	twice := new(big.Int).Lsh(new(big.Int).Abs(rem), 1)
	if twice.Cmp(den) >= 0 {
		if num.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	if !q.IsInt64() {
		return 0, fmt.Errorf("area overflow")
	}
	return q.Int64(), nil
}
