package orchestrator

import (
	"errors"
	"reflect"
	"testing"
)

func TestGeneratedSchemasNormalizeEquivalently(t *testing.T) {
	scenario, err := GenerateScenario(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	for name, document := range map[string][]byte{
		"v1": scenario.V1,
		"v2": scenario.V2,
		"v3": scenario.V3,
	} {
		project, err := LoadProject(document)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !EquivalentProjects(project, scenario.Canonical) {
			t.Fatalf("%s did not normalize to canonical project", name)
		}
		explicit, _ := project.Node("a")
		if explicit.Timeout.Ticks != (Ratio{Num: 12, Den: 1}) || explicit.Timeout.Default {
			t.Fatalf("%s explicit timeout not normalized: %+v", name, explicit.Timeout)
		}
		omitted, _ := project.Node("f")
		if omitted.Timeout.Ticks != (Ratio{Num: 20, Den: 1}) || !omitted.Timeout.Default {
			t.Fatalf("%s omitted timeout did not use default: %+v", name, omitted.Timeout)
		}
	}
}

func TestGeneratorIsDeterministicForMultipleSeeds(t *testing.T) {
	for _, seed := range []int64{0, 1, 7, DefaultSeed, -9} {
		first, err := GenerateScenario(seed)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		second, err := GenerateScenario(seed)
		if err != nil {
			t.Fatalf("seed %d repeat: %v", seed, err)
		}
		if !reflect.DeepEqual(first.V1, second.V1) || !reflect.DeepEqual(first.V2, second.V2) || !reflect.DeepEqual(first.V3, second.V3) {
			t.Fatalf("seed %d did not produce byte-identical schemas", seed)
		}
	}
}

func TestSolverMatchesTruthAcrossSchemas(t *testing.T) {
	scenario, err := GenerateScenario(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	projects := []*Project{}
	for _, document := range [][]byte{scenario.V1, scenario.V2, scenario.V3} {
		project, err := LoadProject(document)
		if err != nil {
			t.Fatal(err)
		}
		projects = append(projects, project)
	}

	var baseline *RunResult
	for index, project := range projects {
		solver, err := NewSolver(project)
		if err != nil {
			t.Fatal(err)
		}
		result, err := solver.Solve()
		if err != nil {
			t.Fatalf("schema %d: %v", index+1, err)
		}
		if baseline == nil {
			baseline = result
			if err := VerifyTruth(result, scenario.Truth); err != nil {
				t.Fatal(err)
			}
		} else if !reflect.DeepEqual(baseline, result) {
			t.Fatalf("schema %d produced a different run result", index+1)
		}
	}
}

func TestTimeoutBoundaryAndUpgrade(t *testing.T) {
	project := testProject()
	solver, err := NewSolver(project)
	if err != nil {
		t.Fatal(err)
	}
	result, err := solver.Solve()
	if err != nil {
		t.Fatal(err)
	}
	// Timeout 5/2 ticks becomes a deadline at tick 3 for a probe at tick 0.
	if result.Ticks[1].Domains[0].Forced {
		t.Fatal("domain was forced before the exact boundary")
	}
	domain := result.Ticks[3].Domains[0]
	if !domain.Forced || domain.UpgradedTick == nil || *domain.UpgradedTick != 3 {
		t.Fatalf("domain was not forced exactly at tick 3: %+v", domain)
	}
	// Commit at tick 7 wins over a deadline that would have been exactly 7.
	project2 := testProject()
	project2.Nodes[0].Timeout = Timeout{Set: true, Ticks: Ratio{Num: 7, Den: 1}}
	project2.Nodes[1].Timeout = Timeout{Set: true, Ticks: Ratio{Num: 7, Den: 1}}
	project2.Events = []Event{
		{Tick: 0, Seq: 1, Kind: EventProbe, NodeID: "n1"},
		{Tick: 7, Seq: 1, Kind: EventCommit, DomainID: int64Ptr(1)},
	}
	if err := project2.Normalize(); err != nil {
		t.Fatal(err)
	}
	solver2, err := NewSolver(project2)
	if err != nil {
		t.Fatal(err)
	}
	result2, err := solver2.Solve()
	if err != nil {
		t.Fatal(err)
	}
	for _, tick := range result2.Ticks {
		for _, domain := range tick.Domains {
			if domain.Forced {
				t.Fatalf("commit at deadline should win, got forced domain at tick %d", tick.Tick)
			}
		}
	}
	// A declared zero timeout means the isolation is immediately eligible for
	// forced upgrade at the end of the probe tick.
	project3 := testProject()
	project3.Nodes[0].Timeout = Timeout{Set: true, Ticks: Ratio{Num: 0, Den: 1}}
	project3.Nodes[1].Timeout = Timeout{Set: true, Ticks: Ratio{Num: 0, Den: 1}}
	project3.Events = []Event{{Tick: 0, Seq: 1, Kind: EventProbe, NodeID: "n1"}}
	if err := project3.Normalize(); err != nil {
		t.Fatal(err)
	}
	solver3, err := NewSolver(project3)
	if err != nil {
		t.Fatal(err)
	}
	result3, err := solver3.Solve()
	if err != nil {
		t.Fatal(err)
	}
	if len(result3.Ticks) == 0 || len(result3.Ticks[0].Domains) == 0 || !result3.Ticks[0].Domains[0].Forced {
		t.Fatal("zero timeout did not upgrade at probe tick")
	}
}

func TestDependencyCycleReport(t *testing.T) {
	project := testProject()
	project.Edges = []Edge{{From: "n1", To: "n2"}, {From: "n2", To: "n1"}}
	if err := project.Normalize(); err != nil {
		t.Fatal(err)
	}
	_, err := NewSolver(project)
	var cycle *CycleError
	if !errors.As(err, &cycle) {
		t.Fatalf("expected cycle error, got %v", err)
	}
	if len(cycle.Cycles) != 1 || !stringSlicesEqual(cycle.Cycles[0], []string{"n1", "n2", "n1"}) {
		t.Fatalf("unexpected cycle report: %v", cycle.Cycles)
	}
}

func TestUnsupportedSchemaReturnsStableBusinessCode(t *testing.T) {
	_, err := LoadProject([]byte(`{"schema":"cluster.v99","project_id":"p"}`))
	if CodeOf(err) != CodeUnsupportedSchema {
		t.Fatalf("code = %s, want %s (%v)", CodeOf(err), CodeUnsupportedSchema, err)
	}
	_, err = LoadProject([]byte(`{"schema_version":"cluster.v1","schema":"cluster/v2"}`))
	if CodeOf(err) != CodeUnsupportedSchema {
		t.Fatalf("conflicting schema code = %s, want %s (%v)", CodeOf(err), CodeUnsupportedSchema, err)
	}
}

func testProject() *Project {
	return &Project{
		SchemaVersion:         "test",
		ProjectID:             "p",
		SnapshotID:            "s",
		Revision:              "r",
		TickMillis:            Ratio{Num: 5, Den: 2},
		TimeoutBits:           32,
		DefaultTimeout:        Timeout{Set: true, Wire: 5, Unit: UnitTick},
		WorkerBudget:          2,
		PerDomainPendingLimit: 2,
		Nodes: []Node{
			{ID: "n1", Rack: "r1", Zone: 1, Workloads: []Workload{{ID: "w1", BudgetBytes: 10, UsedBytes: 0}}, Timeout: Timeout{Set: true, Ticks: Ratio{Num: 5, Den: 2}}},
			{ID: "n2", Rack: "r1", Zone: 1, Workloads: []Workload{{ID: "w2", BudgetBytes: 10, UsedBytes: 0}}, Timeout: Timeout{Set: true, Ticks: Ratio{Num: 5, Den: 2}}},
		},
		Events: []Event{
			{Tick: 0, Seq: 1, Kind: EventProbe, NodeID: "n1"},
			{Tick: 7, Seq: 1, Kind: EventRequest, WorkloadID: "w1", AmountBytes: 1},
		},
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
