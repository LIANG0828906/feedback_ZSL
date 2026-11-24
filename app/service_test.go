package app_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"virtualstage/app"
	"virtualstage/pipeline"
	"virtualstage/sample"

	_ "modernc.org/sqlite"
)

func TestEndToEndPipelineAndRecovery(t *testing.T) {
	ctx := context.Background()
	work := t.TempDir()
	if _, err := sample.Generate(sample.Options{OutDir: work, Seed: 123}); err != nil {
		t.Fatal(err)
	}
	service, err := app.OpenService(app.Config{WorkDir: work, Workers: 3, MaxFrameBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	validation, err := service.ValidateTruth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !validation.OK || validation.MaxAbsError != 0 {
		t.Fatalf("truth validation = %+v", validation)
	}
	snapshot, err := service.RunFull(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Committed != 4 || snapshot.Total != 4 {
		t.Fatalf("full progress = %d/%d", snapshot.Committed, snapshot.Total)
	}
	if service.Engine().Recovery().Purged {
		t.Fatal("unexpected purge on first run")
	}

	opacity := 0.62
	invalidation, err := service.ApplyIncremental(ctx, pipeline.EditRequest{
		Kind: pipeline.EditLayerParameter, LayerID: "prop", Opacity: &opacity,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(invalidation.Affected) != 3 {
		t.Fatalf("affected windows = %+v", invalidation.Affected)
	}
	if len(invalidation.Reused) != 1 || invalidation.Reused[0] != 3 {
		t.Fatalf("reused windows = %v", invalidation.Reused)
	}
	heatmap, err := service.Heatmap(0, 0, 8, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !heatmap.Available || heatmap.MaxDelta <= 0 {
		t.Fatalf("heatmap = %+v", heatmap)
	}

	verification, err := service.VerifyIncremental(ctx, pipeline.EditRequest{
		Kind: pipeline.EditLayerParameter, LayerID: "prop", Opacity: floatPtr(0.57),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !verification.Match || verification.Comparison.First.Found {
		t.Fatalf("incremental comparison = %+v", verification.Comparison)
	}

	exported, err := service.ExportAndVerify(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !exported.RoundTripOK || !exported.Deterministic || !exported.StackEqual || !exported.VisibilityEqual {
		t.Fatalf("export verification = %+v", exported)
	}
	if len(exported.Warnings) == 0 {
		t.Fatal("expected E_LOSSY warning for v1 alpha normalization")
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", filepath.Join(work, "checkpoints", "pipeline.db"))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT window_index, committed_sequence FROM window_snapshots ORDER BY window_index`)
	if err != nil {
		t.Fatal(err)
	}
	var indexes, sequences []int
	for rows.Next() {
		var index, sequence int
		if err := rows.Scan(&index, &sequence); err != nil {
			t.Fatal(err)
		}
		indexes = append(indexes, index)
		sequences = append(sequences, sequence)
	}
	rows.Close()
	db.Close()
	if len(indexes) != 4 || indexes[0] != 0 || indexes[3] != 3 {
		t.Fatalf("window order = %v", indexes)
	}
	for i := 1; i < len(sequences); i++ {
		if sequences[i] <= sequences[i-1] {
			t.Fatalf("commit sequence is not stable: %v", sequences)
		}
	}

	reopened, err := app.OpenService(app.Config{WorkDir: work, Workers: 2, MaxFrameBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	recovery := reopened.Snapshot().Recovery
	if recovery.Purged || len(recovery.Restored) != 4 || reopened.Snapshot().Committed != 4 {
		t.Fatalf("recovery = %+v", recovery)
	}
}

func TestCorruptCachePurgesAllWindows(t *testing.T) {
	ctx := context.Background()
	work := t.TempDir()
	if _, err := sample.Generate(sample.Options{OutDir: work, Seed: 8}); err != nil {
		t.Fatal(err)
	}
	service, err := app.OpenService(app.Config{WorkDir: work, Workers: 2, MaxFrameBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.RunFull(ctx); err != nil {
		t.Fatal(err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(work, "cache", "frames", "window-00002.vspw")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write([]byte("corrupt"))
	file.Close()

	reopened, err := app.OpenService(app.Config{WorkDir: work, Workers: 2, MaxFrameBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	snapshot := reopened.Snapshot()
	if !snapshot.Recovery.Purged || len(snapshot.Recovery.Restored) != 0 || snapshot.Committed != 0 {
		t.Fatalf("corruption did not purge all windows: %+v", snapshot.Recovery)
	}
}

func TestAlgorithmVersionMismatchPurgesAll(t *testing.T) {
	ctx := context.Background()
	work := t.TempDir()
	if _, err := sample.Generate(sample.Options{OutDir: work, Seed: 5}); err != nil {
		t.Fatal(err)
	}
	service, err := app.OpenService(app.Config{WorkDir: work, Workers: 2, MaxFrameBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.RunFull(ctx); err != nil {
		t.Fatal(err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(work, "checkpoints", "pipeline.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE window_snapshots SET algorithm_version='old-algorithm' WHERE window_index=1`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	reopened, err := app.OpenService(app.Config{WorkDir: work, Workers: 2, MaxFrameBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if !reopened.Snapshot().Recovery.Purged || reopened.Snapshot().Committed != 0 {
		t.Fatalf("algorithm mismatch did not purge: %+v", reopened.Snapshot().Recovery)
	}
}

func TestSourceHashMismatchPurgesAll(t *testing.T) {
	ctx := context.Background()
	work := t.TempDir()
	if _, err := sample.Generate(sample.Options{OutDir: work, Seed: 6}); err != nil {
		t.Fatal(err)
	}
	service, err := app.OpenService(app.Config{WorkDir: work, Workers: 2, MaxFrameBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.RunFull(ctx); err != nil {
		t.Fatal(err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(work, "checkpoints", "pipeline.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE window_snapshots SET source_hash='old-source-version' WHERE window_index=2`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	reopened, err := app.OpenService(app.Config{WorkDir: work, Workers: 2, MaxFrameBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if !reopened.Snapshot().Recovery.Purged || reopened.Snapshot().Committed != 0 {
		t.Fatalf("source mismatch did not purge: %+v", reopened.Snapshot().Recovery)
	}
}

func TestTimeBaseEditInvalidatesEveryWindow(t *testing.T) {
	ctx := context.Background()
	work := t.TempDir()
	if _, err := sample.Generate(sample.Options{OutDir: work, Seed: 17}); err != nil {
		t.Fatal(err)
	}
	service, err := app.OpenService(app.Config{WorkDir: work, Workers: 2, MaxFrameBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if _, err := service.RunFull(ctx); err != nil {
		t.Fatal(err)
	}
	oldSourceHash := service.Engine().SourceHash()
	list, err := service.ApplyIncremental(ctx, pipeline.EditRequest{
		Kind: pipeline.EditTimeBase,
		TimeBase: &pipeline.TimeBase{
			Unit: "second", FramePeriod: pipeline.NewRational(1, 30),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Affected) != 4 || len(list.Reused) != 0 {
		t.Fatalf("time-base invalidation = %+v", list)
	}
	if service.Engine().SourceHash() == oldSourceHash {
		t.Fatal("time-base edit did not change source fingerprint")
	}
	if snapshot := service.Snapshot(); snapshot.Committed != 4 {
		t.Fatalf("committed = %d", snapshot.Committed)
	}
	frame, err := service.Engine().Frame(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Timestamp != pipeline.NewRational(1, 30) {
		t.Fatalf("timestamp = %v", frame.Timestamp)
	}
}

func floatPtr(value float64) *float64 {
	return &value
}
