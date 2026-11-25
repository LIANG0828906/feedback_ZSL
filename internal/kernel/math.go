package kernel

import (
	"math/big"
	"strings"
)

const (
	PosScale      int64 = 1024
	CoeffScale    int64 = 10000
	DefaultTickMS int64 = 50

	RoundingTruncate = "truncate_toward_zero"
	RoundingFloor    = "floor"
	RoundingCeil     = "ceil"
	RoundingNearest  = "nearest_half_away"
)

type Vec2 struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
}

type RoundingMode string

func ParseRounding(s string) RoundingMode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case RoundingFloor:
		return RoundingFloor
	case RoundingCeil:
		return RoundingCeil
	case RoundingNearest:
		return RoundingNearest
	default:
		return RoundingTruncate
	}
}

func (m RoundingMode) String() string { return string(m) }

func Mul64(a, b int64) (int64, bool) {
	r := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	if !r.IsInt64() {
		return 0, false
	}
	return r.Int64(), true
}

func Add64(a, b int64) (int64, bool) {
	r := new(big.Int).Add(big.NewInt(a), big.NewInt(b))
	if !r.IsInt64() {
		return 0, false
	}
	return r.Int64(), true
}

func Sub64(a, b int64) (int64, bool) {
	r := new(big.Int).Sub(big.NewInt(a), big.NewInt(b))
	if !r.IsInt64() {
		return 0, false
	}
	return r.Int64(), true
}

func CheckedMulField(a, b int64, field, id string, version int) (int64, error) {
	v, ok := Mul64(a, b)
	if !ok {
		return 0, overflowError(field, id, version, a, b)
	}
	return v, nil
}

func CheckedAddField(a, b int64, field, id string, version int) (int64, error) {
	v, ok := Add64(a, b)
	if !ok {
		return 0, overflowError(field, id, version, a, b)
	}
	return v, nil
}

func ScaleWorld(v int64, field, id string, version int) (int64, error) {
	return CheckedMulField(v, PosScale, field, id, version)
}

func PermilleToMyriad(v int64) (int64, bool) {
	return Mul64(v, 10)
}

func TicksToMS(ticks, msPerTick int64) (int64, bool) {
	return Mul64(ticks, msPerTick)
}

// MulDiv returns round(a*b/div, mode) and the signed remainder of the
// truncated quotient. All arithmetic before rounding is arbitrary precision.
func MulDiv(a, b, div int64, mode RoundingMode) (quotient int64, remainder int64, err error) {
	if div == 0 {
		return 0, 0, asError(newIssue(CodeOvf, "$.muldiv.div", 0, "", "division by zero"))
	}
	num := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	d := big.NewInt(div)
	if div < 0 {
		d.Neg(d)
		num.Neg(num)
	}
	var q, r big.Int
	q.QuoRem(num, d, &r)

	absR := new(big.Int).Abs(&r)
	twice := new(big.Int).Lsh(absR, 1)
	switch mode {
	case RoundingFloor:
		if r.Sign() < 0 {
			q.Sub(&q, big.NewInt(1))
		}
	case RoundingCeil:
		if r.Sign() > 0 {
			q.Add(&q, big.NewInt(1))
		}
	case RoundingNearest:
		if twice.Cmp(d) >= 0 {
			if num.Sign() < 0 {
				q.Sub(&q, big.NewInt(1))
			} else {
				q.Add(&q, big.NewInt(1))
			}
		}
	}
	if !q.IsInt64() || !r.IsInt64() {
		return 0, 0, overflowError("$.muldiv", "", 0, a, b)
	}
	return q.Int64(), r.Int64(), nil
}

func DivScalar(v, div int64, mode RoundingMode) (int64, int64, error) {
	return MulDiv(v, 1, div, mode)
}

func NormalizeFixed(raw Vec2) Vec2 {
	n2 := new(big.Int).Add(
		new(big.Int).Mul(big.NewInt(raw.X), big.NewInt(raw.X)),
		new(big.Int).Mul(big.NewInt(raw.Y), big.NewInt(raw.Y)),
	)
	if n2.Sign() == 0 {
		return Vec2{}
	}
	length := new(big.Int).Sqrt(n2)
	scale := big.NewInt(PosScale)

	norm := func(component int64) int64 {
		num := new(big.Int).Mul(big.NewInt(component), scale)
		var q, r big.Int
		q.QuoRem(num, length, &r)
		twice := new(big.Int).Lsh(new(big.Int).Abs(&r), 1)
		if twice.Cmp(length) >= 0 {
			if num.Sign() < 0 {
				q.Sub(&q, big.NewInt(1))
			} else {
				q.Add(&q, big.NewInt(1))
			}
		}
		return q.Int64()
	}
	return Vec2{X: norm(raw.X), Y: norm(raw.Y)}
}

func RawNormal(a, b Vec2, side string) Vec2 {
	dx, _ := Sub64(b.X, a.X)
	dy, _ := Sub64(b.Y, a.Y)
	if strings.EqualFold(side, "right") || side == "R" {
		return Vec2{X: dy, Y: -dx}
	}
	return Vec2{X: -dy, Y: dx}
}

func Dot(a, b Vec2) int64 {
	// Callers in the physics core use bigDot when operands can approach limits.
	return a.X*b.X + a.Y*b.Y
}

func bigDot(a, b Vec2) *big.Int {
	return new(big.Int).Add(
		new(big.Int).Mul(big.NewInt(a.X), big.NewInt(b.X)),
		new(big.Int).Mul(big.NewInt(a.Y), big.NewInt(b.Y)),
	)
}

func bigSub(a, b int64) *big.Int {
	return new(big.Int).Sub(big.NewInt(a), big.NewInt(b))
}
