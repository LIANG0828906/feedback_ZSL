package pipeline_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"iconbench/internal/checkpoint"
	"iconbench/internal/imaging"
	"iconbench/internal/model"
	"iconbench/internal/pipeline"
	"iconbench/internal/sample"
	"iconbench/internal/verify"
)

func setupManifest(t *testing.T) (string, *model.Manifest) {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "data")
	m, err := sample.Generate(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	return dataDir, m
}

func newPipeline(t *testing.T, dataDir string, m *model.Manifest, dbName string, workers int) (*pipeline.Pipeline, *checkpoint.Store) {
	t.Helper()
	store, err := checkpoint.Open(filepath.Join(dataDir, dbName))
	if err != nil {
		t.Fatal(err)
	}
	budget := m.Budget
	if workers > 0 {
		budget.WorkerThreads = workers
	}
	return &pipeline.Pipeline{Manifest: m, DataDir: dataDir, Store: store, Pool: pipeline.NewResourcePool(budget)}, store
}

func commitKeys(seq []checkpoint.CommitRecord) []string {
	out := make([]string, 0, len(seq))
	for _, s := range seq {
		out = append(out, checkpoint.Key(s.BlockIndex, s.Layer)+"@"+s.ContentHash)
	}
	return out
}

func TestPreviewSettledRecoveryAndFullAreIdentical(t *testing.T) {
	ctx := context.Background()
	dataDir, m := setupManifest(t)
	rec := m.Images[0]
	params := model.DefaultParams()
	params.ParamVersion = 42

	fullPipe, fullStore := newPipeline(t, dataDir, m, "full.db", 4)
	defer fullStore.Close()
	fullRes, err := fullPipe.RunImage(ctx, rec.ID, params, pipeline.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}

	previewPipe, previewStore := newPipeline(t, dataDir, m, "preview.db", 4)
	defer previewStore.Close()
	img, _, err := sample.LoadImage(dataDir, rec)
	if err != nil {
		t.Fatal(err)
	}
	rgb := imaging.ToRGB(img)
	rough, err := pipeline.RunPreview(rec.ID, rgb, params, m.Budget, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !rough.WithinDelayBudget {
		t.Fatalf("preview elapsed %dms exceeds budget %dms", rough.ElapsedMS, rough.DelayBudgetMS)
	}
	previewFinal, err := previewPipe.RunImage(ctx, rec.ID, params, pipeline.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(previewFinal.Mask, fullRes.Mask) {
		t.Fatal("settled preview path mask differs from full recomputation")
	}
	if !reflect.DeepEqual(previewFinal.Stats, fullRes.Stats) {
		t.Fatal("settled preview path stats differ from full recomputation")
	}

	recoveryPath := filepath.Join(dataDir, "recovery.db")
	recStore1, err := checkpoint.Open(recoveryPath)
	if err != nil {
		t.Fatal(err)
	}
	recPipe1 := &pipeline.Pipeline{Manifest: m, DataDir: dataDir, Store: recStore1, Pool: pipeline.NewResourcePool(m.Budget)}
	if _, err := recPipe1.RunImage(ctx, rec.ID, params, pipeline.RunOptions{MaxCommits: 5}); !errors.Is(err, pipeline.ErrInterrupted) {
		t.Fatalf("expected interruption, got %v", err)
	}
	if err := recStore1.Close(); err != nil {
		t.Fatal(err)
	}
	recStore2, err := checkpoint.Open(recoveryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer recStore2.Close()
	recPipe2 := &pipeline.Pipeline{Manifest: m, DataDir: dataDir, Store: recStore2, Pool: pipeline.NewResourcePool(m.Budget)}
	recoveryRes, err := recPipe2.RunImage(ctx, rec.ID, params, pipeline.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(recoveryRes.Mask, fullRes.Mask) {
		t.Fatal("recovery mask differs from full recomputation")
	}
	if !reflect.DeepEqual(recoveryRes.Stats, fullRes.Stats) {
		t.Fatal("recovery stats differ")
	}
	if !reflect.DeepEqual(commitKeys(recoveryRes.Sequence), commitKeys(fullRes.Sequence)) {
		t.Fatalf("commit sequence mismatch\nrecovery=%v\nfull=%v", commitKeys(recoveryRes.Sequence), commitKeys(fullRes.Sequence))
	}

	workerPipe, workerStore := newPipeline(t, dataDir, m, "one-worker.db", 1)
	defer workerStore.Close()
	workerRes, err := workerPipe.RunImage(ctx, rec.ID, params, pipeline.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if workerRes.MaskHash != fullRes.MaskHash {
		t.Fatal("single-worker result differs from four-worker result")
	}

	report := verify.VerifyImage(ctx, m, recStore2, dataDir, "recovery.db", rec.ID)
	if !report.OK {
		t.Fatalf("independent verifier failed: %+v", report.Violation)
	}

	judgment, jerr := pipeline.JudgePreviewExact(rec.ID, params.ParamVersion, rough.Mask, fullRes.Mask, fullRes.Width, fullRes.Height, m.Tolerances)
	if jerr != nil {
		t.Fatalf("rough preview should stay within declared tolerance: %+v metrics=%+v", jerr, judgment.Metrics)
	}
}

func TestSnapshotTamperTriggersWholeReset(t *testing.T) {
	ctx := context.Background()
	dataDir, m := setupManifest(t)
	rec := m.Images[0]
	params := model.DefaultParams()
	pl, store := newPipeline(t, dataDir, m, "tamper.db", 4)
	defer store.Close()
	first, err := pl.RunImage(ctx, rec.ID, params, pipeline.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.DB().ExecContext(ctx, `UPDATE checkpoints SET content_hash='tampered' WHERE image_id=? AND block_index=0 AND layer=?`, rec.ID, checkpoint.LayerMarkers)
	if err != nil {
		t.Fatal(err)
	}
	second, err := pl.RunImage(ctx, rec.ID, params, pipeline.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !second.SnapshotReset || second.SnapshotViolation == nil || second.SnapshotViolation.Field != "content_hash" {
		t.Fatalf("expected whole snapshot reset, reset=%v violation=%+v", second.SnapshotReset, second.SnapshotViolation)
	}
	if second.MaskHash != first.MaskHash {
		t.Fatal("recomputed mask after reset differs")
	}
	report := verify.VerifyImage(ctx, m, store, dataDir, "tamper.db", rec.ID)
	if !report.OK {
		t.Fatalf("verifier failed after reset: %+v", report.Violation)
	}
}

func TestResourcePoolQueuesAndRejectsOversizedTask(t *testing.T) {
	pool := pipeline.NewResourcePool(model.Budget{WorkerThreads: 2, PixelBufferPerThread: 100})
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		i := i
		go func() {
			_ = pool.Run(context.Background(), model.QueueRecord{BlockIndex: i, Layer: "gradient"}, 100, func() error {
				started <- struct{}{}
				<-release
				return nil
			})
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("workers did not start")
		}
	}

	done := make(chan error, 1)
	go func() {
		done <- pool.Run(context.Background(), model.QueueRecord{BlockIndex: 9, Layer: "gradient"}, 100, func() error { return nil })
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(pool.QueueRecords()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	records := pool.QueueRecords()
	if len(records) == 0 || records[0].Reason != "pixel-buffer-exhausted" {
		t.Fatalf("expected pixel-buffer-exhausted queue record, got %+v", records)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued task did not finish after release")
	}

	err := pool.Run(context.Background(), model.QueueRecord{BlockIndex: 10}, 101, func() error { return nil })
	var me *model.Error
	if !errors.As(err, &me) || me.Code != model.CodeResource {
		t.Fatalf("expected E_RESOURCE for oversized buffer, got %v", err)
	}
}

func TestVersionErrorDivergenceAndTighteningCurve(t *testing.T) {
	params := model.DefaultParams()
	previewParams := params
	previewParams.ParamVersion = 1
	exactParams := params
	exactParams.ParamVersion = 2
	if err := pipeline.CheckVersions(previewParams, exactParams); err == nil {
		t.Fatal("expected E_VER")
	} else {
		var me *model.Error
		if !errors.As(err, &me) || me.Code != model.CodeVer || me.VersionA == "" || me.VersionB == "" {
			t.Fatalf("bad version error: %+v", err)
		}
	}

	mask := []byte{0, 1, 0, 0, 1, 0}
	exact := append([]byte(nil), mask...)
	j, err := pipeline.JudgePreviewExact("x", 1, mask, exact, 3, 2, model.DefaultTolerances())
	if err != nil || !j.OK {
		t.Fatalf("identical masks should pass: %v %+v", err, j)
	}

	bad := append([]byte(nil), exact...)
	for i := 0; i < 4; i++ {
		bad[i] = 1
	}
	tol := model.DefaultTolerances()
	j2, derr := pipeline.JudgePreviewExact("x", 1, bad, exact, 3, 2, tol)
	if derr == nil || derr.Code != model.CodeDiverge || derr.Metrics == nil {
		t.Fatalf("expected E_DIVERGE with metrics, got %v judgment=%+v", derr, j2)
	}
	if derr.DiffX < 0 || derr.DiffY < 0 {
		t.Fatal("expected first difference coordinate")
	}

	sequence := []model.Tolerances{
		{PreviewRegionCount: 5, PreviewCentroidPx: 100, PreviewAreaRel: 10},
		{PreviewRegionCount: 2, PreviewCentroidPx: 40, PreviewAreaRel: 2},
		{PreviewRegionCount: 0, PreviewCentroidPx: 5, PreviewAreaRel: 0.2},
		{PreviewRegionCount: 0, PreviewCentroidPx: 0.5, PreviewAreaRel: 0.02},
	}
	curve := pipeline.EvaluateToleranceCurve("x", 1, bad, exact, 3, 2, sequence)
	if len(curve) != len(sequence) {
		t.Fatalf("expected %d curve points", len(sequence))
	}
	for i := 1; i < len(curve); i++ {
		if curve[i].ReportedStat > curve[i-1].ReportedStat {
			t.Fatalf("reported statistic increased under tightening: %f -> %f", curve[i-1].ReportedStat, curve[i].ReportedStat)
		}
		if curve[i].Judgment.Tolerances.PreviewCentroidPx != sequence[i].PreviewCentroidPx {
			t.Fatal("judgment did not retain tolerance")
		}
	}
}
