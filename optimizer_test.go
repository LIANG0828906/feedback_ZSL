package queryopt

import (
	"reflect"
	"testing"
)

func planFor(t *testing.T, p *Project, bindings BindingSet, qid string) *QueryPlan {
	t.Helper()
	est, err := EstimateProject(p, bindings)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanQuery(p, est, qid, bindings, OptionsFromProject(p))
	if err != nil {
		if coded, ok := AsError(err); ok && coded.Code == CodeDisconnectedPlan {
			return plan
		}
		t.Fatalf("plan %s: %v", qid, err)
	}
	return plan
}

func TestJoinOrderDegenerateRulesAndRulingBasis(t *testing.T) {
	sample, _ := loadedSample(t)
	p := sample.Project

	q1 := planFor(t, p, sample.Bindings, "q1_sales_slice")
	wantQ1 := []string{"tbl_customer", "tbl_orders", "tbl_lineitem"}
	if !reflect.DeepEqual(q1.SelectedOrder, wantQ1) {
		t.Fatalf("q1 order=%v want %v", q1.SelectedOrder, wantQ1)
	}
	if q1.RulingRuleID != "RR_MIN_TOTAL_COST" {
		t.Fatalf("q1 ruling=%s / %s", q1.RulingRuleID, q1.RulingBasisID)
	}

	p.Rules.TieRelativePermyriad = 2_000
	tied := planFor(t, p, sample.Bindings, "q1_sales_slice")
	if tied.RulingRuleID != "RR_TIE_DECLARED_ORDER" {
		t.Fatalf("tie ruling=%s basis=%s", tied.RulingRuleID, tied.RulingBasisID)
	}
	if tied.SelectedCandidateID != "CAND-q1_sales_slice-01" {
		t.Fatalf("declared-order tie winner=%s", tied.SelectedCandidateID)
	}

	q3 := planFor(t, p, sample.Bindings, "q3_cycle")
	if !reflect.DeepEqual(q3.CutEdgeIDs, []string{"edge_q3_co"}) {
		t.Fatalf("cycle cuts=%v", q3.CutEdgeIDs)
	}
	foundCutDiag := false
	for _, d := range q3.Diagnostics {
		if d.Code == ReasonCycleCut && d.Context["cut_edge_id"] == "edge_q3_co" {
			foundCutDiag = true
		}
	}
	if !foundCutDiag {
		t.Fatalf("missing cycle diagnostic: %+v", q3.Diagnostics)
	}

	q4 := planFor(t, p, sample.Bindings, "q4_cartesian")
	cartesianReject := false
	for _, c := range q4.Candidates {
		if c.RejectCode == ReasonCartesianForbidden && c.EdgeID == "edge_q4_cross_cl" {
			cartesianReject = true
		}
	}
	if !cartesianReject {
		t.Fatalf("missing cartesian rejection: %+v", q4.Candidates)
	}
	if len(q4.SelectedOrder) != 3 {
		t.Fatalf("q4 should still find path via orders: %v", q4.SelectedOrder)
	}

	est, _ := EstimateProject(p, sample.Bindings)
	q5, err := PlanQuery(p, est, "q5_unreachable", sample.Bindings, OptionsFromProject(p))
	if code := errorCode(err); code != CodeDisconnectedPlan {
		t.Fatalf("q5 code=%s want %s err=%v", code, CodeDisconnectedPlan, err)
	}
	if q5 == nil || q5.ErrorCode != CodeDisconnectedPlan {
		t.Fatalf("q5 plan error envelope missing: %+v", q5)
	}
	unreachable := false
	for _, d := range q5.Diagnostics {
		if d.Code == ReasonUnreachablePred && d.Context["predicate_id"] == "pred_q5_audit_tag" {
			unreachable = true
		}
	}
	if !unreachable {
		t.Fatalf("missing unreachable predicate diagnostic: %+v", q5.Diagnostics)
	}
}

func TestParameterSensitivityAndRelativeExpressions(t *testing.T) {
	sample, _ := loadedSample(t)
	rare := sample.Bindings
	frequent := BindingSet{Global: map[int]int64{1: 5, 2: 25, 3: 1}}
	report, err := AnalyzeSensitivity(sample.Project, rare, frequent, OptionsFromProject(sample.Project))
	if err != nil {
		t.Fatal(err)
	}
	var q2 QuerySensitivity
	for _, q := range report.Queries {
		if q.QueryID == "q2_account_txn" {
			q2 = q
		}
	}
	if !q2.OrderSwitched {
		t.Fatalf("q2 did not switch: %+v", q2)
	}
	if !reflect.DeepEqual(q2.OrderBefore, []string{"tbl_account", "tbl_txn"}) {
		t.Fatalf("before order=%v", q2.OrderBefore)
	}
	if !reflect.DeepEqual(q2.OrderAfter, []string{"tbl_txn", "tbl_account"}) {
		t.Fatalf("after order=%v", q2.OrderAfter)
	}
	if q2.CostDelta <= 0 {
		t.Fatalf("cost delta should be observable: %+d", q2.CostDelta)
	}
	if !containsString(q2.TriggerPredicateIDs, "pred_q2_skew") {
		t.Fatalf("trigger predicates=%v", q2.TriggerPredicateIDs)
	}
	if !q2.AccessPathSwitched {
		t.Fatalf("access path did not switch: %+v", q2.AccessPathChanges)
	}

	relRare, err := RelativeFromGlobal(sample.Project, rare)
	if err != nil {
		t.Fatal(err)
	}
	relFreq := RelativeBindings{
		"q1_sales_slice": {0: 5, 1: 25},
		"q2_account_txn": {0: 1},
	}
	globalRare2, err := GlobalFromRelative(sample.Project, relRare)
	if err != nil {
		t.Fatal(err)
	}
	globalFreq2, err := GlobalFromRelative(sample.Project, relFreq)
	if err != nil {
		t.Fatal(err)
	}
	if !sameBindings(globalRare2, rare) || !sameBindings(globalFreq2, frequent) {
		t.Fatalf("relative conversion mismatch rare=%v freq=%v", globalRare2, globalFreq2)
	}
	report2, err := AnalyzeSensitivity(sample.Project, globalRare2, globalFreq2, OptionsFromProject(sample.Project))
	if err != nil {
		t.Fatal(err)
	}
	for i := range report.Queries {
		a, b := report.Queries[i], report2.Queries[i]
		if a.OrderSwitched != b.OrderSwitched || a.CostDelta != b.CostDelta {
			t.Fatalf("sensitivity expression mismatch at %s: %+v vs %+v", a.QueryID, a, b)
		}
	}
}

func errorCode(err error) string {
	if e, ok := AsError(err); ok {
		return e.Code
	}
	return ""
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
