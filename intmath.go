package mirror

import "math/big"

// RoundingMode controls integer division and conversion remainders.
type RoundingMode string

const (
	RoundFloor   RoundingMode = "floor"
	RoundCeil    RoundingMode = "ceil"
	RoundNearest RoundingMode = "nearest"
)

func ParseRoundingMode(s string) (RoundingMode, bool) {
	switch RoundingMode(s) {
	case "", RoundFloor:
		return RoundFloor, true
	case RoundCeil:
		return RoundCeil, true
	case RoundNearest:
		return RoundNearest, true
	default:
		return "", false
	}
}

// FloorDiv returns floor(a/b) for positive b.
func FloorDiv(a, b int64) int64 {
	q, _ := divMod(a, b)
	return q
}

func divMod(a, b int64) (int64, int64) {
	q := a / b
	r := a % b
	if r < 0 {
		q--
		r += b
	}
	return q, r
}

func divRound(a, b int64, mode RoundingMode) (q, remainder int64, overflow bool) {
	if b <= 0 {
		return 0, 0, true
	}
	if a == -1<<63 && b == -1 {
		return 0, 0, true
	}
	q = a / b
	r := a % b
	switch mode {
	case RoundFloor:
		if r < 0 {
			q--
			r += b
		}
	case RoundCeil:
		if r > 0 {
			q++
			r -= b
		}
	case RoundNearest:
		if r < 0 {
			if -r*2 >= b {
				q--
			}
		} else if r*2 >= b {
			q++
		}
		// residual is reported in the same convention as floor for
		// reproducibility: distance from the rounded result.
		r = a - q*b
	default:
		return 0, 0, true
	}
	return q, r, false
}

// MulDivChecked performs a*b/c with arbitrary-precision intermediates and
// reports E_OVF instead of silently wrapping int64.
func MulDivChecked(field string, a, b, c int64, mode RoundingMode) (int64, int64, error) {
	if c <= 0 {
		return 0, 0, &Error{Code: CodeOverflow, Path: field, Field: field,
			Operands: []int64{a, b, c}, Message: "divisor must be positive"}
	}
	product := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	q, r := new(big.Int), new(big.Int)
	q.QuoRem(product, big.NewInt(c), r)
	switch mode {
	case RoundFloor:
		if product.Sign() < 0 && r.Sign() != 0 {
			q.Sub(q, big.NewInt(1))
			r.Add(r, big.NewInt(c))
		}
	case RoundCeil:
		if product.Sign() > 0 && r.Sign() != 0 {
			q.Add(q, big.NewInt(1))
			r.Sub(r, big.NewInt(c))
		}
	case RoundNearest:
		absR := new(big.Int).Abs(r)
		absR.Mul(absR, big.NewInt(2))
		if absR.Cmp(big.NewInt(c)) >= 0 {
			if product.Sign() < 0 {
				q.Sub(q, big.NewInt(1))
			} else {
				q.Add(q, big.NewInt(1))
			}
			r.Sub(product, new(big.Int).Mul(q, big.NewInt(c)))
		}
	default:
		return 0, 0, &Error{Code: CodeOverflow, Path: field, Field: field,
			Operands: []int64{a, b, c}, Message: "unknown rounding mode"}
	}
	if !q.IsInt64() {
		return 0, 0, &Error{Code: CodeOverflow, Path: field, Field: field,
			Operands: []int64{a, b}, Message: "multiplication/division result overflows int64"}
	}
	if !r.IsInt64() {
		return 0, 0, &Error{Code: CodeOverflow, Path: field, Field: field,
			Operands: []int64{a, b}, Message: "remainder overflows int64"}
	}
	return q.Int64(), r.Int64(), nil
}

// AddChecked adds two integers and returns E_OVF with both operands.
func AddChecked(field string, a, b int64) (int64, error) {
	if (b > 0 && a > (1<<63-1)-b) || (b < 0 && a < (-1<<63)-b) {
		return 0, &Error{Code: CodeOverflow, Path: field, Field: field,
			Operands: []int64{a, b}, Message: "addition overflows int64"}
	}
	return a + b, nil
}

// SubChecked subtracts b from a and returns E_OVF with both operands.
func SubChecked(field string, a, b int64) (int64, error) {
	if (b < 0 && a > (1<<63-1)+b) || (b > 0 && a < (-1<<63)+b) {
		return 0, &Error{Code: CodeOverflow, Path: field, Field: field,
			Operands: []int64{a, b}, Message: "subtraction overflows int64"}
	}
	return a - b, nil
}
