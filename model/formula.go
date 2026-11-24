package model

import (
	"fmt"

	"economycore/fixed"
)

// Expression operators supported by the canonical and v2/v3 expression trees.
const (
	ExprConst   = "const"
	ExprAdd     = "add"
	ExprSub     = "sub"
	ExprMul     = "mul"
	ExprDiv     = "div"
	ExprWorkers = "workers"
)

// Expr is an integer-only production-rate expression.
type Expr struct {
	Op    string `json:"op"`
	Value int64  `json:"value,omitempty"`
	Var   string `json:"var,omitempty"`
	Args  []Expr `json:"args,omitempty"`
}

// ConstExpr builds a constant expression.
func ConstExpr(value int64) Expr {
	return Expr{Op: ExprConst, Value: value}
}

// Clone deep-copies an expression.
func (e Expr) Clone() Expr {
	clone := e
	if e.Args != nil {
		clone.Args = make([]Expr, len(e.Args))
		for i, arg := range e.Args {
			clone.Args[i] = arg.Clone()
		}
	}
	return clone
}

// HasVariable reports whether evaluation needs runtime state.
func (e Expr) HasVariable() bool {
	if e.Op == ExprWorkers || (e.Var != "" && e.Op != ExprConst) {
		return true
	}
	for _, arg := range e.Args {
		if arg.HasVariable() {
			return true
		}
	}
	return false
}

// Eval evaluates the expression using only fixed-point integer operations.
func (e Expr) Eval(vars map[string]int64) (int64, error) {
	switch e.Op {
	case ExprConst:
		return e.Value, nil
	case ExprWorkers:
		if vars == nil {
			return 0, fmt.Errorf("formula: workers variable unavailable")
		}
		return vars[ExprWorkers], nil
	case ExprAdd:
		if len(e.Args) != 2 {
			return 0, fmt.Errorf("formula: add requires two arguments")
		}
		left, err := e.Args[0].Eval(vars)
		if err != nil {
			return 0, err
		}
		right, err := e.Args[1].Eval(vars)
		if err != nil {
			return 0, err
		}
		return fixed.Add(left, right)
	case ExprSub:
		if len(e.Args) != 2 {
			return 0, fmt.Errorf("formula: sub requires two arguments")
		}
		left, err := e.Args[0].Eval(vars)
		if err != nil {
			return 0, err
		}
		right, err := e.Args[1].Eval(vars)
		if err != nil {
			return 0, err
		}
		return fixed.Sub(left, right)
	case ExprMul:
		if len(e.Args) != 2 {
			return 0, fmt.Errorf("formula: mul requires two arguments")
		}
		left, err := e.Args[0].Eval(vars)
		if err != nil {
			return 0, err
		}
		right, err := e.Args[1].Eval(vars)
		if err != nil {
			return 0, err
		}
		return fixed.Mul(left, right)
	case ExprDiv:
		if len(e.Args) != 2 {
			return 0, fmt.Errorf("formula: div requires two arguments")
		}
		left, err := e.Args[0].Eval(vars)
		if err != nil {
			return 0, err
		}
		right, err := e.Args[1].Eval(vars)
		if err != nil {
			return 0, err
		}
		if right == 0 {
			return 0, fmt.Errorf("formula: division by zero")
		}
		return fixed.MulDiv(left, 1, right, fixed.RoundFloor)
	default:
		return 0, fmt.Errorf("formula: unknown op %q", e.Op)
	}
}

// Validate checks operator arity and constant values.
func (e Expr) Validate() error {
	switch e.Op {
	case ExprConst:
		return nil
	case ExprWorkers:
		return nil
	case ExprAdd, ExprSub, ExprMul, ExprDiv:
		if len(e.Args) != 2 {
			return fmt.Errorf("operator %s requires exactly two arguments", e.Op)
		}
		for _, arg := range e.Args {
			if err := arg.Validate(); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown operator %q", e.Op)
	}
}
