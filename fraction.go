package colortrace

import (
	"encoding/json"
	"fmt"
	"math/big"
)

// Fraction is an exact signed rational value. JSON keeps numerator and
// denominator explicit so diagnostics never depend on floating point.
type Fraction struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

func Rat(num, den int64) Fraction {
	if den == 0 {
		panic("zero fraction denominator")
	}
	if den < 0 {
		num, den = -num, -den
	}
	g := gcd64(abs64(num), den)
	if g > 0 {
		num /= g
		den /= g
	}
	return Fraction{Num: num, Den: den}
}

func I64(v int64) Fraction { return Fraction{Num: v, Den: 1} }

func (f Fraction) IsZero() bool { return f.Num == 0 }

func (f Fraction) Float64() float64 {
	if f.Den == 0 {
		return 0
	}
	return float64(f.Num) / float64(f.Den)
}

func (f Fraction) String() string {
	if f.Den == 1 || f.Den == 0 {
		return fmt.Sprint(f.Num)
	}
	return fmt.Sprintf("%d/%d", f.Num, f.Den)
}

func (f Fraction) bigRat() *big.Rat {
	den := f.Den
	if den == 0 {
		den = 1
	}
	return new(big.Rat).SetFrac(big.NewInt(f.Num), big.NewInt(den))
}

func fractionFromBig(r *big.Rat) Fraction {
	if r == nil {
		return I64(0)
	}
	return Rat(r.Num().Int64(), r.Denom().Int64())
}

func AddFrac(a, b Fraction) Fraction {
	return fractionFromBig(new(big.Rat).Add(a.bigRat(), b.bigRat()))
}

func SubFrac(a, b Fraction) Fraction {
	return fractionFromBig(new(big.Rat).Sub(a.bigRat(), b.bigRat()))
}

func MulFrac(a, b Fraction) Fraction {
	return fractionFromBig(new(big.Rat).Mul(a.bigRat(), b.bigRat()))
}

func DivFrac(a, b Fraction) Fraction {
	br := b.bigRat()
	if br.Sign() == 0 {
		panic("division by zero fraction")
	}
	return fractionFromBig(new(big.Rat).Quo(a.bigRat(), br))
}

func AbsFrac(a Fraction) Fraction {
	if a.Num < 0 {
		return Rat(-a.Num, a.Den)
	}
	return a
}

func CmpFrac(a, b Fraction) int {
	return a.bigRat().Cmp(b.bigRat())
}

func SumFracs(values ...Fraction) Fraction {
	out := I64(0)
	for _, v := range values {
		out = AddFrac(out, v)
	}
	return out
}

func (f Fraction) MarshalJSON() ([]byte, error) {
	type alias Fraction
	return json.Marshal(alias(Rat(f.Num, f.Den)))
}

func gcd64(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
