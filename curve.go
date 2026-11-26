package silhouette

import (
	"math"
	"math/big"
)

type rpoint struct {
	x *big.Rat
	y *big.Rat
}

func newRPoint(v Vec2) rpoint {
	return rpoint{x: new(big.Rat).SetInt64(v.X), y: new(big.Rat).SetInt64(v.Y)}
}

func midRP(a, b rpoint) rpoint {
	x := new(big.Rat).Add(a.x, b.x)
	x.Quo(x, big.NewRat(2, 1))
	y := new(big.Rat).Add(a.y, b.y)
	y.Quo(y, big.NewRat(2, 1))
	return rpoint{x: x, y: y}
}

func lerpRP(a, b rpoint, t *big.Rat) rpoint {
	oneMinusT := new(big.Rat).Sub(big.NewRat(1, 1), t)
	x := new(big.Rat).Mul(oneMinusT, a.x)
	x.Add(x, new(big.Rat).Mul(t, b.x))
	y := new(big.Rat).Mul(oneMinusT, a.y)
	y.Add(y, new(big.Rat).Mul(t, b.y))
	return rpoint{x: x, y: y}
}

func splitCubic(cp [4]rpoint) (left, right [4]rpoint) {
	p01 := midRP(cp[0], cp[1])
	p12 := midRP(cp[1], cp[2])
	p23 := midRP(cp[2], cp[3])
	p012 := midRP(p01, p12)
	p123 := midRP(p12, p23)
	p := midRP(p012, p123)
	left = [4]rpoint{cp[0], p01, p012, p}
	right = [4]rpoint{p, p123, p23, cp[3]}
	return left, right
}

func evalCubic(cp [4]rpoint, t *big.Rat) rpoint {
	p01 := lerpRP(cp[0], cp[1], t)
	p12 := lerpRP(cp[1], cp[2], t)
	p23 := lerpRP(cp[2], cp[3], t)
	p012 := lerpRP(p01, p12, t)
	p123 := lerpRP(p12, p23, t)
	return lerpRP(p012, p123, t)
}

func ratSub(a, b *big.Rat) *big.Rat { return new(big.Rat).Sub(a, b) }
func ratMul(a, b *big.Rat) *big.Rat { return new(big.Rat).Mul(a, b) }
func ratAdd(a, b *big.Rat) *big.Rat { return new(big.Rat).Add(a, b) }

func crossRat(ax, ay, bx, by *big.Rat) *big.Rat {
	left := ratMul(ax, by)
	right := ratMul(ay, bx)
	return ratSub(left, right)
}

func squareRat(v *big.Rat) *big.Rat {
	return ratMul(v, v)
}

func distanceSquaredToLineRat(p, a, b rpoint) *big.Rat {
	dx := ratSub(b.x, a.x)
	dy := ratSub(b.y, a.y)
	chordSq := ratAdd(squareRat(dx), squareRat(dy))
	if chordSq.Sign() == 0 {
		px := ratSub(p.x, a.x)
		py := ratSub(p.y, a.y)
		return ratAdd(squareRat(px), squareRat(py))
	}
	vx := ratSub(p.x, a.x)
	vy := ratSub(p.y, a.y)
	cross := crossRat(dx, dy, vx, vy)
	return new(big.Rat).Quo(squareRat(cross), chordSq)
}

func compareDistanceSquaredToTolerance(distSq *big.Rat, tolerance int64) int {
	if tolerance < 0 {
		return 1
	}
	limit := new(big.Rat).SetInt64(tolerance)
	limit.Mul(limit, limit)
	return distSq.Cmp(limit)
}

// floorSqrtRat computes floor(sqrt(r)) without floating point. For r=N/D,
// sqrt(N/D) = sqrt(N*D)/D.
func floorSqrtRat(r *big.Rat) (int64, bool) {
	if r == nil || r.Sign() <= 0 {
		return 0, false
	}
	n := new(big.Int).Set(r.Num())
	n.Abs(n)
	d := new(big.Int).Set(r.Denom())
	product := new(big.Int).Mul(n, d)
	root := new(big.Int).Sqrt(product)
	root.Quo(root, d)
	if !root.IsInt64() {
		return math.MaxInt64, true
	}
	return root.Int64(), false
}

func ratToFixedNearest(v *big.Rat) (int64, bool) {
	if v == nil {
		return 0, false
	}
	num := new(big.Int).Set(v.Num())
	den := new(big.Int).Set(v.Denom())
	neg := num.Sign() < 0
	num.Abs(num)
	q := new(big.Int).Quo(num, den)
	r := new(big.Int).Rem(num, den)
	r.Lsh(r, 1)
	if r.Cmp(den) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	if neg {
		q.Neg(q)
	}
	if !q.IsInt64() {
		return 0, true
	}
	return q.Int64(), false
}

func ratInt64(v *big.Rat, field string) (int64, error) {
	if !v.IsInt() || !v.Num().IsInt64() {
		return 0, overflowError(field, "rational_to_int", 0, 0)
	}
	return v.Num().Int64(), nil
}

func curveFlatnessSq(cp [4]rpoint) *big.Rat {
	max := distanceSquaredToLineRat(cp[1], cp[0], cp[3])
	d2 := distanceSquaredToLineRat(cp[2], cp[0], cp[3])
	if d2.Cmp(max) > 0 {
		max = d2
	}
	mid := evalCubic(cp, big.NewRat(1, 2))
	d3 := distanceSquaredToLineRat(mid, cp[0], cp[3])
	if d3.Cmp(max) > 0 {
		max = d3
	}
	return max
}

func curveMeasuredChordHeight(cp [4]rpoint) int64 {
	chordA, chordB := cp[0], cp[3]
	max := int64(0)
	for _, t := range []*big.Rat{big.NewRat(1, 4), big.NewRat(1, 2), big.NewRat(3, 4)} {
		p := evalCubic(cp, t)
		v, overflow := floorSqrtRat(distanceSquaredToLineRat(p, chordA, chordB))
		if overflow {
			return math.MaxInt64
		}
		if v > max {
			max = v
		}
	}
	return max
}
