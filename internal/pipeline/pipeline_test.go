package pipeline_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"maskpipe/internal/model"
	"maskpipe/internal/pipeline"
	"maskpipe/internal/sample"
	"maskpipe/internal/store"
	"maskpipe/internal/verify"

	_ "modernc.org/sqlite"
)

func loadSceneForTest(t *testing.T, id string) *sample.LoadedScene {
	t.Helper()
	root := filepath.Join(t.TempDir(), "samples")
	if _, err := sample.Generate(root, 424242); err != nil {
		t.Fatal(err)
	}
	loaded, err := sample.LoadScene(root, id)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func TestParallelAndSerialMasksArePixelIdentical(t *testing.T) {
	loaded := loadSceneForTest(t, "blocks")
	ctx := context.Background()

	loaded.Scene.Declared.Budget.WorkerCount = 1
	serialService, err := pipeline.NewService(loaded, nil, nil, pipeline.NewControl())
	if err != nil {
		t.Fatal(err)
	}
	serial, err := serialService.Run(ctx, pipeline.ModeFull, nil)
	if err != nil {
		t.Fatal(err)
	}
	if serial.Quality == nil || !serial.Quality.OK {
		t.Fatalf("serial quality failed: %+v", serial.FirstError)
	}

	loaded.Scene.Declared.Budget.WorkerCount = 4
	loaded.Scene.Declared.Budget.PendingTaskQueueBudget = 2
	parallelService, err := pipeline.NewService(loaded, nil, nil, pipeline.NewControl())
	if err != nil {
		t.Fatal(err)
	}
	parallel, err := parallelService.Run(ctx, pipeline.ModeFull, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(serial.Mask) != string(parallel.Mask) {
		for i := range serial.Mask {
			if serial.Mask[i] != parallel.Mask[i] {
				x := i % loaded.Scene.Width
				y := i / loaded.Scene.Width
				t.Fatalf("mask first differs at (%d,%d)", x, y)
			}
		}
	}
	if serial.MaskHash != parallel.MaskHash {
		t.Fatalf("hash mismatch %s vs %s", serial.MaskHash, parallel.MaskHash)
	}
	if parallel.Metrics.MaxActiveWorkersObserved < 2 {
		t.Fatalf("expected real parallel workers, peak=%d", parallel.Metrics.MaxActiveWorkersObserved)
	}
}

func TestStateMachineIllegalTransitionKeepsState(t *testing.T) {
	chunks := pipeline.PlanChunks(192, 128, model.Tile{Width: 64, Height: 64})
	machine := pipeline.NewStateMachine(chunks)
	key := model.ChunkKey{ChunkCoord: model.ChunkCoord{X: 0, Y: 0}, Stage: model.StageGrow}
	err := machine.Transition(key, model.StatusCommitted, "pv-x", nil)
	pe, ok := model.AsError(err)
	if !ok || pe.Code != model.CodeState {
		t.Fatalf("expected E_STATE, got %v", err)
	}
	if got := machine.Get(key).Status; got != model.StatusPending {
		t.Fatalf("illegal transition changed state to %s", got)
	}
	if err := machine.Transition(key, model.StatusProcessing, "pv-x", nil); err != nil {
		t.Fatal(err)
	}
	if err := machine.Transition(key, model.StatusCommitted, "pv-x", nil); err != nil {
		t.Fatal(err)
	}
	if err := machine.Transition(key, model.StatusProcessing, "pv-x", nil); err == nil {
		t.Fatal("committed stage must be frozen")
	}
}

func TestBadSeedFailsOnlyOwningChunk(t *testing.T) {
	loaded := loadSceneForTest(t, "blocks")
	params := loaded.Scene.Parameters
	params.Seeds = append(params.Seeds, model.IntPoint{X: 2, Y: 2})
	svc, err := pipeline.NewService(loaded, nil, &params, pipeline.NewControl())
	if err != nil {
		t.Fatal(err)
	}
	result, runErr := svc.Run(context.Background(), pipeline.ModeFull, nil)
	pe, ok := model.AsError(runErr)
	if !ok || pe.Code != model.CodeSeed {
		t.Fatalf("expected E_SEED, got %v", runErr)
	}
	if result == nil {
		t.Fatal("expected partial result")
	}
	failed := svc.Machine.Get(model.ChunkKey{ChunkCoord: model.ChunkCoord{X: 0, Y: 0}, Stage: model.StageGrow})
	if failed.Status != model.StatusFailed || failed.LastErrorCode != model.CodeSeed {
		t.Fatalf("bad seed chunk state=%+v", failed)
	}
	other := svc.Machine.Get(model.ChunkKey{ChunkCoord: model.ChunkCoord{X: 1, Y: 0}, Stage: model.StageStats})
	if other.Status != model.StatusCommitted {
		t.Fatalf("other chunk should remain committed, got %s", other.Status)
	}
	committed, total := svc.Machine.Progress(len(svc.Chunks))
	if total != 18 || committed != 15 {
		t.Fatalf("expected 15/18 unaffected commits, got %d/%d", committed, total)
	}
}

func TestBudgetExhaustionRecordsBlockingQueue(t *testing.T) {
	loaded := loadSceneForTest(t, "blocks")
	loaded.Scene.Tile = model.Tile{Width: 16, Height: 16}
	svc, err := pipeline.NewService(loaded, nil, nil, pipeline.NewControl())
	if err != nil {
		t.Fatal(err)
	}
	svc.Budget.WorkerCount = 1
	svc.Budget.PendingTaskQueueBudget = 1
	result, err := svc.Run(context.Background(), pipeline.ModeFull, nil)
	if pe, ok := model.AsError(err); ok && pe.Code != model.CodeRegion && pe.Code != model.CodeQual {
		t.Fatal(err)
	}
	if len(result.Metrics.QueueEvents) == 0 {
		t.Fatal("expected queue_budget_exhausted events")
	}
	if result.Metrics.QueueEvents[0].Reason != "queue_budget_exhausted" {
		t.Fatalf("unexpected reason %q", result.Metrics.QueueEvents[0].Reason)
	}
}

func TestThreeWayResumeTamperAndIndependentVerify(t *testing.T) {
	loaded := loadSceneForTest(t, "blocks")
	dir := filepath.Join(t.TempDir(), "threeway")
	ctx := context.Background()
	threeWay, err := pipeline.RunThreeWay(ctx, loaded, loaded.Scene.Parameters, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !threeWay.Report.OK {
		t.Fatalf("three-way mismatch: %+v", threeWay.Report.Comparisons)
	}
	verification, err := verify.Run(ctx, threeWay.DBPaths["full"], "", &loaded.Scene)
	if err != nil {
		t.Fatal(err)
	}
	if !verification.OK || verification.FirstViolation != nil {
		t.Fatalf("independent verification failed: %+v", verification.FirstViolation)
	}

	db, err := sql.Open("sqlite", threeWay.DBPaths["resume"])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE stages SET content_hash='tampered' WHERE canonical_order=0`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	tamperedVerify, err := verify.Run(ctx, threeWay.DBPaths["resume"], "", &loaded.Scene)
	if err != nil {
		t.Fatal(err)
	}
	if tamperedVerify.OK || tamperedVerify.FirstViolation == nil ||
		tamperedVerify.FirstViolation.Field != "content_hash" {
		t.Fatalf("expected content_hash violation, got %+v", tamperedVerify.FirstViolation)
	}

	st, err := store.Open(threeWay.DBPaths["resume"])
	if err != nil {
		t.Fatal(err)
	}
	resumeService, err := pipeline.NewService(loaded, st, nil, pipeline.NewControl())
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := resumeService.Run(ctx, pipeline.ModeResume, nil)
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	if recovered.SnapshotResetReason == "" {
		t.Fatal("expected tampered snapshot to be discarded")
	}
	if recovered.MaskHash != threeWay.Results["full"].MaskHash {
		t.Fatalf("recovered hash %s != full %s", recovered.MaskHash, threeWay.Results["full"].MaskHash)
	}
}

func TestPreviewDivergenceReport(t *testing.T) {
	full := []byte{0, 0, 0, 0, 1, 1, 0, 0, 1, 1, 1, 0, 0, 1, 1, 0}
	preview := append([]byte(nil), full...)
	preview[4] = 0
	fullRegions := []model.Region{{ID: 1, Area: 6, Centroid: model.Point{X: 2.5, Y: 1.5}}}
	previewRegions := []model.Region{{ID: 1, Area: 5, Centroid: model.Point{X: 9, Y: 9}}}
	report := pipeline.ComparePreview(full, preview, 4, 4, fullRegions, previewRegions,
		model.DivergenceTolerance{CentroidPixels: 0.5, AreaRelative: 0.01, PixelMismatch: 0})
	if report.OK || report.Error == nil {
		t.Fatal("expected E_DIVERGE report")
	}
	pe, ok := model.AsError(report.Error)
	if !ok || pe.Code != model.CodeDiverge {
		t.Fatalf("wrong error %v", report.Error)
	}
	if report.FirstMismatchPixel == nil || report.MaxDifferencePixel == nil {
		t.Fatal("expected mismatch coordinates")
	}
}

func TestParameterEditsKeepCommittedStageFrozen(t *testing.T) {
	loaded := loadSceneForTest(t, "blocks")
	svc, err := pipeline.NewService(loaded, nil, nil, pipeline.NewControl())
	if err != nil {
		t.Fatal(err)
	}
	svc.ChunkFilter = map[int]bool{0: true}
	if _, err := svc.Run(context.Background(), pipeline.ModeFull, nil); err != nil {
		if pe, ok := model.AsError(err); !ok || (pe.Code != model.CodeRegion && pe.Code != model.CodeQual) {
			t.Fatal(err)
		}
	}
	committedKey := model.ChunkKey{ChunkCoord: model.ChunkCoord{X: 0, Y: 0}, Stage: model.StageGrow}
	oldVersion := svc.Machine.Get(committedKey).ParamVersion
	if oldVersion == "" {
		t.Fatal("committed stage should record parameter version")
	}

	params := svc.CurrentParams()
	params.ColorTolerance = 55
	params.ParamVersion = "pv-edited-draft"
	if err := svc.UpdateParameters(params); err != nil {
		t.Fatal(err)
	}
	if got := svc.Machine.Get(committedKey).ParamVersion; got != oldVersion {
		t.Fatalf("committed version changed from %s to %s", oldVersion, got)
	}

	pendingKey := model.ChunkKey{ChunkCoord: model.ChunkCoord{X: 1, Y: 0}, Stage: model.StageGrow}
	newVersion := svc.CurrentParams().ParamVersion
	if err := svc.Machine.Transition(pendingKey, model.StatusProcessing, newVersion, nil); err != nil {
		t.Fatal(err)
	}
	if got := svc.Machine.Get(pendingKey).ParamVersion; got != "pv-edited-draft" {
		t.Fatalf("pending stage should use edited version, got %s", got)
	}
}
