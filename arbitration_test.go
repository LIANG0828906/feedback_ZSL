package macrokit

import (
	"strings"
	"testing"
)

func TestPriorityAdjudicationAndConflictInjection(t *testing.T) {
	set := GenerateSamples(FixedSampleSeed)
	sample := findSample(t, set, "priority-conflict")
	result, err := RunProject(sample.Project, sample.Events, ExecutorConfig{FrameCount: sample.Truth.FrameCount})
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyTruth(result, sample.Truth); err != nil {
		t.Fatal(err)
	}
	if len(result.Adjudications) != 1 {
		t.Fatalf("adjudications = %d, want 1: %+v", len(result.Adjudications), result.Adjudications)
	}
	adj := result.Adjudications[0]
	if adj.Frame != 3 || adj.WinnerID != "p_high" || adj.SuppressedID != "p_low" {
		t.Fatalf("unexpected adjudication: %+v", adj)
	}
	if adj.Basis == "" || !strings.Contains(adj.Basis, "priority") {
		t.Fatalf("basis should explain priority: %q", adj.Basis)
	}

	for _, branch := range sample.Project.Branches {
		if branch.ID == "p_high" || branch.ID == "p_low" {
			branch.Priority = 7
			branch.HasPriority = true
		}
	}
	_, err = RunProject(sample.Project, sample.Events, ExecutorConfig{
		FrameCount: sample.Truth.FrameCount, Arbitration: ArbitrationStrict,
	})
	if !HasCode(err, EConflict) {
		t.Fatalf("expected E_CONFLICT under strict equal priority, got %v", err)
	}
	domainErr, _ := AsError(err)
	if !contains(domainErr.ConflictSet, "p_high") || !contains(domainErr.ConflictSet, "p_low") {
		t.Fatalf("conflict set missing branches: %+v", domainErr.ConflictSet)
	}
}

func TestDeterministicStableIDTieBreak(t *testing.T) {
	set := GenerateSamples(FixedSampleSeed + 1)
	sample := findSample(t, set, "priority-conflict")
	for _, branch := range sample.Project.Branches {
		if branch.ID == "p_high" || branch.ID == "p_low" {
			branch.Priority = 10
			branch.HasPriority = true
		}
	}
	result, err := RunProject(sample.Project, sample.Events, ExecutorConfig{FrameCount: sample.Truth.FrameCount})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Adjudications) != 1 {
		t.Fatalf("expected one adjudication, got %+v", result.Adjudications)
	}
	if result.Adjudications[0].WinnerID != "p_high" {
		t.Fatalf("stable ID winner = %s", result.Adjudications[0].WinnerID)
	}
	if !strings.Contains(result.Adjudications[0].Basis, "stable_id") {
		t.Fatalf("basis = %q", result.Adjudications[0].Basis)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
