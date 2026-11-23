package tactical

import (
	"math"
	"math/big"
	"math/bits"
)

const (
	// CoordinateScale converts grid coordinates to the integer fixed-point
	// coordinate system used by smoothing and tracing.
	CoordinateScale int64 = 1000
	// AngleScale converts radians to milliradians.
	AngleScale int64 = 1000
	// TwoPiMilli is one full turn in milliradians.
	TwoPiMilli int64 = 6283
)

// FixPt is a deterministic fixed-point coordinate in milli-grid units.
type FixPt struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
}

// WorldPoint is the continuous-coordinate diagnostic representation of a
// fixed-point point.
type WorldPoint struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

func (p FixPt) World() WorldPoint {
	return WorldPoint{
		X: float64(p.X) / float64(CoordinateScale),
		Y: float64(p.Y) / float64(CoordinateScale),
	}
}

func (p FixPt) Add(q FixPt) FixPt {
	return FixPt{X: p.X + q.X, Y: p.Y + q.Y}
}

func (p FixPt) Sub(q FixPt) FixPt {
	return FixPt{X: p.X - q.X, Y: p.Y - q.Y}
}

func GridToFixed(p GridPoint) FixPt {
	return FixPt{
		X: int64(p.X) * CoordinateScale,
		Y: int64(p.Y) * CoordinateScale,
	}
}

func FixedToGrid(p FixPt) GridPoint {
	return GridPoint{
		X: int(math.Round(float64(p.X) / float64(CoordinateScale))),
		Y: int(math.Round(float64(p.Y) / float64(CoordinateScale))),
	}
}

// MilliFloat validates and converts a finite JSON number to fixed-point
// milli-units.
func MilliFloat(v float64) (int64, bool) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	scaled := math.Round(v * float64(CoordinateScale))
	if scaled < math.MinInt64 || scaled > math.MaxInt64 {
		return 0, false
	}
	return int64(scaled), true
}

func DistanceMilli(a, b FixPt) int64 {
	dx := a.X - b.X
	dy := a.Y - b.Y
	if dx < 0 {
		dx = -dx
	}
	if dy < 0 {
		dy = -dy
	}
	return int64(isqrt(uint64(dx)*uint64(dx) + uint64(dy)*uint64(dy)))
}

func isqrt(value uint64) uint64 {
	if value == 0 {
		return 0
	}
	x := uint64(1) << ((bits.Len64(value) + 1) / 2)
	for {
		next := (x + value/x) / 2
		if next >= x {
			return x
		}
		x = next
	}
}

// DistanceSquaredBelow compares the distance from p to segment ab with tol
// exactly, using arbitrary precision integers so large finite maps cannot
// overflow the geometry predicate.
func DistanceSquaredBelow(a, b, p FixPt, tol int64) bool {
	if a == b {
		return squaredDistanceLE(a, p, tol)
	}
	abx := b.X - a.X
	aby := b.Y - a.Y
	apx := p.X - a.X
	apy := p.Y - a.Y
	cross := abx*apy - aby*apx
	len2 := abx*abx + aby*aby
	if len2 == 0 {
		return squaredDistanceLE(a, p, tol)
	}
	cross2 := new(big.Int).Mul(big.NewInt(cross), big.NewInt(cross))
	limit := new(big.Int).Mul(big.NewInt(tol), big.NewInt(tol))
	limit.Mul(limit, big.NewInt(len2))
	return cross2.Cmp(limit) <= 0
}

func squaredDistanceLE(a, b FixPt, tol int64) bool {
	dx := new(big.Int).Sub(big.NewInt(a.X), big.NewInt(b.X))
	dy := new(big.Int).Sub(big.NewInt(a.Y), big.NewInt(b.Y))
	dx.Mul(dx, dx)
	dy.Mul(dy, dy)
	d2 := new(big.Int).Add(dx, dy)
	limit := new(big.Int).Mul(big.NewInt(tol), big.NewInt(tol))
	return d2.Cmp(limit) <= 0
}

func AngleTo(a, b FixPt) int64 {
	dx := b.X - a.X
	dy := b.Y - a.Y
	if dx == 0 && dy == 0 {
		return 0
	}
	return NormalizeAngle(int64(math.Round(math.Atan2(float64(dy), float64(dx)) * float64(AngleScale))))
}

func NormalizeAngle(angle int64) int64 {
	if TwoPiMilli <= 0 {
		return 0
	}
	angle %= TwoPiMilli
	if angle < 0 {
		angle += TwoPiMilli
	}
	return angle
}

func AngleDelta(from, to int64) int64 {
	delta := NormalizeAngle(to) - NormalizeAngle(from)
	if delta > TwoPiMilli/2 {
		delta -= TwoPiMilli
	}
	if delta < -TwoPiMilli/2 {
		delta += TwoPiMilli
	}
	return delta
}

func Sign(v int64) int64 {
	switch {
	case v < 0:
		return -1
	case v > 0:
		return 1
	default:
		return 0
	}
}
