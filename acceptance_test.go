package meshpolicy_test

import (
	"reflect"
	"strconv"
	"testing"

	"meshpolicy/internal/engine"
	"meshpolicy/internal/gen"
	"meshpolicy/internal/model"
	"meshpolicy/internal/schema"
)

func TestThreeSchemaEquivalenceAndGateAnchors(t *testing.T) {
	sample := gen.Generate(gen.DefaultSeed)
	versions := []string{"v1", "v2", "v3"}
	projects := map[string]*model.Project{}
	reports := map[string]*engine.RunReport{}

	for _, version := range versions {
		p, err := schema.Load(sample.Documents[version])
		if err != nil {
			t.Fatalf("load %s: %v", version, err)
		}
		projects[version] = p
		report, err := engine.Run(p, engine.RunOptions{Workers: 3, BufferPerNS: 32})
		if err != nil {
			t.Fatalf("run %s: %v", version, err)
		}
		reports[version] = report
	}

	base := projects["v1"]
	for _, version := range []string{"v2", "v3"} {
		if !reflect.DeepEqual(policyOrder(base), policyOrder(projects[version])) {
			t.Fatalf("%s policy order mismatch:\nv1=%v\n%s=%v", version, policyOrder(base), version, policyOrder(projects[version]))
		}
		if !reflect.DeepEqual(attemptIDs(base), attemptIDs(projects[version])) {
			t.Fatalf("%s attempt order mismatch", version)
		}
		if !reflect.DeepEqual(selectorMatchSet(base), selectorMatchSet(projects[version])) {
			t.Fatalf("%s selector match mismatch:\nv1=%v\n%s=%v", version, selectorMatchSet(base), version, selectorMatchSet(projects[version]))
		}
		if !reflect.DeepEqual(propagationSignature(reports["v1"]), propagationSignature(reports[version])) {
			t.Fatalf("%s propagation arrival mismatch:\nv1=%v\n%s=%v", version, propagationSignature(reports["v1"]), version, propagationSignature(reports[version]))
		}
	}

	for _, workers := range []int{1, 2, 4} {
		r, err := engine.Run(base, engine.RunOptions{Workers: workers, BufferPerNS: 64})
		if err != nil {
			t.Fatalf("run workers=%d: %v", workers, err)
		}
		if !reflect.DeepEqual(decisionSignature(r), decisionSignature(reports["v1"])) {
			t.Fatalf("worker count %d changed decisions", workers)
		}
	}

	byID := map[string]engine.Decision{}
	for _, d := range reports["v1"].Decisions {
		byID[d.AttemptID] = d
	}
	for _, anchor := range sample.Truth.Anchors {
		d, ok := byID[anchor.AttemptID]
		if !ok {
			t.Fatalf("missing decision %s", anchor.AttemptID)
		}
		if d.Allow != anchor.Allow {
			t.Fatalf("%s allow=%v want %v (candidate=%v hit=%s gate=%s marker=%s)", anchor.AttemptID, d.Allow, anchor.Allow, d.CandidateAllow, d.HitPolicyID, d.Gate.State, d.Gate.MarkerID)
		}
		if anchor.HitPolicyID != "" && d.HitPolicyID != anchor.HitPolicyID {
			t.Fatalf("%s hit=%s want %s", anchor.AttemptID, d.HitPolicyID, anchor.HitPolicyID)
		}
		if anchor.AppliedPolicy != "" && d.AppliedPolicyID != anchor.AppliedPolicy {
			t.Fatalf("%s applied=%s want %s gate=%+v", anchor.AttemptID, d.AppliedPolicyID, anchor.AppliedPolicy, d.Gate)
		}
		if anchor.Gate != "" && d.Gate.State != anchor.Gate {
			t.Fatalf("%s gate=%s want %s", anchor.AttemptID, d.Gate.State, anchor.Gate)
		}
		if anchor.MarkerGen != 0 && d.Gate.Generation != anchor.MarkerGen {
			t.Fatalf("%s marker gen=%d want %d", anchor.AttemptID, d.Gate.Generation, anchor.MarkerGen)
		}
	}
}

func policyOrder(p *model.Project) []string {
	policies := append([]model.Policy(nil), p.Policies...)
	return idsSortedByPriority(policies)
}

func idsSortedByPriority(policies []model.Policy) []string {
	out := make([]string, 0, len(policies))
	// simple insertion via sort in helper? Use engine-independent stable ordering.
	for len(policies) > 0 {
		best := 0
		for i := 1; i < len(policies); i++ {
			a, b := policies[best], policies[i]
			if b.Priority > a.Priority || (b.Priority == a.Priority && b.Order < a.Order) ||
				(b.Priority == a.Priority && b.Order == a.Order && b.ID < a.ID) {
				best = i
			}
		}
		out = append(out, policies[best].ID)
		policies = append(policies[:best], policies[best+1:]...)
	}
	return out
}

func attemptIDs(p *model.Project) []string {
	var out []string
	for _, a := range p.Attempts() {
		out = append(out, a.ID)
	}
	return out
}

func selectorMatchSet(p *model.Project) map[string][]string {
	workloads := map[string]model.Workload{}
	for _, w := range p.Workloads {
		workloads[w.ID] = w
	}
	out := map[string][]string{}
	for _, pol := range p.Policies {
		var ids []string
		for id, w := range workloads {
			if engine.SelectorMatches(pol.Source, pol.Namespace, w) {
				ids = append(ids, id)
			}
		}
		out[pol.ID] = model.SortedStrings(ids)
	}
	return out
}

func decisionSignature(r *engine.RunReport) [][6]string {
	out := make([][6]string, 0, len(r.Decisions))
	for _, d := range r.Decisions {
		verdict := "deny"
		if d.Allow {
			verdict = "allow"
		}
		out = append(out, [6]string{d.AttemptID, verdict, d.HitPolicyID, d.AppliedPolicyID, d.Gate.State, d.Gate.MarkerID})
	}
	return out
}

func propagationSignature(r *engine.RunReport) map[string][]string {
	out := map[string][]string{}
	if r.Propagation == nil {
		return out
	}
	for _, layer := range r.Propagation.Layers {
		if layer.Generation == 0 {
			continue
		}
		key := layer.ChangeID + "#G" + strconv.Itoa(layer.Generation)
		out[key] = append([]string(nil), layer.ArrivalSet...)
	}
	return out
}

func TestIncrementalMatchesFullRecompute(t *testing.T) {
	sample := gen.Generate(gen.DefaultSeed)
	req := engine.EditRequest{
		Kind: "priority", Seq: 2, PolicyID: "pol-pay-order-allow", Priority: intPtr(30),
	}
	report, edited, err := engine.IncrementalRecompute(sample.Project, req, engine.RunOptions{Workers: 2, BufferPerNS: 64})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Consistent {
		t.Fatalf("incremental mismatch: %v", report.Mismatches)
	}
	if len(report.RecomputedIDs) == 0 || len(report.ReusedIDs) == 0 {
		t.Fatalf("expected recomputed and reused ids, got recomputed=%v reused=%v", report.RecomputedIDs, report.ReusedIDs)
	}
	if report.Boundary.MaxGeneration < 2 {
		t.Fatalf("expected dependency closure generation >=2, got %+v", report.Boundary)
	}

	actionReq := engine.EditRequest{Kind: "action", Seq: 2, PolicyID: "pol-edge-gateway-allow", Action: "deny"}
	actionReport, _, err := engine.IncrementalRecompute(edited, actionReq, engine.RunOptions{Workers: 3, BufferPerNS: 64})
	if err != nil {
		t.Fatal(err)
	}
	if !actionReport.Consistent {
		t.Fatalf("action incremental mismatch: %v", actionReport.Mismatches)
	}

	selectorReport, _, err := engine.IncrementalRecompute(sample.Project, engine.EditRequest{
		Kind: "selector_label", Seq: 2, PolicyID: "pol-pay-order-allow", LabelKey: "app", LabelValue: "",
	}, engine.RunOptions{Workers: 2, BufferPerNS: 64})
	if err != nil {
		t.Fatal(err)
	}
	if !selectorReport.Consistent {
		t.Fatalf("selector incremental mismatch: %v", selectorReport.Mismatches)
	}

	workloadReport, _, err := engine.IncrementalRecompute(sample.Project, engine.EditRequest{
		Kind: "workload_label", Seq: 2, WorkloadID: "order", LabelKey: "tier", LabelValue: "critical",
	}, engine.RunOptions{Workers: 2, BufferPerNS: 64})
	if err != nil {
		t.Fatal(err)
	}
	if !workloadReport.Consistent {
		t.Fatalf("workload incremental mismatch: %v", workloadReport.Mismatches)
	}
}

func intPtr(v int) *int { return &v }
