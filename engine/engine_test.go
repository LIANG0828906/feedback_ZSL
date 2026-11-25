package engine_test

import (
	"path/filepath"
	"testing"

	"wardrobe/diagnostics"
	"wardrobe/engine"
	"wardrobe/generator"
	"wardrobe/store"
)

func TestTriCompareAfterLatePartEdit(t *testing.T) {
	bundle := generator.Generate(generator.DefaultSeed)
	project := bundle.Projects[0]
	engineUnderTest, err := engine.New(bundle.Projects, engine.Config{})
	if err != nil {
		t.Fatal(err)
	}
	truthCase := project.Truth.Cases[0]
	report, err := engineUnderTest.TriCompare(project.ProjectID, truthCase.CaseID, engine.EditRequest{
		PartID:       "hand_r_glove",
		ScalePercent: 90,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Equal {
		t.Fatalf("three paths differ: inc=%v recovery=%v first=%v", report.IncrementalMismatch, report.RecoveryMismatch, report.FirstMismatch)
	}
	if len(report.Paths) != 3 {
		t.Fatalf("expected 3 paths, got %d", len(report.Paths))
	}
}

func TestIncrementalEditReusesPrefixAndMatchesFull(t *testing.T) {
	bundle := generator.Generate(generator.DefaultSeed)
	project := bundle.Projects[1]
	truthCase := project.Truth.Cases[0]
	engineUnderTest, err := engine.New(bundle.Projects, engine.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engineUnderTest.BuildCase(project.ProjectID, truthCase.CaseID); err != nil {
		t.Fatal(err)
	}
	report, err := engineUnderTest.EditPart(engine.EditRequest{
		ProjectID:    project.ProjectID,
		CaseID:       truthCase.CaseID,
		PartID:       "hand_r_glove",
		MaterialSlot: "mat_leather",
		ScalePercent: 92,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Result.Source != "incremental" {
		t.Fatalf("expected incremental source, got %s; applied=%v", report.Result.Source, report.Applied)
	}
	if len(report.ReusedBatchIDs) == 0 {
		t.Fatal("expected prefix batches to be reused")
	}
	if len(report.InvalidatedBatchIDs) == 0 {
		t.Fatal("expected suffix batches to be invalidated")
	}
	if report.Result.Batches[0].Source != "reused" {
		t.Fatalf("batch 1 should be reused, got %s", report.Result.Batches[0].Source)
	}
	triEngine, err := engine.New(bundle.Projects, engine.Config{})
	if err != nil {
		t.Fatal(err)
	}
	fullReport, err := triEngine.TriCompare(project.ProjectID, truthCase.CaseID, engine.EditRequest{
		PartID:       "hand_r_glove",
		MaterialSlot: "mat_leather",
		ScalePercent: 92,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !fullReport.Equal {
		t.Fatalf("three paths differ: %+v", fullReport.FirstMismatch)
	}
}

func TestSQLiteRecoveryMatchesFull(t *testing.T) {
	bundle := generator.Generate(generator.DefaultSeed)
	project := bundle.Projects[2]
	truthCase := project.Truth.Cases[1]
	path := filepath.Join(t.TempDir(), "checkpoints.db")
	sqliteStore, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := engine.New(bundle.Projects, engine.Config{Store: sqliteStore})
	if err != nil {
		t.Fatal(err)
	}
	full, err := first.BuildCase(project.ProjectID, truthCase.CaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqliteStore.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	recoveryEngine, err := engine.New(bundle.Projects, engine.Config{Store: reopened})
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := recoveryEngine.RecoverCase(project.ProjectID, truthCase.CaseID)
	if err != nil {
		t.Fatal(err)
	}
	if !recovery.Recovered {
		t.Fatalf("expected recovery, got found=%t invalidated=%t reason=%s", recovery.Found, recovery.Invalidated, recovery.Reason)
	}
	if mismatch := diagnostics.CompareResults(full, recovery.Result); mismatch != nil {
		t.Fatal(mismatch)
	}
}

func TestEnsureCaseReusesReadyCheckpointAfterRestart(t *testing.T) {
	bundle := generator.Generate(generator.DefaultSeed)
	project := bundle.Projects[0]
	truthCase := project.Truth.Cases[0]
	path := filepath.Join(t.TempDir(), "ensure.db")
	firstStore, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := engine.New(bundle.Projects, engine.Config{Store: firstStore})
	if err != nil {
		t.Fatal(err)
	}
	full, err := first.BuildCase(project.ProjectID, truthCase.CaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := firstStore.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted, err := engine.New(bundle.Projects, engine.Config{Store: reopened})
	if err != nil {
		t.Fatal(err)
	}
	reused, err := restarted.EnsureCase(project.ProjectID, truthCase.CaseID)
	if err != nil {
		t.Fatal(err)
	}
	if reused.Source != "sqlite_recovery" {
		t.Fatalf("expected SQLite reuse after restart, got source=%s", reused.Source)
	}
	if mismatch := diagnostics.CompareResults(full, reused); mismatch != nil {
		t.Fatal(mismatch)
	}
}

func TestMaterialVersionChangeInvalidatesEarlierDependentBatch(t *testing.T) {
	bundle := generator.Generate(generator.DefaultSeed)
	project := bundle.Projects[0]
	truthCase := project.Truth.Cases[0]
	engineUnderTest, err := engine.New(bundle.Projects, engine.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engineUnderTest.BuildCase(project.ProjectID, truthCase.CaseID); err != nil {
		t.Fatal(err)
	}
	report, err := engineUnderTest.EditPart(engine.EditRequest{
		ProjectID:    project.ProjectID,
		CaseID:       truthCase.CaseID,
		PartID:       "hand_r_glove",
		MaterialSlot: "mat_cloth",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.ReusedBatchIDs) != 0 {
		t.Fatalf("material version dependency should invalidate from B001, reused=%v", report.ReusedBatchIDs)
	}
	if len(report.InvalidatedBatchIDs) != len(truthCase.Batches) {
		t.Fatalf("expected all batches invalidated, got %v", report.InvalidatedBatchIDs)
	}
}
