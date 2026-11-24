package workbench_test

import (
	"context"
	"path/filepath"
	"testing"

	"storyboardstudio/internal/dependency"
	"storyboardstudio/internal/model"
	"storyboardstudio/internal/sample"
	"storyboardstudio/internal/workbench"
)

func TestResumeAfterContentHashMismatchDiscardsAndRunsFull(t *testing.T) {
	wb, err := workbench.New(filepath.Join(t.TempDir(), "recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer wb.Close()
	project := sample.Generate(sample.DefaultSeeds()[0])
	if err := wb.AddProject(project); err != nil {
		t.Fatal(err)
	}
	if _, err := wb.RunCheckpointed(project.ID, "broken-session", 2); err != workbench.ErrInterrupted {
		t.Fatalf("expected interruption, got %v", err)
	}

	newDuration := int64(4242)
	if err := workbench.ApplyEdit(project, workbench.EditSpec{
		NodeID: "n06_epilogue", Kind: dependency.EditDuration, LocalIndex: 0, DurationMS: &newDuration,
	}); err != nil {
		t.Fatal(err)
	}

	report, err := wb.Resume(project.ID, "broken-session", -1)
	if err != nil {
		t.Fatal(err)
	}
	if !report.RecoveredFullRun || report.DiscardedSnapshot == nil || report.DiscardedSnapshot.Code != "E_HASH" {
		t.Fatalf("expected E_HASH full recovery, got %+v", report)
	}
	if len(report.Result.Shots) == 0 || report.Result.TerminalFlags["case_closed"] != true {
		t.Fatalf("recovery produced incomplete result: %+v", report.Result)
	}
}

func TestParallelVerificationUsesBoundedPool(t *testing.T) {
	projects := []*model.Project{
		sample.Generate(252001),
		sample.Generate(252002),
		sample.Generate(252003),
	}
	report, err := workbench.VerifyProjectsParallel(context.Background(), projects, t.TempDir(), 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Reports) != 3 {
		t.Fatalf("expected 3 reports, got %d", len(report.Reports))
	}
	if report.PeakParallel > 2 {
		t.Fatalf("pool exceeded bound: peak=%d", report.PeakParallel)
	}
	for _, item := range report.Reports {
		if !item.OK {
			t.Fatalf("project %s failed verification", item.ProjectID)
		}
	}
}
