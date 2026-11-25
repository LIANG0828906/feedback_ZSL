// Package fixed contains deterministic signed integer arithmetic with explicit
// overflow detection. Values in the terrain model use millimetres, microradians
// and millionths of a normalized weight.
package fixed

import (
	"math/big"

	"terrainkernel/internal/kerr"
)

const Scale = int64(1_000_000)

type Rounding int

const (
	TowardZero Rounding = iota
	Floor
	Ceiling
	Nearest
)

var (
	bigMax = big.NewInt(1<<63 - 1)
	bigMin = new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 63))
)

func fromBig(v *big.Int, path string) (int64, error) {
	if v.Cmp(bigMax) > 0 || v.Cmp(bigMin) < 0 {
		return 0, kerr.Overflow(path)
	}
	return v.Int64(), nil
}

func Add(a, b int64, path string) (int64, error) {
	v := new(big.Int).Add(big.NewInt(a), big.NewInt(b))
	return fromBig(v, path)
}

func Sub(a, b int64, path string) (int64, error) {
	v := new(big.Int).Sub(big.NewInt(a), big.NewInt(b))
	return fromBig(v, path)
}

func Mul(a, b int64, path string) (int64, error) {
	v := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	return fromBig(v, path)
}

func divRounded(num, den *big.Int, mode Rounding) *big.Int {
	if den.Sign() == 0 {
		return new(big.Int)
	}
	q := new(big.Int)
	r := new(big.Int)
	q.QuoRem(num, den, r)
	if r.Sign() == 0 {
		return q
	}
	neg := num.Sign() != den.Sign()
	switch mode {
	case Floor:
		if neg {
			q.Sub(q, big.NewInt(1))
		}
	case Ceiling:
		if !neg {
			q.Add(q, big.NewInt(1))
		}
	case Nearest:
		twice := new(big.Int).Lsh(new(big.Int).Abs(r), 1)
		absDen := new(big.Int).Abs(den)
		if twice.Cmp(absDen) >= 0 {
			if neg {
				q.Sub(q, big.NewInt(1))
			} else {
				q.Add(q, big.NewInt(1))
			}
		}
	}
	return q
}

func Div(a, b int64, path string) (int64, error) {
	if b == 0 {
		return 0, kerr.New(kerr.CodeSchema, path, "division by zero")
	}
	v := divRounded(big.NewInt(a), big.NewInt(b), TowardZero)
	return fromBig(v, path)
}

func MulDiv(a, b, c int64, path string) (int64, error) {
	if c == 0 {
		return 0, kerr.New(kerr.CodeSchema, path, "division by zero")
	}
	num := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	v := divRounded(num, big.NewInt(c), TowardZero)
	return fromBig(v, path)
}

func MulDivRounded(a, b, c int64, path string, mode Rounding) (int64, error) {
	if c == 0 {
		return 0, kerr.New(kerr.CodeSchema, path, "division by zero")
	}
	num := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	v := divRounded(num, big.NewInt(c), mode)
	return fromBig(v, path)
}

// DivScale divides a by b while retaining six fractional digits.
func DivScale(a, b int64, path string) (int64, error) {
	return MulDiv(a, Scale, b, path)
}

// MulScale multiplies two fixed-point values carrying six fractional digits.
func MulScale(a, b int64, path string) (int64, error) {
	return MulDiv(a, b, Scale, path)
}

func Clamp(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func Abs(v int64) int64 {
	if v == -1<<63 {
		return 0
	}
	if v < 0 {
		return -v
	}
	return v
}
