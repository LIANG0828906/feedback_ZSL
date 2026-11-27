package fixed

import (
	"math/big"

	"proxydiag/bizerr"
)

// Result records whether a declared-width operation saturated.
type Result struct {
	Value     int64
	Overflow  bool
	Saturated bool
}

func MaxForWidth(width int) int64 {
	if width >= 63 {
		return int64(^uint64(0) >> 1)
	}
	if width <= 0 {
		return 0
	}
	return int64((uint64(1) << uint(width)) - 1)
}

func Unit(frac uint) int64 {
	return int64(uint64(1) << frac)
}

// MulDiv computes (a*b)/c using an exact intermediate and clamps the result to
// the declared integer width. Operands are non-negative fixed-point values.
func MulDiv(a, b, c int64, width int) (Result, error) {
	if a < 0 || b < 0 || c <= 0 {
		return Result{}, bizerr.New(bizerr.FixedOverflow, "invalid operands for fixed-point multiply/divide").
			WithInt64("a", a).WithInt64("b", b).WithInt64("c", c)
	}
	max := MaxForWidth(width)
	if a == 0 || b == 0 {
		return Result{Value: 0}, nil
	}
	prod := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	q := new(big.Int).Quo(prod, big.NewInt(c))
	limit := big.NewInt(max)
	if q.Cmp(limit) > 0 {
		return Result{Value: max, Overflow: true, Saturated: true}, nil
	}
	return Result{Value: q.Int64()}, nil
}

func Add(a, b int64, width int) (Result, error) {
	if a < 0 || b < 0 {
		return Result{}, bizerr.New(bizerr.FixedOverflow, "negative fixed-point addition").
			WithInt64("a", a).WithInt64("b", b)
	}
	max := MaxForWidth(width)
	sum := new(big.Int).Add(big.NewInt(a), big.NewInt(b))
	if sum.Cmp(big.NewInt(max)) > 0 {
		return Result{Value: max, Overflow: true, Saturated: true}, nil
	}
	return Result{Value: sum.Int64()}, nil
}

// Normalize returns value/total as a fraction scaled by 2^frac.
func Normalize(value, total int64, width int, frac uint) (Result, error) {
	return MulDiv(value, Unit(frac), total, width)
}

// Require checks a fixed result and converts saturation or overflow to a
// stable business error when precision would be silently lost.
func Require(r Result, operation string) (int64, error) {
	if r.Overflow || r.Saturated {
		return r.Value, bizerr.New(bizerr.FixedOverflow, "fixed-point operation exceeded declared width").
			With("operation", operation).WithInt64("saturated_value", r.Value)
	}
	return r.Value, nil
}
