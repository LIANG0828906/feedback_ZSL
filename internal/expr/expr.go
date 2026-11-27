// Package expr 提供资源回复与冷却缩减的两种表达式形态：
//   - Tree：显式表达式树（schema v3），支持四则、取模、开方、min/max；
//   - Flat：扁平常量表（schema v1/v2），仅能表达线性式 const + Σ coef*var。
//
// 两种形态对同一系数在同一环境变量下必须求出相同整型结果。
package expr

import (
	"strconv"

	"skillsched/internal/errx"
	"skillsched/internal/fixed"
)

type Op string

const (
	OpNum  Op = "num" // 整型常量
	OpVar  Op = "var" // 环境变量引用
	OpAdd  Op = "add"
	OpSub  Op = "sub"
	OpMul  Op = "mul"
	OpDiv  Op = "div"
	OpMod  Op = "mod"
	OpNeg  Op = "neg"
	OpSqrt Op = "sqrt"
	OpMin  Op = "min"
	OpMax  Op = "max"
)

// Node 是表达式树节点。ID 是稳定的节点路径，用于 E_EVAL 定位。
type Node struct {
	ID    string      `json:"id"`
	Op    Op          `json:"op"`
	Value int64       `json:"value,omitempty"` // OpNum
	Name  string      `json:"name,omitempty"`  // OpVar
	Round fixed.Round `json:"round,omitempty"` // OpDiv 取舍方向
	Left  *Node       `json:"left,omitempty"`
	Right *Node       `json:"right,omitempty"`
}

// Env 是求值环境，保存变量名到整型值的映射。
type Env map[string]int64

func evalErr(n *Node, a, b int64, msg string) *errx.Error {
	e := errx.New(errx.EEval, msg).With("node", n.ID, "op", string(n.Op),
		"operand_a", strconv.FormatInt(a, 10), "operand_b", strconv.FormatInt(b, 10))
	return e
}

// Eval 以纯整型方式求表达式树的值。
func (n *Node) Eval(env Env) (int64, error) {
	switch n.Op {
	case OpNum:
		return n.Value, nil
	case OpVar:
		v, ok := env[n.Name]
		if !ok {
			return 0, errx.New(errx.EEval, "unbound variable").
				With("node", n.ID, "var", n.Name)
		}
		return v, nil
	case OpNeg:
		v, err := n.Left.Eval(env)
		if err != nil {
			return 0, err
		}
		if v == minInt64 {
			return 0, evalErr(n, v, 0, "negate overflow")
		}
		return -v, nil
	case OpSqrt:
		v, err := n.Left.Eval(env)
		if err != nil {
			return 0, err
		}
		return fixed.Sqrt(n.ID, v)
	case OpMin, OpMax:
		a, err := n.Left.Eval(env)
		if err != nil {
			return 0, err
		}
		b, err := n.Right.Eval(env)
		if err != nil {
			return 0, err
		}
		if n.Op == OpMin {
			if a < b {
				return a, nil
			}
			return b, nil
		}
		if a > b {
			return a, nil
		}
		return b, nil
	default:
		a, err := n.Left.Eval(env)
		if err != nil {
			return 0, err
		}
		b, err := n.Right.Eval(env)
		if err != nil {
			return 0, err
		}
		return binary(n, a, b)
	}
}

const minInt64 = -1 << 63

func binary(n *Node, a, b int64) (int64, error) {
	switch n.Op {
	case OpAdd:
		return fixed.Add(n.ID, a, b)
	case OpSub:
		return fixed.Add(n.ID, a, -b)
	case OpMul:
		return fixed.Mul(n.ID, a, b)
	case OpMod:
		if b == 0 {
			return 0, evalErr(n, a, b, "modulo by zero")
		}
		return a % b, nil
	case OpDiv:
		if b == 0 {
			return 0, evalErr(n, a, b, "division by zero")
		}
		q, _ := divRound(a, b, n.Round)
		return q, nil
	default:
		return 0, errx.New(errx.EEval, "unknown binary op").With("node", n.ID, "op", string(n.Op))
	}
}

// divRound 纯整型带取舍方向除法，b 必须非零。返回商与残差（非负）。
func divRound(a, b int64, r fixed.Round) (q, rem int64) {
	q = a / b
	rem = a - q*b
	absRem := rem
	if absRem < 0 {
		absRem = -rem
	}
	sign := int64(1)
	if (a < 0) != (b < 0) {
		sign = -1
	}
	switch r {
	case fixed.RoundUp:
		if absRem != 0 {
			q += sign
		}
	case fixed.RoundNear:
		if absRem*2 >= abs64(b) {
			q += sign
		}
	}
	return q, absRem
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// Term 是扁平常量表中的一个线性项 coef*var。
type Term struct {
	Var  string `json:"var"`
	Coef int64  `json:"coef"`
}

// Flat 是扁平常量表表达：Const + Σ Coef*Var。
type Flat struct {
	Const int64  `json:"const"`
	Terms []Term `json:"terms"`
}

// Eval 求扁平常量表的值，与等价树求值结果一致。
func (f *Flat) Eval(env Env) (int64, error) {
	out := f.Const
	for _, t := range f.Terms {
		v, ok := env[t.Var]
		if !ok {
			return 0, errx.New(errx.EEval, "unbound variable").
				With("node", "flat:"+t.Var, "var", t.Var)
		}
		prod, err := fixed.Mul("flat:"+t.Var, t.Coef, v)
		if err != nil {
			return 0, err
		}
		out, err = fixed.Add("flat:sum", out, prod)
		if err != nil {
			return 0, err
		}
	}
	return out, nil
}

// TreeFromFlat 由常量表构造等价表达式树（节点 ID 稳定可复算）。
func TreeFromFlat(idPrefix string, f *Flat) *Node {
	root := &Node{ID: idPrefix + ".const", Op: OpNum, Value: f.Const}
	for i, t := range f.Terms {
		mul := &Node{
			ID:    idPrefix + ".term" + strconv.Itoa(i),
			Op:    OpMul,
			Left:  &Node{ID: idPrefix + ".term" + strconv.Itoa(i) + ".coef", Op: OpNum, Value: t.Coef},
			Right: &Node{ID: idPrefix + ".term" + strconv.Itoa(i) + ".var", Op: OpVar, Name: t.Var},
		}
		root = &Node{ID: idPrefix + ".add" + strconv.Itoa(i), Op: OpAdd, Left: root, Right: mul}
	}
	return root
}

// FlatFromTree 尝试把线性表达式树折叠为常量表。
// 遇到乘除/开方/min/max 等无法用线性表表达的节点时返回 E_LOSSY 并指出节点。
func FlatFromTree(n *Node) (*Flat, error) {
	f := &Flat{}
	if err := flatten(n, f); err != nil {
		return nil, err
	}
	return f, nil
}

func flatten(n *Node, f *Flat) error {
	switch n.Op {
	case OpNum:
		c, err := fixed.Add(n.ID, f.Const, n.Value)
		if err != nil {
			return err
		}
		f.Const = c
		return nil
	case OpVar:
		f.Terms = append(f.Terms, Term{Var: n.Name, Coef: 1})
		return nil
	case OpNeg:
		sub := &Flat{}
		if err := flatten(n.Left, sub); err != nil {
			return err
		}
		return mergeNeg(n.ID, f, sub)
	case OpAdd, OpSub:
		if err := flatten(n.Left, f); err != nil {
			return err
		}
		sub := &Flat{}
		if err := flatten(n.Right, sub); err != nil {
			return err
		}
		if n.Op == OpSub {
			if err := mergeNeg(n.ID, f, sub); err != nil {
				return err
			}
		} else {
			if err := merge(f, sub); err != nil {
				return err
			}
		}
		return nil
	case OpMul:
		// 仅当一侧为常量时保持线性。
		lv, lok := numOrOne(n.Left)
		rv, rok := numOrOne(n.Right)
		switch {
		case lok:
			return scaleInto(n.ID, f, n.Right, lv)
		case rok:
			return scaleInto(n.ID, f, n.Left, rv)
		default:
			return lossy(n)
		}
	default:
		return lossy(n)
	}
}

func numOrOne(n *Node) (int64, bool) {
	if n.Op == OpNum {
		return n.Value, true
	}
	return 0, false
}

func scaleInto(where string, dst *Flat, sub *Node, k int64) error {
	tmp := &Flat{}
	if err := flatten(sub, tmp); err != nil {
		return err
	}
	c, err := fixed.Mul(where, tmp.Const, k)
	if err != nil {
		return err
	}
	dst.Const, err = fixed.Add(where, dst.Const, c)
	if err != nil {
		return err
	}
	for _, t := range tmp.Terms {
		cf, err := fixed.Mul(where, t.Coef, k)
		if err != nil {
			return err
		}
		dst.Terms = append(dst.Terms, Term{Var: t.Var, Coef: cf})
	}
	return nil
}

func merge(dst, src *Flat) error {
	c, err := fixed.Add("flat:merge", dst.Const, src.Const)
	if err != nil {
		return err
	}
	dst.Const = c
	dst.Terms = append(dst.Terms, src.Terms...)
	return nil
}

func mergeNeg(where string, dst, src *Flat) error {
	neg := &Flat{Const: -src.Const}
	for _, t := range src.Terms {
		neg.Terms = append(neg.Terms, Term{Var: t.Var, Coef: -t.Coef})
	}
	return merge(dst, neg)
}

func lossy(n *Node) *errx.Error {
	return errx.New(errx.ELossy, "expression node cannot be represented as flat linear table").
		With("node", n.ID, "op", string(n.Op))
}
