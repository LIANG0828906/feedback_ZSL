package fixedpoint

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"roomreverb/internal/model"
)

type Engine struct {
	Decimals int
	Scale    *big.Int
	Trace    *[]model.Truncation
	RoomID   string
	PathKey  string
}

func New(decimals int, trace *[]model.Truncation, roomID, pathKey string) *Engine {
	if decimals < 0 {
		decimals = 0
	}
	return &Engine{
		Decimals: decimals,
		Scale:    new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil),
		Trace:    trace,
		RoomID:   roomID,
		PathKey:  pathKey,
	}
}

func (e *Engine) record(step string, idx int, before, after *big.Int) {
	if e.Trace == nil {
		return
	}
	*e.Trace = append(*e.Trace, model.Truncation{
		Step:      step,
		RoomID:    e.RoomID,
		PathKey:   e.PathKey,
		SampleIdx: idx,
		At:        fmt.Sprintf("1e-%d", e.Decimals),
		Raw:       before.String(),
		Truncated: after.String(),
	})
}

func (e *Engine) Q() *big.Int { return new(big.Int).Set(e.Scale) }

func FromDecimalString(s string, decimals int) (*big.Int, error) {
	neg := false
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	} else if strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	parts := strings.SplitN(s, ".", 2)
	whole := parts[0]
	if whole == "" {
		whole = "0"
	}
	frac := ""
	if len(parts) == 2 {
		frac = parts[1]
	}
	if len(frac) < decimals {
		frac += strings.Repeat("0", decimals-len(frac))
	} else if decimals < len(frac) {
		frac = frac[:decimals]
	}
	combined := whole + frac
	combined = strings.TrimLeft(combined, "0")
	if combined == "" {
		combined = "0"
	}
	v, ok := new(big.Int).SetString(combined, 10)
	if !ok {
		return nil, fmt.Errorf("invalid decimal %q", s)
	}
	if neg {
		v.Neg(v)
	}
	return v, nil
}

func (e *Engine) FromFloat(v float64, step string, idx int) *big.Int {
	s := strconv.FormatFloat(v, 'f', e.Decimals+4, 64)
	q, err := FromDecimalString(s, e.Decimals)
	if err != nil {
		panic(err)
	}
	e.record(step, idx, q, q)
	return q
}

func (e *Engine) Const(s string) *big.Int {
	q, err := FromDecimalString(s, e.Decimals)
	if err != nil {
		panic(err)
	}
	return q
}

func (e *Engine) Mul(a, b *big.Int, step string, idx int) *big.Int {
	raw := new(big.Int).Mul(a, b)
	q, r := new(big.Int).QuoRem(raw, e.Scale, new(big.Int))
	if r.Sign() != 0 {
		e.record(step, idx, raw, q)
	}
	return q
}

func (e *Engine) Div(a, b *big.Int, step string, idx int) *big.Int {
	if b.Sign() == 0 {
		panic("fixedpoint: division by zero")
	}
	raw := new(big.Int).Mul(a, e.Scale)
	q, r := new(big.Int).QuoRem(raw, b, new(big.Int))
	if r.Sign() != 0 {
		e.record(step, idx, raw, q)
	}
	return q
}

func (e *Engine) DivInt(a *big.Int, n int64, step string, idx int) *big.Int {
	if n == 0 {
		panic("fixedpoint: integer division by zero")
	}
	q, r := new(big.Int).QuoRem(a, big.NewInt(n), new(big.Int))
	if r.Sign() != 0 {
		e.record(step, idx, a, q)
	}
	return q
}

func (e *Engine) MulInt(a *big.Int, n int64) *big.Int {
	return new(big.Int).Mul(a, big.NewInt(n))
}

func (e *Engine) Float(a *big.Int) float64 {
	if a == nil {
		return 0
	}
	f := new(big.Float).SetInt(a)
	scale := new(big.Float).SetInt(e.Scale)
	out, _ := new(big.Float).Quo(f, scale).Float64()
	return out
}

func Hypot(dx, dy int, decimals int) *big.Int {
	d := big.NewInt(int64(dx*dx + dy*dy))
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	raw := new(big.Int).Mul(d, new(big.Int).Mul(scale, scale))
	return new(big.Int).Sqrt(raw)
}

func SegmentLength(a, b model.Point, decimals int) *big.Int {
	return Hypot(b.X-a.X, b.Y-a.Y, decimals)
}

func PolylineLength(points []model.Point, decimals int) *big.Int {
	total := new(big.Int)
	for i := 1; i < len(points); i++ {
		total.Add(total, SegmentLength(points[i-1], points[i], decimals))
	}
	return total
}

const (
	ln2Decimal  = "0.693147180559945309417232121458176568"
	ln10Decimal = "2.302585092994045684017991454684364208"
)

func (e *Engine) Ln(x *big.Int, step string, idx int) *big.Int {
	if x.Sign() <= 0 {
		panic("fixedpoint: ln of non-positive value")
	}
	twoQ := new(big.Int).Mul(e.Scale, big.NewInt(2))
	half := big.NewInt(2)
	y := new(big.Int).Set(x)
	k := int64(0)
	for y.Cmp(twoQ) >= 0 {
		y.Quo(y, half)
		k++
	}
	for y.Cmp(e.Scale) < 0 {
		y.Mul(y, half)
		k--
	}

	num := new(big.Int).Sub(y, e.Scale)
	den := new(big.Int).Add(y, e.Scale)
	z := e.Div(num, den, step+":normalize", idx)
	z2 := e.Mul(z, z, step+":z2", idx)
	term := new(big.Int).Set(z)
	sum := new(big.Int).Set(z)
	iterations := 18
	if e.Decimals <= 4 {
		iterations = 10
	} else if e.Decimals <= 7 {
		iterations = 14
	}
	for n := 1; n <= iterations; n++ {
		term = e.Mul(term, z2, step+":series", idx)
		part := e.DivInt(term, int64(2*n+1), step+":odd", idx)
		sum.Add(sum, part)
	}
	result := e.MulInt(sum, 2)
	ln2 := e.Const(ln2Decimal)
	kPart := new(big.Int).Mul(big.NewInt(k), ln2)
	result.Add(result, kPart)
	return result
}

func (e *Engine) Log10(x *big.Int, step string, idx int) *big.Int {
	lnx := e.Ln(x, step+":ln", idx)
	ln10 := e.Const(ln10Decimal)
	return e.Div(lnx, ln10, step+":log10", idx)
}

func Log10Float(v float64, decimals int) float64 {
	e := New(decimals, nil, "", "")
	return e.Float(e.Log10(e.FromFloat(v, "input", 0), "log10", 0))
}
