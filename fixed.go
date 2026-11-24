package editorpick

import (
	"errors"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// FixedScale is the declared decimal fixed-point scale: one unit is 1e-6.
const FixedScale int64 = 1_000_000

// Fixed is a deterministic signed fixed-point value in units of 1/FixedScale.
type Fixed int64

var (
	bigScale = big.NewInt(FixedScale)
	bigOne   = big.NewInt(1)
	bigTwo   = big.NewInt(2)
)

// FixedFromFloat rounds v to the declared fixed precision. Inputs are expected
// to be finite scene/query values validated at the boundary.
func FixedFromFloat(v float64) Fixed {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return Fixed(math.Round(v * float64(FixedScale)))
}

func FixedFromUnits(units int64) Fixed { return Fixed(units) }

func (f Fixed) Units() int64 { return int64(f) }

func (f Fixed) Float64() float64 {
	return float64(f) / float64(FixedScale)
}

func (f Fixed) String() string {
	negative := f < 0
	units := int64(f)
	if negative {
		units = -units
	}
	whole := units / FixedScale
	frac := units % FixedScale
	var b strings.Builder
	if negative {
		b.WriteByte('-')
	}
	b.WriteString(strconv.FormatInt(whole, 10))
	b.WriteByte('.')
	s := strconv.FormatInt(frac, 10)
	for i := len(s); i < 6; i++ {
		b.WriteByte('0')
	}
	b.WriteString(s)
	return b.String()
}

// MarshalJSON emits a decimal number so snapshots and reports remain readable.
func (f Fixed) MarshalJSON() ([]byte, error) {
	return []byte(f.String()), nil
}

// UnmarshalJSON accepts either a JSON integer in fixed units or a decimal
// number/string. Snapshot values use the six-decimal representation.
func (f *Fixed) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	if units, err := strconv.ParseInt(s, 10, 64); err == nil {
		*f = Fixed(units)
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || !isFinite(v) {
		return errInvalidNumber
	}
	*f = FixedFromFloat(v)
	return nil
}

func fixedFromDecimalString(s string) (Fixed, error) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || !isFinite(v) {
		return 0, errInvalidNumber
	}
	return FixedFromFloat(v), nil
}

func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

var errOverflow = errors.New("fixed point overflow")
var errInvalidNumber = errors.New("invalid number")

// mulFixed multiplies two fixed values and rounds the exact product once.
func mulFixed(a, b Fixed) (Fixed, error) {
	var product big.Int
	product.Mul(big.NewInt(int64(a)), big.NewInt(int64(b)))
	q, ok := roundedQuo(&product, FixedScale)
	if !ok {
		return 0, errOverflow
	}
	return Fixed(q), nil
}

func mustMul(a, b Fixed) Fixed {
	v, err := mulFixed(a, b)
	if err != nil {
		panic(err)
	}
	return v
}

// roundedQuo computes round(p / scale), preserving sign and using round-half-up
// on absolute values. All intermediate arithmetic is arbitrary precision.
func roundedQuo(p *big.Int, scale int64) (int64, bool) {
	if scale <= 0 {
		return 0, false
	}
	negative := p.Sign() < 0
	abs := new(big.Int).Abs(p)
	s := big.NewInt(scale)
	q, r := new(big.Int).QuoRem(abs, s, new(big.Int))
	twiceR := new(big.Int).Mul(r, bigTwo)
	if twiceR.Cmp(s) >= 0 {
		q.Add(q, bigOne)
	}
	if negative {
		q.Neg(q)
	}
	if !q.IsInt64() {
		return 0, false
	}
	return q.Int64(), true
}

func ceilQuo(numerator *big.Int, denominator int64) (int64, bool) {
	if denominator <= 0 {
		return 0, false
	}
	negative := numerator.Sign() < 0
	abs := new(big.Int).Abs(numerator)
	d := big.NewInt(denominator)
	q, r := new(big.Int).QuoRem(abs, d, new(big.Int))
	if r.Sign() != 0 {
		q.Add(q, bigOne)
	}
	if negative {
		q.Neg(q)
	}
	if !q.IsInt64() {
		return 0, false
	}
	return q.Int64(), true
}

func absFixed(v Fixed) Fixed {
	if v < 0 {
		return -v
	}
	return v
}

func maxFixed(a, b Fixed) Fixed {
	if a > b {
		return a
	}
	return b
}

func minFixed(a, b Fixed) Fixed {
	if a < b {
		return a
	}
	return b
}
