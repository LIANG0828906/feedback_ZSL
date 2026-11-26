package engine_test

import (
	"testing"

	"schedlab/internal/domain"
	"schedlab/internal/engine"
	"schedlab/internal/gen"
)

func TestFeasibilityCollectsAllConstraints(t *testing.T) {
	b, _ := gen.BuildBundle("standard")
	p := b.Truth.(*domain.Project)
	foundMulti := false
	for _, pod := range p.Pods {
		res := engine.Filter(p, pod)
		for _, el := range res.Eliminations {
			if len(el.Constraints) > 1 {
				foundMulti = true
				// Declaration order: C1 before C2/C3 before C4/C5.
				last := ""
				for _, c := range el.Constraints {
					if c.Code < last {
						t.Fatalf("constraints not in declaration order: %v", el.Constraints)
					}
					last = c.Code
				}
			}
		}
		// Feasible and eliminated must partition the node set.
		if len(res.Feasible)+len(res.Eliminations) != len(p.Nodes) {
			t.Fatalf("feasibility partition broken for %s", pod.ID)
		}
	}
	if !foundMulti {
		t.Fatal("expected at least one node failing multiple constraints")
	}
}

func TestBothPathsAndOracleAgree(t *testing.T) {
	for _, profile := range []string{"standard", "large"} {
		b, _ := gen.BuildBundle(profile)
		p := b.Truth.(*domain.Project)
		paths := map[string]bool{}
		for _, pod := range p.Pods {
			feas := engine.Filter(p, pod)
			res := engine.Score(p, pod, feas.Feasible)
			paths[res.Path] = true
			oracle := gen.OracleScore(p, pod, feas.Feasible)
			if len(oracle) != len(res.Scores) {
				t.Fatalf("%s/%s length mismatch", profile, pod.ID)
			}
			for i, ns := range res.Scores {
				if ns.NodeID != oracle[i].NodeID ||
					ns.RawTotal != oracle[i].RawTotal ||
					ns.NormalizedBps != oracle[i].NormalizedBps {
					t.Fatalf("%s/%s oracle mismatch at %d: %+v vs %+v",
						profile, pod.ID, i, ns, oracle[i])
				}
				for j, c := range ns.Components {
					if c.Contribution != oracle[i].ComponentRaw[j] {
						t.Fatalf("component mismatch %s rule %d", pod.ID, j)
					}
				}
			}
			// Normalised values must stay inside the bps domain.
			for _, ns := range res.Scores {
				if ns.NormalizedBps < 0 || ns.NormalizedBps > 10000 {
					t.Fatalf("normalised score out of bps range: %d", ns.NormalizedBps)
				}
			}
		}
		if profile == "large" && !paths[engine.PathAgg] {
			t.Fatal("large profile must select the pre-aggregated path")
		}
	}
}

func TestSelectionDeterministic(t *testing.T) {
	b, _ := gen.BuildBundle("standard")
	p := b.Truth.(*domain.Project)
	pod := p.Pods[0]
	feas := engine.Filter(p, pod)
	first := engine.Score(p, pod, feas.Feasible).SelectedNode
	for i := 0; i < 5; i++ {
		if got := engine.Score(p, pod, feas.Feasible).SelectedNode; got != first {
			t.Fatalf("selection nondeterministic: %s vs %s", got, first)
		}
	}
}

// TestAccumulationOrderIndependent verifies rule enumeration order does not
// change the saturated total (integer addition is associative under the
// component set).
func TestAccumulationOrderIndependent(t *testing.T) {
	b, _ := gen.BuildBundle("standard")
	p := b.Truth.(*domain.Project)
	pod := p.Pods[0]
	feas := engine.Filter(p, pod)
	r1 := engine.Score(p, pod, feas.Feasible)
	reversed := *pod
	rules := append([]domain.AffinityRule(nil), pod.PreferRules...)
	for i, j := 0, len(rules)-1; i < j; i, j = i+1, j-1 {
		rules[i], rules[j] = rules[j], rules[i]
	}
	reversed.PreferRules = rules
	r2 := engine.Score(p, &reversed, feas.Feasible)
	for i := range r1.Scores {
		if r1.Scores[i].RawTotal != r2.Scores[i].RawTotal {
			t.Fatalf("raw total changed under rule reorder: %d vs %d",
				r1.Scores[i].RawTotal, r2.Scores[i].RawTotal)
		}
	}
}
