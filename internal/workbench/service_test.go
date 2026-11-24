package workbench_test

import (
	"os"
	"path/filepath"
	"testing"

	"occlusionlab/internal/model"
	"occlusionlab/internal/workbench"
)

func TestBootstrapStepRestartAndValidate(t *testing.T) {
	dataDir := t.TempDir()
	first := workbench.NewService(dataDir)
	dash, appErr := first.Bootstrap()
	if appErr != nil {
		t.Fatal(appErr)
	}
	if dash.Progress.Committed != 0 || dash.Progress.Total != 6 {
		t.Fatalf("unexpected initial progress: %+v", dash.Progress)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "projects", "current", "manifest.json")); err != nil {
		t.Fatal("project bundle was not generated")
	}
	dash, appErr = first.Step()
	if appErr != nil {
		t.Fatal(appErr)
	}
	if dash.Progress.Committed != 1 || dash.Snapshot.Count != 1 {
		t.Fatalf("step did not commit one window: %+v %+v", dash.Progress, dash.Snapshot)
	}
	if _, appErr := first.MoveCamera(0, 10, 0, 0); appErr == nil || appErr.Code != model.CodeState {
		t.Fatalf("committed window accepted camera adjustment: %+v", appErr)
	}
	if _, appErr := first.MoveCamera(2, 10, 0, 0); appErr != nil {
		t.Fatalf("uncommitted camera adjustment failed: %+v", appErr)
	}
	if _, appErr := first.SelectWindow(0); appErr != nil {
		t.Fatal(appErr)
	}
	dash, appErr = first.Dashboard()
	if appErr != nil {
		t.Fatal(appErr)
	}
	frozenHash := dash.Selected.Chunks[0].Status + "|" + dash.Selected.ParamsHash
	dash, appErr = first.UpdateParams(workbench.RuntimeOptions{RasterResolution: 96, MaxWorkers: 2})
	if appErr != nil {
		t.Fatal(appErr)
	}
	if dash.Selected.Chunks[0].Status+"|"+dash.Selected.ParamsHash != frozenHash {
		t.Fatal("committed window changed after parameter adjustment")
	}
	dash, appErr = first.Step()
	if appErr != nil {
		t.Fatal(appErr)
	}
	if dash.Progress.Committed != 2 || dash.Snapshot.Count != 2 {
		t.Fatal("second window did not commit")
	}
	first.Close()

	second := workbench.NewService(dataDir)
	defer second.Close()
	restored, appErr := second.Bootstrap()
	if appErr != nil {
		t.Fatal(appErr)
	}
	if restored.Progress.Committed != 2 || restored.Snapshot.Count != 2 {
		t.Fatalf("restart did not resume from latest committed window: %+v %+v",
			restored.Progress, restored.Snapshot)
	}
	if restored.SelectedWindow != 1 {
		t.Fatalf("restart selected window %d, want 1", restored.SelectedWindow)
	}
	report, appErr := second.Validate()
	if appErr != nil {
		t.Fatal(appErr)
	}
	if !report.Passed {
		t.Fatalf("validation failed after restart: %+v", report.Checks)
	}
}
