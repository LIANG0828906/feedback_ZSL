package expr

import (
	"testing"

	"skillsched/internal/errx"
	"skillsched/internal/fixed"
)

func TestFlatAndTreeEquivalent(t *testing.T) {
	flat := &Flat{Const: 7, Terms: []Term{{Var: "a", Coef: 3}, {Var: "b", Coef: -2}}}
	tree := TreeFromFlat("t", flat)
	env := Env{"a": 11, "b": 4, "base_regen": 10}
	fv, err := flat.Eval(env)
	if err != nil {
		t.Fatal(err)
	}
	tv, err := tree.Eval(env)
	if err != nil {
		t.Fatal(err)
	}
	if fv != tv || fv != 7+33-8 {
		t.Fatalf("flat=%d tree=%d", fv, tv)
	}
	// 树可无损折叠回常量表。
	back, err := FlatFromTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	bv, _ := back.Eval(env)
	if bv != fv {
		t.Fatalf("roundtrip flat=%d want %d", bv, fv)
	}
}

func TestEvalErrorsPointAtNode(t *testing.T) {
	div := &Node{ID: "n.div", Op: OpDiv, Round: fixed.RoundDown,
		Left: &Node{ID: "n.a", Op: OpNum, Value: 10}, Right: &Node{ID: "n.b", Op: OpNum, Value: 0}}
	_, err := div.Eval(Env{})
	if errx.CodeOf(err) != errx.EEval {
		t.Fatalf("want E_EVAL got %v", err)
	}
	sqrtNode := &Node{ID: "n.sqrt", Op: OpSqrt, Left: &Node{ID: "n.x", Op: OpNum, Value: -9}}
	if _, err := sqrtNode.Eval(Env{}); errx.CodeOf(err) != errx.EEval {
		t.Fatalf("want E_EVAL got %v", err)
	}
}

func TestNonLinearTreeIsLossy(t *testing.T) {
	tree := &Node{ID: "r", Op: OpSqrt, Left: &Node{ID: "r.x", Op: OpVar, Name: "x"}}
	_, err := FlatFromTree(tree)
	if errx.CodeOf(err) != errx.ELossy {
		t.Fatalf("want E_LOSSY got %v", err)
	}
}
