package main

import (
	"context"
	"path/filepath"
	"testing"

	"stageworkbench/pkg/generator"
	"stageworkbench/pkg/stage"
)

func testApp(t *testing.T) (*App, *stage.Project) {
	t.Helper()
	project, err := generator.Generate(generator.Options{Preset: "tiny", Seed: 123})
	if err != nil {
		t.Fatal(err)
	}
	params := stage.DefaultParams()
	params.WindowMS = project.WindowMS
	evaluator, err := stage.NewEvaluator(project, params)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	app := &App{
		ctx:       context.Background(),
		project:   project,
		params:    params,
		evaluator: evaluator,
		workDir:   dir,
		dbPath:    filepath.Join(dir, "workbench.db"),
	}
	return app, project
}

func TestEditAfterCheckpointRebasesAndResumeMatchesFull(t *testing.T) {
	app, project := testApp(t)
	state, err := app.Run(string(stage.RunRealtime), 1)
	if err != nil {
		t.Fatalf("initial partial run: %v", err)
	}
	if state.Progress.Committed != 1 {
		t.Fatalf("expected one committed window, got %+v", state.Progress)
	}
	committedEnd := committedEndMS(app.lastRun)
	if committedEnd == 0 {
		t.Fatal("missing committed horizon")
	}

	committedCue := project.Cues[0]
	_, err = app.EditCue(committedCue.ID, CuePatch{Priority: intPtr(committedCue.Priority + 1)})
	if domain, ok := stage.AsDomainError(err); !ok || domain.Code != stage.CodeCommitted {
		t.Fatalf("expected E_COMMITTED for committed cue edit, got %v", err)
	}

	var editCue *stage.Cue
	for i := range app.project.Cues {
		cue := &app.project.Cues[i]
		if cue.TimeMS*1000 >= committedEnd && cue.TimeMS+10 <= app.project.DurationMS {
			editCue = cue
			break
		}
	}
	if editCue == nil {
		t.Fatal("no future cue available for edit test")
	}
	newTime := editCue.TimeMS + 10
	state, err = app.EditCue(editCue.ID, CuePatch{TimeMS: &newTime})
	if err != nil {
		t.Fatalf("future edit: %v", err)
	}
	if state.LastNotice == "" {
		t.Fatal("rebase was not surfaced to the UI")
	}
	if state.Progress.Committed != 1 {
		t.Fatalf("edit should preserve committed windows, got %+v", state.Progress)
	}

	resumedState, err := app.Resume()
	if err != nil {
		t.Fatalf("resume after rebase: %v", err)
	}
	if resumedState.Progress.Committed != resumedState.Progress.Total {
		t.Fatalf("resume did not complete: %+v", resumedState.Progress)
	}
	resumed := *app.lastRun

	if _, err := app.FullRecompute(); err != nil {
		t.Fatalf("full recompute: %v", err)
	}
	full := *app.lastRun
	if err := stage.ComparePathSnapshots(resumed.Snapshot(), full.Snapshot()); err != nil {
		t.Fatalf("rebase/resume diverged from full recompute: %v", err)
	}
}

func TestGroupEditRejectsCommittedDependency(t *testing.T) {
	app, _ := testApp(t)
	if _, err := app.Run(string(stage.RunRealtime), 1); err != nil {
		t.Fatal(err)
	}
	targetGroup := app.project.Groups[0].ID
	_, err := app.EditGroup(targetGroup, GroupPatch{Priority: intPtr(app.project.Groups[0].Priority + 1)})
	if domain, ok := stage.AsDomainError(err); !ok || domain.Code != stage.CodeCommitted {
		t.Fatalf("expected E_COMMITTED for committed group edit, got %v", err)
	}
}

func intPtr(v int) *int { return &v }
