package acousticfield

import (
	"math/big"
	"math/bits"
)

const (
	// Q16 is the fixed-point scale used for absorption coefficients, unit
	// normals and fractional gains.
	Q16 int64 = 1 << 16

	// Length units are integer micrometres. Energy units are deliberately
	// abstract integer "energy units" so all accounting is exact.
	Micrometre int64 = 1

	// Numeric-domain guards keep squares/products inside int64.
	maxCoordinate = int64(1_000_000_000)
	maxEnergyUnit = int64(1_000_000_000_000)
)

func mulQ16Floor(a, b int64) (value, residual int64) {
	prod := a * b
	value = prod / Q16
	residual = prod % Q16
	if residual < 0 {
		residual = -residual
	}
	return value, residual
}

func mulQ16Round(a, b int64) int64 {
	return divRound(a*b, Q16)
}

func divRound(n, d int64) int64 {
	if d == 0 {
		return 0
	}
	if n >= 0 {
		return (n + d/2) / d
	}
	return -((-n + d/2) / d)
}

func clampInt64(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ISqrt returns floor(sqrt(n)) for n >= 0 using integer arithmetic only.
func ISqrt(n int64) int64 {
	if n <= 0 {
		return 0
	}
	x := int64(bits.Len64(uint64(n)) / 2)
	x = int64(1) << x
	for {
		y := (x + n/x) / 2
		if y >= x {
			return x
		}
		x = y
	}
}

// Distance returns the integer Euclidean distance in micrometres.
func Distance(a, b Point) int64 {
	dx := a.X - b.X
	dy := a.Y - b.Y
	return ISqrt(dx*dx + dy*dy)
}

func rectArea(r Rect) int64 {
	return (r.MaxX - r.MinX) * (r.MaxY - r.MinY)
}

func pointInRect(p Point, r Rect) bool {
	return p.X >= r.MinX && p.X <= r.MaxX && p.Y >= r.MinY && p.Y <= r.MaxY
}

func rectInside(inner, outer Rect) bool {
	return inner.MinX >= outer.MinX && inner.MinY >= outer.MinY &&
		inner.MaxX <= outer.MaxX && inner.MaxY <= outer.MaxY
}

func rectOverlap(a, b Rect) bool {
	return a.MinX < b.MaxX && b.MinX < a.MaxX && a.MinY < b.MaxY && b.MinY < a.MaxY
}

func reflectPoint(p Point, r Reflector) Point {
	// All generated reflectors are axis-aligned. Avoid intersection rounding
	// in that common case; retain a generic formula for loaded documents.
	if r.Normal.X == Q16 && r.Normal.Y == 0 {
		return Point{X: 2*r.A.X - p.X, Y: p.Y}
	}
	if r.Normal.X == -Q16 && r.Normal.Y == 0 {
		return Point{X: 2*r.A.X - p.X, Y: p.Y}
	}
	if r.Normal.X == 0 && r.Normal.Y == Q16 {
		return Point{X: p.X, Y: 2*r.A.Y - p.Y}
	}
	if r.Normal.X == 0 && r.Normal.Y == -Q16 {
		return Point{X: p.X, Y: 2*r.A.Y - p.Y}
	}
	vx := p.X - r.A.X
	vy := p.Y - r.A.Y
	dot := divRound(vx*r.Normal.X+vy*r.Normal.Y, Q16)
	return Point{
		X: p.X - divRound(2*dot*r.Normal.X, Q16),
		Y: p.Y - divRound(2*dot*r.Normal.Y, Q16),
	}
}

func reflectAxisPoint(p Point, normal Vec, c int64) Point {
	if normal.X != 0 {
		return Point{X: 2*c - p.X, Y: p.Y}
	}
	return Point{X: p.X, Y: 2*c - p.Y}
}

func axisLineCoord(r Reflector) (vertical bool, c int64, ok bool) {
	if r.Normal.X != 0 && r.Normal.Y == 0 {
		return true, r.A.X, true
	}
	if r.Normal.Y != 0 && r.Normal.X == 0 {
		return false, r.A.Y, true
	}
	return false, 0, false
}

func intersectAxis(a, b Point, vertical bool, c int64) (Point, bool) {
	if vertical {
		dx := b.X - a.X
		if dx == 0 {
			return Point{}, false
		}
		y := a.Y + divRound((b.Y-a.Y)*(c-a.X), dx)
		return Point{X: c, Y: y}, true
	}
	dy := b.Y - a.Y
	if dy == 0 {
		return Point{}, false
	}
	x := a.X + divRound((b.X-a.X)*(c-a.Y), dy)
	return Point{X: x, Y: c}, true
}

// intersectReflector intersects line a-b with the reflector plane. Axis-aligned
// surfaces use the exact fast path; arbitrary normals use big-integer
// intermediates and round once back to the micrometre grid.
func intersectReflector(a, b Point, r Reflector) (Point, bool) {
	if vertical, coord, ok := axisLineCoord(r); ok {
		return intersectAxis(a, b, vertical, coord)
	}
	dx := b.X - a.X
	dy := b.Y - a.Y
	denom := dx*r.Normal.X + dy*r.Normal.Y
	if denom == 0 {
		return Point{}, false
	}
	numer := (r.A.X-a.X)*r.Normal.X + (r.A.Y-a.Y)*r.Normal.Y
	x := a.X + mulDivRoundBig(dx, numer, denom)
	y := a.Y + mulDivRoundBig(dy, numer, denom)
	return Point{X: x, Y: y}, true
}

func mulDivRoundBig(a, b, denom int64) int64 {
	if denom == 0 {
		return 0
	}
	num := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	den := big.NewInt(denom)
	q := new(big.Int)
	rem := new(big.Int)
	q.QuoRem(num, den, rem)
	twiceRem := new(big.Int).Abs(rem)
	twiceRem.Lsh(twiceRem, 1)
	absDen := new(big.Int).Abs(den)
	if twiceRem.Cmp(absDen) >= 0 {
		sign := int64(1)
		if num.Sign()*den.Sign() < 0 {
			sign = -1
		}
		q.Add(q, big.NewInt(sign))
	}
	return q.Int64()
}

func pointOnSegment(p, a, b Point, tolerance int64) bool {
	minX, maxX := a.X, b.X
	if minX > maxX {
		minX, maxX = maxX, minX
	}
	minY, maxY := a.Y, b.Y
	if minY > maxY {
		minY, maxY = maxY, minY
	}
	if p.X < minX-tolerance || p.X > maxX+tolerance || p.Y < minY-tolerance || p.Y > maxY+tolerance {
		return false
	}
	// Collinearity via cross product. The tolerance is widened in proportion
	// to segment extent to account for the fixed-point intersection.
	cross := (b.X-a.X)*(p.Y-a.Y) - (b.Y-a.Y)*(p.X-a.X)
	if cross < 0 {
		cross = -cross
	}
	length := Distance(a, b)
	return cross <= (tolerance+1)*(length+1)
}
