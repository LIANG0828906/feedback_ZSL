package budget

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Expr is a deliberately tiny integer expression tree.
//
// Supported kinds: lit, var, add, sub, mul, div, min, max, neg.
// Arithmetic is checked against the int64 boundary. Division declares its
// rounding in div.rounding; sample rules use non-negative numerators.
type Expr struct {
	Kind     string           `json:"kind"`
	Node     string           `json:"node,omitempty"`
	Value    int64            `json:"value,omitempty"`
	Name     string           `json:"name,omitempty"`
	Rounding Rounding         `json:"rounding,omitempty"`
	Args     []*Expr          `json:"args,omitempty"`
	Vars     map[string]int64 `json:"vars,omitempty"`
}

// Eval evaluates an expression tree with integer-only arithmetic.
func Eval(expr *Expr, vars map[string]int64, field string) (int64, error) {
	if expr == nil {
		return 0, &Error{Code: ECodeEval, Message: "nil expression", Node: field, Field: field}
	}
	if len(vars) == 0 && len(expr.Vars) > 0 {
		vars = expr.Vars
	}
	node := expr.Node
	if node == "" {
		node = field
	}

	switch expr.Kind {
	case "lit":
		return expr.Value, nil
	case "var":
		if v, ok := vars[expr.Name]; ok {
			return v, nil
		}
		if v, ok := expr.Vars[expr.Name]; ok {
			return v, nil
		}
		return 0, &Error{
			Code: ECodeEval, Message: "unknown expression variable",
			Node: node, Field: field, Arguments: vars,
		}
	case "neg":
		if len(expr.Args) != 1 {
			return 0, evalArgError(node, field, expr, "neg requires one argument")
		}
		v, err := Eval(expr.Args[0], vars, field)
		if err != nil {
			return 0, err
		}
		if v == -9223372036854775808 {
			return 0, overflowError(field+".neg", v, -1).withNode(node)
		}
		return -v, nil
	case "add", "sub", "mul", "div", "min", "max":
		if len(expr.Args) == 0 {
			return 0, evalArgError(node, field, expr, expr.Kind+" requires arguments")
		}
		values := make([]int64, len(expr.Args))
		argMap := map[string]int64{}
		for i, arg := range expr.Args {
			v, err := Eval(arg, vars, field)
			if err != nil {
				return 0, err
			}
			values[i] = v
			argMap[fmt.Sprintf("arg%d", i)] = v
		}
		result, err := evalOp(expr.Kind, values, expr.Rounding, field)
		if err != nil {
			if be, ok := AsError(err); ok {
				be.Node = node
				if be.Arguments == nil {
					be.Arguments = argMap
				}
			}
			return 0, err
		}
		return result, nil
	default:
		return 0, &Error{Code: ECodeEval, Message: "unsupported expression kind " + expr.Kind, Node: node, Field: field}
	}
}

func evalOp(op string, values []int64, rounding Rounding, field string) (int64, error) {
	switch op {
	case "add":
		result := values[0]
		for _, v := range values[1:] {
			next, err := addChecked(result, v, field)
			if err != nil {
				return 0, err
			}
			result = next
		}
		return result, nil
	case "sub":
		result := values[0]
		for _, v := range values[1:] {
			next, err := subChecked(result, v, field)
			if err != nil {
				return 0, err
			}
			result = next
		}
		return result, nil
	case "mul":
		result := values[0]
		for _, v := range values[1:] {
			next, err := mulChecked(result, v, field)
			if err != nil {
				return 0, err
			}
			result = next
		}
		return result, nil
	case "div":
		if len(values) != 2 {
			return 0, newError(ECodeEval, "div requires exactly two arguments").withField(field)
		}
		mode := rounding
		if mode == "" {
			mode = RoundTrunc
		}
		q, _, err := divRemChecked(values[0], values[1], mode, field)
		return q, err
	case "min":
		result := values[0]
		for _, v := range values[1:] {
			if v < result {
				result = v
			}
		}
		return result, nil
	case "max":
		result := values[0]
		for _, v := range values[1:] {
			if v > result {
				result = v
			}
		}
		return result, nil
	default:
		return 0, newError(ECodeEval, "unknown operator %q", op).withField(field)
	}
}

func subChecked(a, b int64, field string) (int64, error) {
	switch {
	case b > 0 && a < math.MinInt64+b:
		return 0, overflowError(field, a, b)
	case b < 0 && a > math.MaxInt64+b:
		return 0, overflowError(field, a, b)
	default:
		return a - b, nil
	}
}

func evalArgError(node, field string, expr *Expr, msg string) *Error {
	args := map[string]int64{}
	for i, arg := range expr.Args {
		if arg != nil && arg.Kind == "lit" {
			args[fmt.Sprintf("arg%d", i)] = arg.Value
		}
	}
	return &Error{Code: ECodeEval, Message: msg, Node: node, Field: field, Arguments: args}
}

func (e *Error) withNode(node string) *Error {
	e.Node = node
	return e
}

// FlatRuleTable is the flat constant-table representation. Keys are stable
// expression node identifiers such as regen:mana and cdr:fireball.
type FlatRuleTable map[string]int64

// EquivalenceMismatch identifies the first expression node whose tree and
// flat-table evaluations disagree.
type EquivalenceMismatch struct {
	Node      string `json:"node"`
	FlatValue int64  `json:"flat_value"`
	TreeValue int64  `json:"tree_value"`
	FirstNode string `json:"first_node"`
}

// CompareFlatAndTree evaluates tree nodes in deterministic lexical order and
// compares them with the flat table. The first divergence is returned.
func CompareFlatAndTree(flat FlatRuleTable, trees map[string]*Expr, vars map[string]int64) (*EquivalenceMismatch, error) {
	nodes := make([]string, 0, len(trees))
	for node := range trees {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)
	for _, node := range nodes {
		treeValue, err := Eval(trees[node], vars, "expression."+node)
		if err != nil {
			return nil, err
		}
		flatValue, ok := flat[node]
		if !ok {
			return &EquivalenceMismatch{Node: node, FirstNode: node, TreeValue: treeValue}, &Error{
				Code: ECodeEval, Message: "flat table missing expression node", Node: node,
			}
		}
		if flatValue != treeValue {
			return &EquivalenceMismatch{
					Node: node, FirstNode: node, FlatValue: flatValue, TreeValue: treeValue,
				}, &Error{
					Code: ECodeDiff, Message: "flat and tree expressions diverge", Node: node,
					Expected: flatValue, Actual: treeValue,
				}
		}
	}
	return nil, nil
}

// EffectiveCooldown applies a basis-point CDR coefficient. Reduction uses
// floor(base*cdr/10000), so the result is always an integer in [0,base].
func EffectiveCooldown(baseTicks, cdrBP int64, field string) (int64, error) {
	if baseTicks < 0 {
		return 0, &Error{Code: ECodeScale, Message: "cooldown must be non-negative", Field: field, Operands: []int64{baseTicks}}
	}
	if cdrBP < 0 || cdrBP > MaxCDRBasisPoints {
		return 0, &Error{
			Code: ECodeEval, Message: "cooldown reduction coefficient outside declared [0,10000] boundary",
			Node: field, Field: field, Arguments: map[string]int64{"cdr_bp": cdrBP},
		}
	}
	scaled, err := mulChecked(baseTicks, cdrBP, field)
	if err != nil {
		return 0, err
	}
	reduction := scaled / MaxCDRBasisPoints
	effective := baseTicks - reduction
	if effective < 0 || effective > baseTicks {
		return 0, &Error{Code: ECodeEval, Message: "effective cooldown crossed declared bound", Node: field}
	}
	return effective, nil
}

// ValidateRegenBoundary rejects negative resource recovery.
func ValidateRegenBoundary(node string, regen BP, args map[string]int64) error {
	if regen < 0 {
		return &Error{
			Code: ECodeEval, Message: "resource regeneration is negative",
			Node: node, Field: "resource.regen", Arguments: args,
		}
	}
	return nil
}

func canonicalNodeID(parts ...string) string {
	return strings.Join(parts, ":")
}
