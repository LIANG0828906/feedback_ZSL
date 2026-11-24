package service_test

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"strata/internal/generator"
	"strata/internal/model"
	"strata/internal/render"
	"strata/internal/service"
)

func newService(t *testing.T) (*service.Service, string) {
	t.Helper()
	root := t.TempDir()
	if _, err := generator.Generate(195, filepath.Join(root, "data", "samples")); err != nil {
		t.Fatal(err)
	}
	svc, err := service.New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = svc.Close()
	})
	return svc, root
}

func TestPreviewExactIncrementalAndStateBoundary(t *testing.T) {
	svc, _ := newService(t)
	boot := svc.Bootstrap()
	if !boot.OK {
		t.Fatalf("bootstrap: %+v", boot.Error)
	}
	ws := boot.Data.(service.WorkspaceDTO)
	if ws.Version != 1 || ws.Stats.SourcePoints != 1837 || ws.Stats.Faces != 3456 || ws.Stats.Chunks != 12 {
		t.Fatalf("unexpected workspace: %+v", ws.Stats)
	}
	preview := svc.Preview(service.PreviewRequest{})
	if !preview.OK {
		t.Fatalf("preview: %+v", preview.Error)
	}
	previewData := preview.Data.(map[string]any)
	previewFrame := previewData["frame"].(service.FrameDTO)
	if previewFrame.LatencyMs >= 50 || !previewFrame.WithinBudget {
		t.Fatalf("preview latency %.3f", previewFrame.LatencyMs)
	}
	exact := svc.Exact(service.PreviewRequest{})
	if !exact.OK {
		t.Fatalf("exact: %+v", exact.Error)
	}
	exactData := exact.Data.(*service.ExactResult)
	if exactData.Comparison == nil || !exactData.Comparison.Passed ||
		exactData.Comparison.PreviewVersion != 1 || exactData.Comparison.ExactVersion != 1 {
		t.Fatalf("comparison mismatch: %+v", exactData.Comparison)
	}
	if exactData.FrozenChunks != 12 {
		t.Fatalf("frozen chunks=%d", exactData.FrozenChunks)
	}

	edit := svc.EditAndIncremental(service.EditRequest{Index: 0, DeltaZ: 0.02, BaseVersion: 1})
	if !edit.OK {
		t.Fatalf("incremental edit: %+v", edit.Error)
	}
	editData := edit.Data.(map[string]any)
	inc := editData["incremental"].(service.IncrementalResult)
	if inc.Version != 2 || inc.Manifest.Rebuilt != 4 || inc.Manifest.Reused != 8 {
		t.Fatalf("unexpected incremental result: %+v", inc.Manifest)
	}
	stale := svc.EditAndIncremental(service.EditRequest{Index: 1, DeltaZ: 0.01, BaseVersion: 1})
	if stale.OK || stale.Error == nil || stale.Error.Code != model.ESTATE {
		t.Fatalf("stale edit must return E_STATE: %+v", stale)
	}
	after := svc.Bootstrap().Data.(service.WorkspaceDTO)
	if after.Version != 2 {
		t.Fatalf("stale edit changed version to %d", after.Version)
	}
}

func TestVerifyPathsAndExport(t *testing.T) {
	svc, _ := newService(t)
	paths := svc.VerifyPaths()
	if !paths.OK {
		t.Fatalf("verify paths: %+v", paths.Error)
	}
	result := paths.Data.(*service.PathResult)
	if !result.Passed || result.FullMeshHash != result.IncrementalMeshHash ||
		result.FullMeshHash != result.ResumeMeshHash || result.FullMeshHash != result.PreviewMeshHash {
		t.Fatalf("path hashes differ: %+v", result)
	}
	if len(result.FullSequence) != 12 || len(result.ResumeSequence) != 12 {
		t.Fatalf("checkpoint sequence length mismatch")
	}
	export := svc.Export(service.ExportRequest{VersionID: model.VersionPCBin, Kind: "mesh"})
	if !export.OK {
		t.Fatalf("export: %+v", export.Error)
	}
	exportData := export.Data.(service.ExportResult)
	if exportData.ReadbackPoints != 2028 || exportData.ReadbackFaces != 3456 || !exportData.RoundTrip {
		t.Fatalf("export readback: %+v", exportData)
	}
	if len(export.Warnings) == 0 {
		t.Fatal("expected E_LOSSY warnings for pcbin mesh export")
	}
	for _, warning := range export.Warnings {
		if warning.Code != model.ELOSSY {
			t.Fatalf("unexpected warning code %s", warning.Code)
		}
	}
}

func TestPausedCheckpointSurvivesRestart(t *testing.T) {
	svc, root := newService(t)
	start := svc.StartExactJob(service.PreviewRequest{})
	if !start.OK {
		t.Fatalf("start job: %+v", start.Error)
	}
	pause := svc.PauseJob()
	if !pause.OK {
		t.Fatalf("pause: %+v", pause.Error)
	}
	var job service.JobDTO
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		jobs := svc.Dispatch("jobs", json.RawMessage(`{}`))
		if jobs.OK {
			raw, _ := json.Marshal(jobs.Data)
			var parsed struct {
				Jobs []service.JobDTO `json:"jobs"`
			}
			_ = json.Unmarshal(raw, &parsed)
			if len(parsed.Jobs) > 0 && parsed.Jobs[0].Status == "paused" {
				job = parsed.Jobs[0]
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if job.Status != "paused" || job.RunID == "" {
		t.Fatalf("job did not pause cleanly: %+v", job)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	svc2, err := service.New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer svc2.Close()
	resume := svc2.RestartResume(job.RunID)
	if !resume.OK {
		t.Fatalf("restart resume: %+v", resume.Error)
	}
	data := resume.Data.(map[string]any)
	if data["status"] != "complete" {
		t.Fatalf("unexpected resume result: %+v", data)
	}
	sequence, _ := data["sequence"].([]string)
	if len(sequence) != 12 {
		t.Fatalf("resume sequence length=%d", len(sequence))
	}
}

func TestExactLocksSnapshotDuringConcurrentEdit(t *testing.T) {
	svc, _ := newService(t)
	start := svc.StartExactJob(service.PreviewRequest{})
	if !start.OK {
		t.Fatalf("start: %+v", start.Error)
	}
	edit := svc.EditAndIncremental(service.EditRequest{Index: 5, DeltaZ: 0.01, BaseVersion: 1})
	if !edit.OK {
		t.Fatalf("concurrent edit: %+v", edit.Error)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		jobs := svc.Dispatch("jobs", json.RawMessage(`{}`))
		raw, _ := json.Marshal(jobs.Data)
		var parsed struct {
			Jobs []service.JobDTO `json:"jobs"`
		}
		_ = json.Unmarshal(raw, &parsed)
		if len(parsed.Jobs) > 0 && parsed.Jobs[0].Status == "complete" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	after := svc.Bootstrap().Data.(service.WorkspaceDTO)
	if after.Version != 2 {
		t.Fatalf("exact job version leaked into active version: %d", after.Version)
	}
	if after.LastComparison == nil || after.LastComparison.ExactVersion != 1 {
		t.Fatalf("exact comparison did not stay locked to snapshot 1: %+v", after.LastComparison)
	}
}

func TestCameraErrorIsParameterError(t *testing.T) {
	svc, _ := newService(t)
	camera := render.DefaultCamera()
	camera.FOV = 2
	res := svc.Preview(service.PreviewRequest{Camera: &camera})
	if res.OK || res.Error == nil || res.Error.Code != model.EPARA {
		t.Fatalf("expected E_PARA, got %+v", res)
	}
}
