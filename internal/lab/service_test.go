package lab_test

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"ocrworkbench/internal/domain"
	"ocrworkbench/internal/lab"
)

func TestHTTPBridgeBackendEndToEnd(t *testing.T) {
	root := t.TempDir()
	service, appErr := lab.New(filepath.Join(root, "samples"), filepath.Join(root, "data", "workbench.db"), domain.DefaultBaseSeed, false)
	if appErr != nil {
		t.Fatalf("new service: %v", appErr)
	}
	defer service.Close()
	bootstrap, appErr := service.Bootstrap()
	if appErr != nil {
		t.Fatalf("bootstrap: %v", appErr)
	}
	if len(bootstrap.Manifest.Samples) != 5 {
		t.Fatalf("samples = %d", len(bootstrap.Manifest.Samples))
	}
	params := bootstrap.Defaults
	params.TileSize = 32
	if _, appErr := service.StartRun(lab.StartRunRequest{SampleID: bootstrap.Manifest.Samples[0].ID, Params: params, Mode: "live"}); appErr == nil || appErr.Code != domain.CodePara {
		t.Fatalf("expected E_PARA, got %v", appErr)
	}

	handle, appErr := service.StartRun(lab.StartRunRequest{
		SampleID: "sparse_single_960x540",
		Params:   bootstrap.Defaults,
		Mode:     "live",
	})
	if appErr != nil {
		t.Fatalf("start: %v", appErr)
	}
	var view domain.RunView
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		view, appErr = service.Snapshot(handle.JobID)
		if appErr != nil {
			t.Fatalf("snapshot: %v", appErr)
		}
		if view.Status == "completed" || view.Status == "failed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if view.Status != "completed" {
		t.Fatalf("status=%s error=%v", view.Status, view.Error)
	}
	if len(view.Lines) != 1 || view.Lines[0].Text != "STAGE-01" || view.Lines[0].Confidence <= 0 {
		t.Fatalf("unexpected lines: %+v", view.Lines)
	}
	if len(view.Commits) != len(view.Tiles)+1 {
		t.Fatalf("commits=%d tiles=%d", len(view.Commits), len(view.Tiles))
	}
	if view.Commits[0].Kind != "tile" || view.Commits[len(view.Commits)-1].Kind != "line" {
		t.Fatalf("unexpected commit order: %+v", view.Commits)
	}
	var tilePayload map[string]any
	if err := json.Unmarshal([]byte(view.Commits[0].PayloadJSON), &tilePayload); err != nil {
		t.Fatalf("decode tile commit payload: %v", err)
	}
	for _, field := range []string{"image_hash", "template_version", "param_version", "algorithm_version"} {
		if tilePayload[field] == nil || tilePayload[field] == "" {
			t.Fatalf("tile commit payload missing %s", field)
		}
	}
	var linePayload map[string]any
	if err := json.Unmarshal([]byte(view.Commits[len(view.Commits)-1].PayloadJSON), &linePayload); err != nil {
		t.Fatalf("decode line commit payload: %v", err)
	}
	for _, field := range []string{"image_hash", "template_version", "param_version", "algorithm_version"} {
		if linePayload[field] == nil || linePayload[field] == "" {
			t.Fatalf("line commit payload missing %s", field)
		}
	}
	export, appErr := service.Export(handle.JobID)
	if appErr != nil {
		t.Fatalf("export: %v", appErr)
	}
	if export["schema_version"] != "ocr-workbench-export/1.0" {
		t.Fatalf("export schema = %v", export["schema_version"])
	}
	image, appErr := service.SampleImage("sparse_single_960x540")
	if appErr != nil {
		t.Fatalf("sample image: %v", appErr)
	}
	if image["data_url"] == "" {
		t.Fatal("image data URL missing")
	}
}

func TestServiceThreePathCompare(t *testing.T) {
	root := t.TempDir()
	service, appErr := lab.New(filepath.Join(root, "samples"), filepath.Join(root, "data", "workbench.db"), domain.DefaultBaseSeed, false)
	if appErr != nil {
		t.Fatalf("new service: %v", appErr)
	}
	defer service.Close()
	bootstrap, appErr := service.Bootstrap()
	if appErr != nil {
		t.Fatalf("bootstrap: %v", appErr)
	}
	result, appErr := service.Compare(lab.StartRunRequest{
		SampleID: "sparse_gray_800x600",
		Params:   bootstrap.Defaults,
		Mode:     "reset",
	})
	if appErr != nil {
		t.Fatalf("compare: %v", appErr)
	}
	if !result.Equal || len(result.Paths) != 3 {
		t.Fatalf("comparison failed: %+v", result)
	}
}
