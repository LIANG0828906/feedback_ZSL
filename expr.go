package skillcore

import (
	"fmt"
	"math/big"
)

type Expr struct {
	Op    string `json:"op"`
	Value int64  `json:"value,omitempty"`
	Args  []Expr `json:"args,omitempty"`
}

func Const(v int64) Expr { return Expr{Op: "const", Value: v} }

func (e Expr) Clone() Expr {
	out := e
	out.Args = make([]Expr, len(e.Args))
	for i := range e.Args {
		out.Args[i] = e.Args[i].Clone()
	}
	return out
}

func (e Expr) Validate(path string) error {
	switch e.Op {
	case "const":
		if len(e.Args) != 0 {
			return domainErr(CodeInput, path, "const expression must not have arguments")
		}
	case "tick":
		if len(e.Args) != 0 || e.Value != 0 {
			return domainErr(CodeInput, path, "tick expression must not have arguments or a value")
		}
	case "add", "sub", "mul", "min", "max":
		if len(e.Args) == 0 {
			return domainErr(CodeInput, path, fmt.Sprintf("%s expression requires arguments", e.Op))
		}
		for i := range e.Args {
			if err := e.Args[i].Validate(fmt.Sprintf("%s.args[%d]", path, i)); err != nil {
				return err
			}
		}
	case "div":
		if len(e.Args) != 2 {
			return domainErr(CodeInput, path, "div expression requires exactly two arguments")
		}
		for i := range e.Args {
			if err := e.Args[i].Validate(fmt.Sprintf("%s.args[%d]", path, i)); err != nil {
				return err
			}
		}
	default:
		return domainErr(CodeInput, path, fmt.Sprintf("unknown expression op %q", e.Op))
	}
	return nil
}

type ExprContext struct {
	Tick int64
}

func (e Expr) Eval(ctx ExprContext, rounding Rounding, path string) (int64, error) {
	switch e.Op {
	case "const":
		return e.Value, nil
	case "tick":
		return ctx.Tick, nil
	case "add", "sub", "mul", "min", "max":
		values := make([]int64, len(e.Args))
		for i := range e.Args {
			v, err := e.Args[i].Eval(ctx, rounding, fmt.Sprintf("%s.args[%d]", path, i))
			if err != nil {
				return 0, err
			}
			values[i] = v
		}
		switch e.Op {
		case "add":
			acc := values[0]
			for _, v := range values[1:] {
				next, ok := addChecked(acc, v)
				if !ok {
					return 0, overflowError(path, "value", "addition overflow")
				}
				acc = next
			}
			return acc, nil
		case "sub":
			acc := values[0]
			for _, v := range values[1:] {
				next, ok := subChecked(acc, v)
				if !ok {
					return 0, overflowError(path, "value", "subtraction overflow")
				}
				acc = next
			}
			return acc, nil
		case "mul":
			acc := values[0]
			for _, v := range values[1:] {
				next, ok := mulChecked(acc, v)
				if !ok {
					return 0, overflowError(path, "value", "multiplication overflow")
				}
				acc = next
			}
			return acc, nil
		case "min":
			acc := values[0]
			for _, v := range values[1:] {
				if v < acc {
					acc = v
				}
			}
			return acc, nil
		case "max":
			acc := values[0]
			for _, v := range values[1:] {
				if v > acc {
					acc = v
				}
			}
			return acc, nil
		}
	case "div":
		a, err := e.Args[0].Eval(ctx, rounding, path+".args[0]")
		if err != nil {
			return 0, err
		}
		b, err := e.Args[1].Eval(ctx, rounding, path+".args[1]")
		if err != nil {
			return 0, err
		}
		if b == 0 {
			return 0, domainErr(CodeInput, path, "division by zero")
		}
		return divRounded(a, b, rounding), nil
	}
	return 0, domainErr(CodeInput, path, fmt.Sprintf("unknown expression op %q", e.Op))
}

// Constant returns the value of a tick-independent expression. It is used to
// safely flatten expression trees when exporting to a flat legacy schema.
func (e Expr) Constant(rounding Rounding, path string) (int64, bool, error) {
	if e.ContainsTick() {
		return 0, false, nil
	}
	v, err := e.Eval(ExprContext{}, rounding, path)
	if err != nil {
		return 0, false, err
	}
	return v, true, nil
}

func (e Expr) ContainsTick() bool {
	if e.Op == "tick" {
		return true
	}
	for i := range e.Args {
		if e.Args[i].ContainsTick() {
			return true
		}
	}
	return false
}

func (e Expr) IsConst() bool {
	return e.Op == "const" && len(e.Args) == 0
}

func addChecked(a, b int64) (int64, bool) {
	z := new(big.Int).Add(big.NewInt(a), big.NewInt(b))
	if !z.IsInt64() {
		return 0, false
	}
	return z.Int64(), true
}

func subChecked(a, b int64) (int64, bool) {
	z := new(big.Int).Sub(big.NewInt(a), big.NewInt(b))
	if !z.IsInt64() {
		return 0, false
	}
	return z.Int64(), true
}

func mulChecked(a, b int64) (int64, bool) {
	z := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	if !z.IsInt64() {
		return 0, false
	}
	return z.Int64(), true
}

func divRounded(a, b int64, rounding Rounding) int64 {
	q := new(big.Int).Quo(big.NewInt(a), big.NewInt(b))
	r := new(big.Int).Rem(big.NewInt(a), big.NewInt(b))
	if r.Sign() == 0 {
		return q.Int64()
	}
	qInt := q.Int64()
	rInt := r.Int64()
	positiveDenominator := b > 0
	switch rounding {
	case RoundFloor:
		if (rInt > 0) != positiveDenominator {
			qInt--
		}
	case RoundCeil:
		if (rInt > 0) == positiveDenominator {
			qInt++
		}
	case RoundNearest:
		absR := rInt
		if absR < 0 {
			absR = -absR
		}
		absB := b
		if absB < 0 {
			absB = -absB
		}
		half := absB/2 + absB%2
		if absR >= half {
			if (rInt > 0) == positiveDenominator {
				qInt++
			} else {
				qInt--
			}
		}
	case RoundTrunc:
	}
	return qInt
}
