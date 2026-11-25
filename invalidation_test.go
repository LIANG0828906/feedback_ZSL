package plancache

import (
	"testing"
)

func closureGenerations(t *testing.T, reports []ClosureReport) map[string]map[string]int {
	t.Helper()
	out := map[string]map[string]int{}
	for _, r := range reports {
		out[r.EventID] = map[string]int{}
		for _, item := range r.Items {
			out[r.EventID][item.StatementID] = item.Generation
		}
	}
	return out
}

func TestInvalidationClosureGenerationsAndNoFalseDependency(t *testing.T) {
	p := GenerateProject(DefaultSeed)
	p.Capacity.MaxCacheBytes = 1024 * 1024
	engine, err := PrimeAllPlans(p)
	if err != nil {
		t.Fatal(err)
	}
	reports, err := engine.SolveInvalidations()
	if err != nil {
		t.Fatal(err)
	}
	gens := closureGenerations(t, reports)

	want := map[string]map[string]int{
		"inv-001": {"S2-orders": 0, "S3-order-items": 1, "S4-inventory": 2},
		"inv-002": {"S1-users": 0, "S2-orders": 1, "S3-order-items": 2},
		"inv-003": {"S4-inventory": 0, "S6-events": 1},
	}
	for eventID, expected := range want {
		for stmt, gen := range expected {
			got, ok := gens[eventID][stmt]
			if !ok || got != gen {
				t.Fatalf("%s expected %s generation %d, got map %+v", eventID, stmt, gen, gens[eventID])
			}
		}
		for stmt := range gens[eventID] {
			if _, ok := expected[stmt]; !ok {
				t.Fatalf("%s unexpectedly invalidates %s (generation %d)", eventID, stmt, gens[eventID][stmt])
			}
		}
	}

	for _, r := range reports {
		for _, item := range r.Items {
			if item.Source == "" || item.PlanID == "" || len(item.Path) == 0 {
				t.Fatalf("invalidation item missing source/plan/path: %+v", item)
			}
		}
	}
}

func TestReferenceCycleReturnsLocatableCode(t *testing.T) {
	p := GenerateProject(DefaultSeed)
	s1 := &p.Statements[0]
	lookup, ok := s1.FindNode("S1-table-lookup")
	if !ok {
		t.Fatal("missing lookup node")
	}
	lookup.Children = append(lookup.Children, "S1-cycle-ref")
	for i := range s1.Subplans {
		if s1.Subplans[i].ID == "main" {
			for j := range s1.Subplans[i].Nodes {
				if s1.Subplans[i].Nodes[j].ID == "S1-table-lookup" {
					s1.Subplans[i].Nodes[j].Children = lookup.Children
				}
			}
			s1.Subplans[i].Nodes = append(s1.Subplans[i].Nodes, Node{
				ID: "S1-cycle-ref", SubplanID: "main", Op: "SUBPLAN_REF",
				Ref:             &NodeRef{StatementID: "S6-events", SubplanID: "main"},
				EstimatedCostNS: 1_000_000,
			})
		}
	}
	s1.Fingerprint = ComputeFingerprint(*s1)
	_, err := SolveInvalidations(p, nil, nil)
	assertCode(t, err, CodeReferenceCycle)
}

func TestDanglingAndEvictedReferenceCodes(t *testing.T) {
	p := GenerateProject(DefaultSeed)
	p.Statements[4].Subplans[0].Nodes[1].Ref = &NodeRef{StatementID: "S404-missing", SubplanID: "main"}
	p.Statements[4].Fingerprint = ComputeFingerprint(p.Statements[4])
	_, err := SolveInvalidations(p, nil, nil)
	assertCode(t, err, CodeReferenceDangling)

	p = GenerateProject(DefaultSeed)
	p.Statements[4].Subplans[0].Nodes[1].Ref = &NodeRef{PlanID: "PLAN-EVICTED-X"}
	p.Statements[4].Fingerprint = ComputeFingerprint(p.Statements[4])
	all := map[string]CacheEntry{
		"PLAN-EVICTED-X": {ID: "PLAN-EVICTED-X", PlanID: "PLAN-EVICTED-X", Evicted: true},
	}
	err = AuditReferences(p, nil, all)
	assertCode(t, err, CodeReferenceEvicted)

	p = GenerateProject(DefaultSeed)
	active := []CacheEntry{{ID: "PLAN-S1-users", PlanID: "PLAN-S1-users", StatementID: "S1-users", Fingerprint: "fp_wrong"}}
	err = AuditReferences(p, active, map[string]CacheEntry{})
	assertCode(t, err, CodeFingerprintConflict)
}
