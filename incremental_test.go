package dressbatch

import (
	"testing"
)

func TestIncrementalLayoutEditMatchesFullRepack(t *testing.T) {
	project := testProject(t)
	candidate := testLegalCandidate(t, project)
	previous, err := Pack(project, candidate)
	if err != nil {
		t.Fatal(err)
	}
	mutated, target := mutateLastBatchLayout(t, project, previous)
	result, err := IncrementalRepack(IncrementalRequest{
		OldProject:       project,
		NewProject:       mutated,
		Previous:         previous,
		CandidatePartIDs: candidate,
	})
	if err != nil {
		t.Fatalf("IncrementalRepack: %v", err)
	}
	if len(result.Invalidation.Entries) == 0 {
		t.Fatal("expected an invalidation entry")
	}
	found := false
	for _, entry := range result.Invalidation.Entries {
		for _, partID := range entry.AffectedParts {
			if partID == target {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("target part %s missing from invalidation report: %+v", target, result.Invalidation)
	}
	full, err := Pack(mutated, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := ComparePackResults(full, result.Result); err != nil {
		t.Fatalf("incremental and full results differ: %v", err)
	}
	if len(result.Invalidation.ReusedBatchIDs) == 0 {
		t.Fatal("expected at least one reused batch for a last-batch layout edit")
	}
}

func TestIncrementalNoChangeReusesAll(t *testing.T) {
	project := testProject(t)
	candidate := testLegalCandidate(t, project)
	previous, err := Pack(project, candidate)
	if err != nil {
		t.Fatal(err)
	}
	result, err := IncrementalRepack(IncrementalRequest{
		OldProject:       project,
		NewProject:       cloneProject(t, project),
		Previous:         previous,
		CandidatePartIDs: candidate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Invalidation.Entries) != 0 {
		t.Fatalf("unexpected invalidations: %+v", result.Invalidation)
	}
	if len(result.Invalidation.ReusedBatchIDs) != len(previous.Batches) {
		t.Fatalf("all batches should be reused")
	}
}

func TestIncrementalDiffReportsFirstBatchAndField(t *testing.T) {
	project := testProject(t)
	candidate := testLegalCandidate(t, project)
	previous, err := Pack(project, candidate)
	if err != nil {
		t.Fatal(err)
	}
	tampered := clonePackResult(t, previous)
	tampered.Batches[0].CacheKey = "tampered"
	_, err = IncrementalRepack(IncrementalRequest{
		OldProject:       project,
		NewProject:       cloneProject(t, project),
		Previous:         tampered,
		CandidatePartIDs: candidate,
		ReusedPrefix:     tampered.Batches,
	})
	if err == nil {
		t.Fatal("expected E_DIFF")
	}
	domainErr, ok := AsDomainError(err)
	if !ok || domainErr.Code != CodeDiff {
		t.Fatalf("expected E_DIFF, got %v", err)
	}
	if domainErr.ObjectID != previous.Batches[0].ID || domainErr.Field != "cache_key" {
		t.Fatalf("unexpected diff location: %s %s", domainErr.ObjectID, domainErr.Field)
	}
}

func TestFitChangeInvalidatesDecisionBatches(t *testing.T) {
	project := testProject(t)
	candidate := testLegalCandidate(t, project)
	previous, err := Pack(project, candidate)
	if err != nil {
		t.Fatal(err)
	}
	mutated, target := mutateLastBatchLayout(t, project, previous)
	// A material slot change affects fit decisions, even when the part would be
	// deferred by earlier batches.
	for i := range mutated.Parts {
		if mutated.Parts[i].ID == target {
			mutated.Parts[i].MaterialSlots = []string{"cloth", "metal"}
			break
		}
	}
	changes, err := DiffProjects(project, mutated)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(changes.FitAffectedParts, target) {
		t.Fatalf("fit change not recorded: %+v", changes)
	}
}

func TestCacheKeyDependsOnPartsMaterialsConstraintsAndUpstream(t *testing.T) {
	project := testProject(t)
	result, _ := testPack(t, project)
	batch := result.Batches[0]
	base := BatchCacheKey(project, batch, "upstream-a")
	if base == BatchCacheKey(project, batch, "upstream-b") {
		t.Fatal("upstream cache key is not part of the batch key")
	}

	changedPart := cloneProject(t, project)
	changedPart.Parts[0].Submeshes[0].VertexCount--
	if base == BatchCacheKey(changedPart, batch, "upstream-a") {
		t.Fatal("part content hash is not part of the batch key")
	}

	changedMaterial := cloneProject(t, project)
	changedMaterial.Materials[0].Version = "2"
	if base == BatchCacheKey(changedMaterial, batch, "upstream-a") {
		t.Fatal("material version is not part of the batch key")
	}

	changedConstraints := cloneProject(t, project)
	changedConstraints.Constraints.MaxVerticesPerBatch++
	if base == BatchCacheKey(changedConstraints, batch, "upstream-a") {
		t.Fatal("constraint version is not part of the batch key")
	}
}
