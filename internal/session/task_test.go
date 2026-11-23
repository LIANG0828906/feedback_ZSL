package session

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stitchworkbench/internal/errs"
	"stitchworkbench/internal/model"
	"stitchworkbench/internal/samplegen"
	"stitchworkbench/internal/stitch"
)

func TestPreviewExactRestartAndVersionBoundary(t *testing.T) {
	data := t.TempDir()
	work := t.TempDir()
	if err := samplegen.Generate(samplegen.Config{OutDir: data, Seed: 42, Sets: 1}); err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(data, work)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := svc.Open("harbor-06")
	if err != nil {
		t.Fatal(err)
	}
	preview, err := sess.Preview(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("preview version=%d pairs=%d canvas=%+v ms=%d", preview.ParamVersion, len(preview.Pairs), preview.Canvas, preview.DurationMs)
	task, err := sess.StartExact(preview.ParamVersion, false)
	if err != nil {
		t.Fatal(err)
	}
	view := waitTask(t, sess, task.ID)
	if view.Status != "completed" {
		t.Fatalf("task failed: status=%s phase=%s err=%#v", view.Status, view.Phase, view.Error)
	}
	t.Logf("exact canvas=%+v pairs=%d divergence=%+v seamP95=%.2f duration=%d", view.Result.Canvas, len(view.Result.Pairs), view.Result.Divergence, view.Result.SeamGradientP95, view.Result.DurationMs)
	if view.Result.Canvas != sess.Set.Manifest.Canvas {
		t.Fatal("canvas mismatch")
	}
	for _, p := range view.Result.Pairs {
		if p.PairIndex > 0 && p.Decision {
			if _, err := sess.CommitPair(view.Result.ParamVersion, p.PairIndex); err != nil {
				t.Fatal(err)
			}
		}
	}
	pano1, ok := sess.Artifact(view.Result.TaskID + "-panorama.png")
	if !ok {
		t.Fatal("first panorama artifact missing")
	}
	hash1 := sha256.Sum256(pano1)

	svc2, err := NewService(data, work)
	if err != nil {
		t.Fatal(err)
	}
	sess2, err := svc2.Open("harbor-06")
	if err != nil {
		t.Fatal(err)
	}
	if sess2.CommitCount() < 1 {
		t.Fatalf("restart committed=%d warning=%s", sess2.CommitCount(), sess2.Warning())
	}
	task2, err := sess2.StartExact(sess2.Latest().Version, true)
	if err != nil {
		t.Fatal(err)
	}
	view2 := waitTask(t, sess2, task2.ID)
	if view2.Status != "completed" {
		t.Fatalf("resume task failed: %+v", view2)
	}
	pano2, ok := sess2.Artifact(view2.Result.TaskID + "-panorama.png")
	if !ok {
		t.Fatal("resumed panorama artifact missing")
	}
	hash2 := sha256.Sum256(pano2)
	if hash1 != hash2 {
		t.Fatalf("fresh and resumed panorama differ: %x != %x", hash1, hash2)
	}
	t.Logf("fresh/resumed panorama hash=%x", hash1[:12])

	oldVersion := sess2.Latest().Version
	next, err := sess2.UpdateParams(map[string]json.RawMessage{
		"feature_budget": json.RawMessage("9000"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if next.Version != oldVersion+1 {
		t.Fatalf("version not incremented: %d -> %d", oldVersion, next.Version)
	}
	_, staleErr := sess2.Preview(oldVersion)
	if staleErr == nil || errs.As(staleErr).Code != errs.EVersion {
		t.Fatalf("expected E_VER, got %v", staleErr)
	}
	if sess2.CommitCount() < 1 {
		t.Fatal("committed pairs were not frozen after parameter update")
	}
}

func TestDivergenceAndCheckpointInvalidation(t *testing.T) {
	params := model.DefaultParams()
	pc := &previewCache{
		Result: &model.PreviewResult{ContentHash: "hash", ParamVersion: 4},
		Pairs: map[int]model.PairStats{
			1: {PairIndex: 1, PairID: "base#a.png", Decision: false, OverlapCoverage: 0.5},
		},
	}
	exact := &stitch.Registration{
		ContentHash: "hash", Version: 4,
		Pairs: []model.PairStats{
			{PairIndex: 0, PairID: "base", Decision: true},
			{PairIndex: 1, PairID: "base#a.png", Decision: true, OverlapCoverage: 0.9},
		},
	}
	_, err := comparePreview(pc, exact, params, nil, time.Now())
	if e := errs.As(err); e.Code != errs.EDiverge || e.Actual == nil {
		t.Fatalf("expected E_DIVERGE with metrics, got %v", err)
	}

	data := t.TempDir()
	work := t.TempDir()
	if err := samplegen.Generate(samplegen.Config{OutDir: data, Seed: 42, Sets: 1}); err != nil {
		t.Fatal(err)
	}
	svc, _ := NewService(data, work)
	sess, err := svc.Open("harbor-06")
	if err != nil {
		t.Fatal(err)
	}
	preview, err := sess.Preview(0)
	if err != nil {
		t.Fatal(err)
	}
	task, _ := sess.StartExact(preview.ParamVersion, false)
	view := waitTask(t, sess, task.ID)
	if view.Status != "completed" {
		t.Fatalf("task failed: %+v", view)
	}
	p := view.Result.Pairs[1]
	if _, err := sess.CommitPair(view.Result.ParamVersion, p.PairIndex); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(work, "harbor-06", "checkpoints", "checkpoint.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snap model.CheckpointSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		t.Fatal(err)
	}
	snap.Records[0].ParamVersion = 999
	b, _ = json.Marshal(snap)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	svc2, _ := NewService(data, work)
	sess2, err := svc2.Open("harbor-06")
	if err != nil {
		t.Fatal(err)
	}
	if sess2.CommitCount() != 0 || sess2.Warning() == "" {
		t.Fatalf("invalid checkpoint was partially reused: commits=%d warning=%q", sess2.CommitCount(), sess2.Warning())
	}
}

func waitTask(t *testing.T, sess *SetSession, id string) TaskView {
	t.Helper()
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		v, ok := sess.Task(id)
		if !ok {
			t.Fatalf("task %s vanished", id)
		}
		if v.Status != "running" {
			return v
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("task timeout")
	return TaskView{}
}
