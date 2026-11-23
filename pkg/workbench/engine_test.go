package workbench

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStateMachineRejectsIllegalTransition(t *testing.T) {
	scene, err := GenerateScene(DefaultGenerateConfig())
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(scene, DefaultProcessConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if err := engine.Step(); err != nil {
		t.Fatal(err)
	}
	before := engine.Status().Segments[0].State
	err = engine.TransitionSegment(0, StateProcessing)
	if err == nil {
		t.Fatal("expected E_STATE")
	}
	we := AsWorkbenchError(err)
	if we.Code != CodeState {
		t.Fatalf("got %s, want %s", we.Code, CodeState)
	}
	if after := engine.Status().Segments[0].State; after != before {
		t.Fatalf("state changed after illegal transition: %s -> %s", before, after)
	}
}

func TestCommittedSegmentFreezesParams(t *testing.T) {
	scene, err := GenerateScene(DefaultGenerateConfig())
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultProcessConfig()
	cfg.MaxWorkers = 1
	engine, err := NewEngine(scene, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if err := engine.Step(); err != nil {
		t.Fatal(err)
	}
	status := engine.Status()
	frozenHash := status.Segments[0].ContentHash
	frozenVersion := status.Segments[0].ParamVersion
	updated := cfg
	updated.WeightBias = 0.55
	updated.DetailStrength = 0.8
	updated.GamutIntent = IntentCompress
	newVersion, err := engine.UpdateParams(updated)
	if err != nil {
		t.Fatal(err)
	}
	if newVersion == frozenVersion {
		t.Fatal("parameter version did not advance")
	}
	status = engine.Status()
	if status.Segments[0].ContentHash != frozenHash {
		t.Fatal("committed content hash changed after parameter update")
	}
	if status.Segments[0].ParamVersion != frozenVersion {
		t.Fatal("committed parameter version changed after update")
	}
}

func TestCheckpointResumeAndThreePathConsistency(t *testing.T) {
	scene, err := GenerateScene(DefaultGenerateConfig())
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewCheckpointStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultProcessConfig()
	cfg.MaxWorkers = 2
	engine, err := NewEngine(scene, cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := engine.Step(); err != nil {
			t.Fatal(err)
		}
	}
	committedBefore := engine.Progress().Committed
	engine.Close()
	if committedBefore == 0 {
		t.Fatal("expected committed checkpoints before restart")
	}

	resumed, err := NewEngine(scene, cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	report := resumed.CheckpointReport()
	if report.Invalidated {
		t.Fatalf("checkpoint unexpectedly invalidated: %s", report.Reason)
	}
	if report.Loaded < committedBefore {
		t.Fatalf("loaded %d segments, want at least %d", report.Loaded, committedBefore)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := resumed.Run(ctx); err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	resumedResult, err := resumed.Result()
	if err != nil {
		t.Fatal(err)
	}
	full, err := RenderScene(scene, cfg, PathFull)
	if err != nil {
		t.Fatal(err)
	}
	tiled, err := RenderScene(scene, cfg, PathTiled)
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]*RenderResult{{resumedResult, full}, {resumedResult, tiled}, {full, tiled}} {
		rep, err := CompareRenderResults(pair[0], pair[1], tiled.Scale.TileWidth, tiled.Scale.TileHeight, scene.Truth.Tolerance)
		if err != nil {
			t.Fatalf("three-path comparison failed: %v (%+v)", err, rep)
		}
		if !rep.Consistent {
			t.Fatalf("three-path comparison inconsistent: %+v", rep)
		}
	}
}

func TestCheckpointCorruptionInvalidatesWholeSnapshot(t *testing.T) {
	scene, err := GenerateScene(DefaultGenerateConfig())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	store, err := NewCheckpointStore(root)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(scene, DefaultProcessConfig(), store)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Step(); err != nil {
		t.Fatal(err)
	}
	engine.Close()
	files, err := filepath.Glob(filepath.Join(root, "*", "segment-*.gob"))
	if err != nil || len(files) == 0 {
		t.Fatalf("expected checkpoint files, got %v err=%v", files, err)
	}
	if err := os.Truncate(files[0], 12); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewEngine(scene, DefaultProcessConfig(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	report := restarted.CheckpointReport()
	if !report.Invalidated {
		t.Fatalf("expected invalidated snapshot, got %+v", report)
	}
	if restarted.Progress().Committed != 0 {
		t.Fatalf("expected full recompute, got %d committed segments", restarted.Progress().Committed)
	}
}

func TestCheckpointVersionMismatchInvalidatesWholeSnapshot(t *testing.T) {
	scene, err := GenerateScene(DefaultGenerateConfig())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	store, err := NewCheckpointStore(root)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(scene, DefaultProcessConfig(), store)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Step(); err != nil {
		t.Fatal(err)
	}
	engine.Close()
	manifests, err := filepath.Glob(filepath.Join(root, "*", "manifest.json"))
	if err != nil || len(manifests) != 1 {
		t.Fatalf("manifest not found: %v err=%v", manifests, err)
	}
	data, err := os.ReadFile(manifests[0])
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte(AlgorithmVersion), []byte("different-algorithm"), 1)
	if err := os.WriteFile(manifests[0], data, 0o644); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewEngine(scene, DefaultProcessConfig(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if report := restarted.CheckpointReport(); !report.Invalidated {
		t.Fatalf("expected version mismatch invalidation, got %+v", report)
	}
	if restarted.Progress().Committed != 0 {
		t.Fatal("snapshot was not fully invalidated")
	}
}

func TestSegmentFailureIsolationAndRetry(t *testing.T) {
	scene, err := GenerateScene(DefaultGenerateConfig())
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(scene, DefaultProcessConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if err := engine.InjectFault(1, "simulated worker panic"); err != nil {
		t.Fatal(err)
	}
	if err := engine.Step(); err != nil {
		t.Fatal(err)
	}
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := engine.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	status := engine.Status()
	if status.Segments[1].State != StateFailed {
		t.Fatalf("segment 1 state=%s, want failed", status.Segments[1].State)
	}
	for i, seg := range status.Segments {
		if i != 1 && seg.State != StateCommitted {
			t.Fatalf("segment %d state=%s, want committed", i, seg.State)
		}
	}
	engine.ClearFault(1)
	if err := engine.RetrySegment(1); err != nil {
		t.Fatal(err)
	}
	if err := engine.Resume(); err != nil {
		t.Fatal(err)
	}
	if err := engine.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if !engine.Status().ResultReady {
		t.Fatal("expected complete result after retry")
	}
}

func TestWeightBiasHasMeasurableDirection(t *testing.T) {
	scene, err := GenerateScene(DefaultGenerateConfig())
	if err != nil {
		t.Fatal(err)
	}
	means := make([]float64, 0, 5)
	for _, bias := range []float64{-0.8, -0.4, 0, 0.4, 0.8} {
		cfg := DefaultProcessConfig()
		cfg.WeightBias = bias
		result, err := RenderScene(scene, cfg, PathFull)
		if err != nil {
			t.Fatal(err)
		}
		means = append(means, result.MeanRadiance)
	}
	if math.Abs(means[0]-means[len(means)-1]) < 1e-7 {
		t.Fatalf("weight bias produced no measurable change: %v", means)
	}
	for i := 1; i < len(means); i++ {
		if means[i] < means[i-1] {
			t.Fatalf("expected monotonic response to weight bias, got %v", means)
		}
	}
}
