package engine_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"maskpreview/internal/core"
	"maskpreview/internal/engine"
	"maskpreview/internal/sample"
	"maskpreview/internal/segment"
)

func TestThreePathsArePixelIdentical(t *testing.T) {
	spec, loaded, snapshot := makeSample(t)
	base := t.TempDir()
	fullStore, err := engine.OpenStore(filepath.Join(base, "full.db"))
	if err != nil {
		t.Fatalf("open full store: %v", err)
	}
	defer fullStore.Close()
	incStore, err := engine.OpenStore(filepath.Join(base, "incremental.db"))
	if err != nil {
		t.Fatalf("open incremental store: %v", err)
	}
	defer incStore.Close()

	cfg := engine.DefaultRunConfig()
	cfg.BlockSize = 16
	cfg.Budget = engine.Budget{MaxParallel: 3, MaxPerBlockBytes: 4096, GlobalBytes: 1600}

	full, err := engine.Execute(context.Background(), loaded.Image, snapshot, spec.TruthFingerprint, engine.ModeFull, fullStore, cfg)
	if err != nil {
		t.Fatalf("full run: %v", err)
	}
	if full.Progress.Committed != full.Progress.Total {
		t.Fatalf("full progress = %+v", full.Progress)
	}
	for _, blockState := range full.BlockStates {
		for _, stage := range engine.StageOrder {
			if blockState.States[stage] != engine.StateCommitted {
				t.Fatalf("block %d stage %s state=%s, want committed", blockState.BlockID, stage, blockState.States[stage])
			}
		}
		if blockState.FrozenVersion != snapshot.Version {
			t.Fatalf("block %d frozen version=%s, want %s", blockState.BlockID, blockState.FrozenVersion, snapshot.Version)
		}
	}

	session := engine.NewSession(spec.PreviewTolerance)
	preview, err := session.Preview(loaded.Image, snapshot, spec.PreviewDelayMS)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if !preview.WithinBudget {
		t.Fatalf("preview exceeded budget: %.3fms > %dms", preview.ElapsedMS, spec.PreviewDelayMS)
	}
	finalized, err := session.Finalize(context.Background(), loaded.Image, snapshot, spec.TruthFingerprint, incStore, cfg)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if finalized.Comparison == nil || !finalized.Comparison.OK {
		t.Fatalf("preview comparison failed: %+v", finalized.Comparison)
	}

	resumed, err := engine.Execute(context.Background(), loaded.Image, snapshot, spec.TruthFingerprint, engine.ModeResume, incStore, cfg)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !resumed.CheckpointReused {
		t.Fatalf("checkpoint was not reused: %s", resumed.InvalidatedReason)
	}

	if full.MaskHash != finalized.Run.MaskHash || full.MaskHash != resumed.MaskHash {
		t.Fatalf("mask hash differs: full=%s incremental=%s resume=%s", full.MaskHash, finalized.Run.MaskHash, resumed.MaskHash)
	}
	if full.RegionCount != finalized.Run.RegionCount || full.RegionCount != resumed.RegionCount {
		t.Fatalf("region counts differ: full=%d incremental=%d resume=%d", full.RegionCount, finalized.Run.RegionCount, resumed.RegionCount)
	}
	for i := range full.Regions {
		if full.Regions[i].Area != finalized.Run.Regions[i].Area || full.Regions[i].Area != resumed.Regions[i].Area ||
			full.Regions[i].Centroid != finalized.Run.Regions[i].Centroid || full.Regions[i].Centroid != resumed.Regions[i].Centroid {
			t.Fatalf("region geometry differs at index %d", i)
		}
	}
	if !engine.SequenceEqual(full.Records, finalized.Run.Records) || !engine.SequenceEqual(full.Records, resumed.Records) {
		t.Fatal("stage commit sequence differs between paths")
	}
	if full.Scheduler.MaxActiveObserved > cfg.Budget.MaxParallel {
		t.Fatalf("max active blocks %d exceeds limit %d", full.Scheduler.MaxActiveObserved, cfg.Budget.MaxParallel)
	}

	report, err := segment.CompareToTruth(full.Regions, spec.Regions, spec.PreviewTolerance)
	if err != nil {
		t.Fatalf("compare truth: %v", err)
	}
	if !report.WithinTolerance {
		t.Fatalf("truth deviation exceeded: %+v", report)
	}
}

func TestMixedSnapshotsReturnEVersion(t *testing.T) {
	spec, loaded, snap1 := makeSample(t)
	params := spec.DefaultParams
	params.ColorTolerance++
	snap2, err := core.NewSnapshot(spec.ID, spec.ImageSHA256, params, spec.Seeds, spec.Quantization)
	if err != nil {
		t.Fatalf("new snapshot: %v", err)
	}
	session := engine.NewSession(spec.PreviewTolerance)
	if _, err := session.Preview(loaded.Image, snap1, spec.PreviewDelayMS); err != nil {
		t.Fatalf("preview: %v", err)
	}
	store, err := engine.OpenStore(filepath.Join(t.TempDir(), "mix.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	_, err = session.Finalize(context.Background(), loaded.Image, snap2, spec.TruthFingerprint, store, engine.DefaultRunConfig())
	if core.CodeOf(err) != core.CodeVersion {
		t.Fatalf("error = %v, want E_VER", err)
	}
	appErr, ok := err.(*core.Error)
	if !ok || appErr.Left == "" || appErr.Right == "" || appErr.Left == appErr.Right {
		t.Fatalf("version error does not expose both versions: %#v", err)
	}
}

func TestCheckpointTamperInvalidatesWholeSnapshot(t *testing.T) {
	spec, loaded, snapshot := makeSample(t)
	path := filepath.Join(t.TempDir(), "tamper.db")
	store, err := engine.OpenStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := engine.Execute(context.Background(), loaded.Image, snapshot, spec.TruthFingerprint, engine.ModeFull, store, engine.DefaultRunConfig()); err != nil {
		t.Fatalf("full run: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec(`UPDATE stages SET payload = x'7b7d' WHERE canonical_index = 0`); err != nil {
		t.Fatalf("tamper payload: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	store, err = engine.OpenStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store.Close()
	resumed, err := engine.Execute(context.Background(), loaded.Image, snapshot, spec.TruthFingerprint, engine.ModeResume, store, engine.DefaultRunConfig())
	if err != nil {
		t.Fatalf("resume fallback: %v", err)
	}
	if resumed.CheckpointReused {
		t.Fatal("tampered checkpoint was reused")
	}
	if resumed.InvalidatedReason == "" {
		t.Fatal("no invalidation reason recorded")
	}
	info, err := store.SnapshotInfo(snapshot.Version)
	if err != nil {
		t.Fatalf("snapshot info: %v", err)
	}
	if info.Status != "complete" {
		t.Fatalf("snapshot status after fallback = %s", info.Status)
	}
}

func TestVersionTamperInvalidatesSnapshot(t *testing.T) {
	spec, loaded, snapshot := makeSample(t)
	path := filepath.Join(t.TempDir(), "version.db")
	store, err := engine.OpenStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := engine.Execute(context.Background(), loaded.Image, snapshot, spec.TruthFingerprint, engine.ModeFull, store, engine.DefaultRunConfig()); err != nil {
		t.Fatalf("full run: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE stages SET param_version = 'tampered-version' WHERE canonical_index = 0`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	store, err = engine.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = store.LoadCheckpoint(snapshot, spec.TruthFingerprint)
	if core.CodeOf(err) != core.CodeResume {
		t.Fatalf("error = %v, want E_RESUME", err)
	}
	info, err := store.SnapshotInfo(snapshot.Version)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != "invalid" {
		t.Fatalf("status = %s, want invalid", info.Status)
	}
}

func TestSchedulingOrderDoesNotChangeOutput(t *testing.T) {
	spec, loaded, snapshot := makeSample(t)
	base := t.TempDir()
	run := func(name string, maxParallel int) *engine.RunResult {
		store, err := engine.OpenStore(filepath.Join(base, name+".db"))
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
		defer store.Close()
		cfg := engine.DefaultRunConfig()
		cfg.Budget.MaxParallel = maxParallel
		result, err := engine.Execute(context.Background(), loaded.Image, snapshot, spec.TruthFingerprint, engine.ModeFull, store, cfg)
		if err != nil {
			t.Fatalf("%s run: %v", name, err)
		}
		return result
	}
	serial := run("serial", 1)
	parallel := run("parallel", 4)
	repeated := run("repeated", 2)
	if serial.MaskHash != parallel.MaskHash || serial.MaskHash != repeated.MaskHash {
		t.Fatal("mask changed with scheduling order")
	}
	if !engine.SequenceEqual(serial.Records, parallel.Records) || !engine.SequenceEqual(serial.Records, repeated.Records) {
		t.Fatal("stage sequence changed with scheduling order")
	}
	for i := range serial.Regions {
		if serial.Regions[i].Area != parallel.Regions[i].Area ||
			serial.Regions[i].Centroid != parallel.Regions[i].Centroid ||
			serial.Regions[i].Centroid != repeated.Regions[i].Centroid {
			t.Fatalf("region stats changed at %d", i)
		}
	}
}

func TestBlockFSMRejectsIllegalTransitionsWithoutMutation(t *testing.T) {
	fsm := engine.NewBlockFSM(7)
	if err := fsm.Commit(engine.StageGrow, "v1", "d1"); core.CodeOf(err) != core.CodeState {
		t.Fatalf("pending commit error = %v, want E_STATE", err)
	}
	if got := fsm.Snapshot(engine.StageGrow).State; got != engine.StatePending {
		t.Fatalf("state mutated to %s", got)
	}
	if err := fsm.Start(engine.StageGrow, "v1"); err != nil {
		t.Fatalf("start grow: %v", err)
	}
	if err := fsm.Commit(engine.StageGrow, "v1", "d1"); err != nil {
		t.Fatalf("commit grow: %v", err)
	}
	if err := fsm.Start(engine.StageGrow, "v1"); core.CodeOf(err) != core.CodeState {
		t.Fatalf("committed start error = %v, want E_STATE", err)
	}
	if got := fsm.Snapshot(engine.StageGrow).State; got != engine.StateCommitted {
		t.Fatalf("state changed to %s after illegal transition", got)
	}
	if err := fsm.Start(engine.StageClean, "v2"); core.CodeOf(err) != core.CodeVersion {
		t.Fatalf("frozen version error = %v, want E_VER", err)
	}
	if got := fsm.Snapshot(engine.StageClean).State; got != engine.StatePending {
		t.Fatalf("clean state changed to %s", got)
	}
	other := engine.NewBlockFSM(8)
	if err := other.Start(engine.StageGrow, "v1"); err != nil {
		t.Fatal(err)
	}
	if err := other.Commit(engine.StageGrow, "v2", "d"); core.CodeOf(err) != core.CodeVersion {
		t.Fatalf("commit version mismatch error = %v, want E_VER", err)
	}
	if got := other.Snapshot(engine.StageGrow).State; got != engine.StateProcessing {
		t.Fatalf("state changed to %s after version mismatch", got)
	}
}

func TestSchedulerQueuesWithoutDropping(t *testing.T) {
	scheduler, err := engine.NewScheduler(engine.Budget{
		MaxParallel:      3,
		MaxPerBlockBytes: 100,
		GlobalBytes:      150,
	})
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	jobs := make([]engine.Job, 0, 4)
	for i := 0; i < 4; i++ {
		i := i
		jobs = append(jobs, engine.Job{
			BlockID: 1 + i%2,
			Run: func(ctx context.Context) error {
				release, err := scheduler.Acquire(ctx, 1+i%2, 60, "stage")
				if err != nil {
					return err
				}
				time.Sleep(5 * time.Millisecond)
				release()
				return nil
			},
		})
	}
	if err := scheduler.Run(context.Background(), jobs); err != nil {
		t.Fatalf("run: %v", err)
	}
	stats := scheduler.Stats()
	if stats.CompletedJobs != len(jobs) {
		t.Fatalf("completed=%d want=%d", stats.CompletedJobs, len(jobs))
	}
	if len(stats.QueuedEvents) == 0 {
		t.Fatal("expected queued events")
	}
	if stats.MaxActiveObserved > 3 {
		t.Fatalf("max active=%d exceeds 3", stats.MaxActiveObserved)
	}
}

func makeSample(t *testing.T) (core.SampleSpec, *sample.LoadedSample, core.Snapshot) {
	t.Helper()
	dir := t.TempDir()
	manifest, err := sample.Generate(sample.GenerateConfig{Seed: 424242, OutDir: dir, Sizes: []int{64}})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	spec := manifest.Samples[0]
	loaded, err := sample.LoadSample(dir, spec)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	snapshot, err := core.NewSnapshot(spec.ID, spec.ImageSHA256, spec.DefaultParams, spec.Seeds, spec.Quantization)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return spec, loaded, snapshot
}
