package fixed

import (
	"errors"
	"math/big"
)

var ErrOverflow = errors.New("fixed-point expression exceeds int64")

// Step is one observable quantization node in the camera -> screen chain.
// Remainder and Divisor are kept as integers so the diagnostic report can
// reproduce the exact lost fraction instead of relying on a floating estimate.
type Step struct {
	Name            string `json:"name"`
	Numerator       string `json:"numerator"`
	Denominator     string `json:"denominator"`
	Quotient        int64  `json:"quotient"`
	Remainder       string `json:"remainder"`
	Divisor         string `json:"divisor"`
	ScaleBits       uint8  `json:"scale_bits"`
	ResidualScreenQ int64  `json:"residual_screen_q"`
}

type Trace struct {
	Steps                   []Step `json:"steps"`
	RoundingResidualScreenQ int64  `json:"rounding_residual_screen_q"`
}

func (t *Trace) Add(step Step) {
	t.Steps = append(t.Steps, step)
	t.RoundingResidualScreenQ += step.ResidualScreenQ
}

func bigInt(v int64) *big.Int { return big.NewInt(v) }

func toInt64(v *big.Int) (int64, error) {
	if !v.IsInt64() {
		return 0, ErrOverflow
	}
	return v.Int64(), nil
}

// MulDivRound computes a*b/d with deterministic round-half-away-from-zero.
// The exact integer remainder is returned as well.
func MulDivRound(a, b, d int64) (q, rem int64, err error) {
	if d == 0 {
		return 0, 0, errors.New("division by zero")
	}
	num := new(big.Int).Mul(bigInt(a), bigInt(b))
	den := bigInt(d)
	quo, r := new(big.Int), new(big.Int)
	quo.QuoRem(num, den, r)
	twice := new(big.Int).Lsh(new(big.Int).Abs(r), 1)
	if twice.Cmp(new(big.Int).Abs(den)) >= 0 {
		if (num.Sign() < 0) != (den.Sign() < 0) {
			quo.Sub(quo, big.NewInt(1))
		} else {
			quo.Add(quo, big.NewInt(1))
		}
	}
	// Recompute remainder for the rounded quotient. The operation above is
	// intentionally explicit to keep rounding independent of platform math.
	r.Sub(num, new(big.Int).Mul(quo, den))
	q, err = toInt64(quo)
	if err != nil {
		return 0, 0, err
	}
	rem, err = toInt64(r)
	return q, rem, err
}

// DivRound is MulDivRound(a, 1, b).
func DivRound(a, b int64) (int64, int64, error) {
	return MulDivRound(a, 1, b)
}

// DotShiftRound computes (a.X*b.X + a.Y*b.Y + a.Z*b.Z) / 2^shift with the
// remainder kept as an integer in the source scale.
func DotShiftRound(ax, ay, az, bx, by, bz int64, shift uint8) (q, rem int64, err error) {
	sum := new(big.Int).Mul(bigInt(ax), bigInt(bx))
	sum.Add(sum, new(big.Int).Mul(bigInt(ay), bigInt(by)))
	sum.Add(sum, new(big.Int).Mul(bigInt(az), bigInt(bz)))
	return DivPow2Round(sum, shift)
}

func DivPow2Round(num *big.Int, shift uint8) (q, rem int64, err error) {
	den := new(big.Int).Lsh(big.NewInt(1), uint(shift))
	quo, r := new(big.Int), new(big.Int)
	quo.QuoRem(num, den, r)
	if new(big.Int).Lsh(new(big.Int).Abs(r), 1).Cmp(den) >= 0 {
		if num.Sign() < 0 {
			quo.Sub(quo, big.NewInt(1))
		} else {
			quo.Add(quo, big.NewInt(1))
		}
	}
	r.Sub(num, new(big.Int).Mul(quo, den))
	q, err = toInt64(quo)
	if err != nil {
		return 0, 0, err
	}
	rem, err = toInt64(r)
	return q, rem, err
}

func CeilDiv(a, b int64) int64 {
	if b == 0 {
		return 0
	}
	if a <= 0 {
		return 0
	}
	return (a + b - 1) / b
}

func Abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func Max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func Min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
