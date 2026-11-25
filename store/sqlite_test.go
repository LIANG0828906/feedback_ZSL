package store

import (
	"context"
	"path/filepath"
	"testing"

	"wardrobe/assembly"
	"wardrobe/diagnostics"
	"wardrobe/domain"
	"wardrobe/generator"
	"wardrobe/planner"
)

func TestCheckpointRoundTripAndColumns(t *testing.T) {
	project, result := sampleCase(t, 0)
	path := filepath.Join(t.TempDir(), "checkpoints.db")
	sqliteStore, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer sqliteStore.Close()
	if err := sqliteStore.SaveCase(context.Background(), project, result); err != nil {
		t.Fatal(err)
	}
	loaded, err := sqliteStore.LoadCase(context.Background(), project.ProjectID, result.CaseID,
		result.InputHash, project.Constraints.Version, domain.AlgorithmVersion)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Recovered || loaded.Result == nil {
		t.Fatalf("expected recovery, got %+v", loaded)
	}
	if mismatch := diagnostics.CompareResults(result, *loaded.Result); mismatch != nil {
		t.Fatal(mismatch)
	}
	var rows int
	if err := sqliteStore.db.QueryRow(`SELECT COUNT(*) FROM batch_snapshots WHERE project_id=? AND case_id=?`,
		project.ProjectID, result.CaseID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != len(result.Batches) {
		t.Fatalf("rows=%d batches=%d", rows, len(result.Batches))
	}
	var partIDs, placements, budget, dependencies, contentHash, constraintVersion, algorithmVersion, cacheKey string
	err = sqliteStore.db.QueryRow(`SELECT part_ids_json, placements_json, budget_json, dependencies_json,
		content_hash, constraint_version, algorithm_version, cache_key
		FROM batch_snapshots WHERE project_id=? AND case_id=? ORDER BY commit_sequence LIMIT 1`,
		project.ProjectID, result.CaseID).Scan(&partIDs, &placements, &budget, &dependencies,
		&contentHash, &constraintVersion, &algorithmVersion, &cacheKey)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"partIds": partIDs, "placements": placements, "budget": budget, "dependencies": dependencies,
		"contentHash": contentHash, "constraintVersion": constraintVersion,
		"algorithmVersion": algorithmVersion, "cacheKey": cacheKey,
	} {
		if value == "" {
			t.Fatalf("column %s is empty", name)
		}
	}
}

func TestPartialCheckpointIsInvalidatedAsAUnit(t *testing.T) {
	project, result := sampleCase(t, 1)
	sqliteStore, err := Open(filepath.Join(t.TempDir(), "partial.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqliteStore.Close()
	if err := sqliteStore.SaveCase(context.Background(), project, result); err != nil {
		t.Fatal(err)
	}
	if err := sqliteStore.DebugSetStatus(context.Background(), project.ProjectID, result.CaseID, StatusBuilding); err != nil {
		t.Fatal(err)
	}
	loaded, err := sqliteStore.LoadCase(context.Background(), project.ProjectID, result.CaseID,
		result.InputHash, project.Constraints.Version, domain.AlgorithmVersion)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Invalidated || loaded.Recovered {
		t.Fatalf("partial checkpoint was not invalidated: %+v", loaded)
	}
	again, err := sqliteStore.LoadCase(context.Background(), project.ProjectID, result.CaseID,
		result.InputHash, project.Constraints.Version, domain.AlgorithmVersion)
	if err != nil {
		t.Fatal(err)
	}
	if again.Found {
		t.Fatalf("invalidated checkpoint was not deleted: %+v", again)
	}
}

func TestVersionAndContentHashMismatchInvalidateWholeSet(t *testing.T) {
	project, result := sampleCase(t, 2)
	sqliteStore, err := Open(filepath.Join(t.TempDir(), "version.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqliteStore.Close()
	if err := sqliteStore.SaveCase(context.Background(), project, result); err != nil {
		t.Fatal(err)
	}
	loaded, err := sqliteStore.LoadCase(context.Background(), project.ProjectID, result.CaseID,
		result.InputHash, "different-constraint-version", domain.AlgorithmVersion)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Invalidated {
		t.Fatalf("version mismatch was not invalidated: %+v", loaded)
	}
	if err := sqliteStore.SaveCase(context.Background(), project, result); err != nil {
		t.Fatal(err)
	}
	if _, err := sqliteStore.db.Exec(`UPDATE batch_snapshots SET content_hash='tampered'
		WHERE project_id=? AND case_id=? AND commit_sequence=2`, project.ProjectID, result.CaseID); err != nil {
		t.Fatal(err)
	}
	loaded, err = sqliteStore.LoadCase(context.Background(), project.ProjectID, result.CaseID,
		result.InputHash, project.Constraints.Version, domain.AlgorithmVersion)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Invalidated {
		t.Fatalf("content hash mismatch was not invalidated: %+v", loaded)
	}
}

func sampleCase(t *testing.T, projectIndex int) (domain.Project, domain.CaseResult) {
	t.Helper()
	bundle := generator.Generate(generator.DefaultSeed)
	project := bundle.Projects[projectIndex]
	truthCase := project.Truth.Cases[0]
	evaluated, err := assembly.Evaluate(project, truthCase.SelectedPartIDs)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.Plan(project, evaluated.AcceptedPartIDs)
	if err != nil {
		t.Fatal(err)
	}
	result := domain.CaseResult{
		CaseID:            truthCase.CaseID,
		Description:       truthCase.Description,
		SelectedPartIDs:   evaluated.SelectedPartIDs,
		Valid:             evaluated.Valid,
		AcceptedPartIDs:   evaluated.AcceptedPartIDs,
		Conflicts:         evaluated.Conflicts,
		Batches:           plan.Batches,
		Decisions:         plan.Decisions,
		CommitSequence:    plan.CommitSequence,
		InputHash:         domain.CaseInputHash(project, evaluated.SelectedPartIDs),
		ConstraintVersion: project.Constraints.Version,
		AlgorithmVersion:  domain.AlgorithmVersion,
		Source:            "test",
	}
	return project, result
}
