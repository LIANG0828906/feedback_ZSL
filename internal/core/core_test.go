package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testSample(t *testing.T, channels int, noisy, low bool) LoadedSample {
	t.Helper()
	spec := SampleSpec{
		Name:        "test-sample",
		Width:       192,
		Height:      128,
		Channels:    channels,
		Seed:        99173,
		Noisy:       noisy,
		LowContrast: low,
	}
	sample, err := GenerateSample(t.TempDir(), spec)
	if err != nil {
		t.Fatalf("GenerateSample: %v", err)
	}
	return sample
}

func TestSampleGeneratorDeterministicAndLoaderValidation(t *testing.T) {
	spec := SampleSpec{Name: "deterministic", Width: 160, Height: 112, Channels: 3, Seed: 7123, Noisy: true, LowContrast: true}
	first, err := GenerateSample(t.TempDir(), spec)
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateSample(t.TempDir(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if first.Manifest.Fingerprint != second.Manifest.Fingerprint {
		t.Fatalf("same seed produced different fingerprints: %s != %s", first.Manifest.Fingerprint, second.Manifest.Fingerprint)
	}
	if len(first.Manifest.References) != len(StageOrder) {
		t.Fatalf("want %d reference masks, got %d", len(StageOrder), len(first.Manifest.References))
	}
	loaded, err := LoadSample(first.Dir)
	if err != nil {
		t.Fatalf("LoadSample: %v", err)
	}
	if loaded.Manifest.Fingerprint != first.Manifest.Fingerprint {
		t.Fatal("loader fingerprint changed")
	}
	if err := ValidateReferences(loaded); err != nil {
		t.Fatalf("ValidateReferences: %v", err)
	}

	raw, _ := os.ReadFile(filepath.Join(first.Dir, "image.png"))
	corruptDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(corruptDir, "manifest.json"), mustJSON(first.Manifest), 0o644)
	badPNG := append([]byte(nil), raw[:20]...)
	badPNG = append(badPNG, 0xff, 0x00)
	_ = os.WriteFile(filepath.Join(corruptDir, "image.png"), badPNG, 0o644)
	if _, err := LoadSample(corruptDir); CodeOf(err) != CodeDecode {
		t.Fatalf("corrupt image should return E_DECODE, got %v", err)
	}

	metaDir := t.TempDir()
	mutated := first.Manifest
	mutated.Width++
	_ = os.WriteFile(filepath.Join(metaDir, "manifest.json"), mustJSON(mutated), 0o644)
	_ = os.WriteFile(filepath.Join(metaDir, "image.png"), raw, 0o644)
	_, err = LoadSample(metaDir)
	if CodeOf(err) != CodeMeta || FieldOf(err) != "width" {
		t.Fatalf("width mismatch should return E_META width, got %v", err)
	}

	metaDir2 := t.TempDir()
	mutated = first.Manifest
	mutated.Channels = 1
	_ = os.WriteFile(filepath.Join(metaDir2, "manifest.json"), mustJSON(mutated), 0o644)
	_ = os.WriteFile(filepath.Join(metaDir2, "image.png"), raw, 0o644)
	_, err = LoadSample(metaDir2)
	if CodeOf(err) != CodeMeta || FieldOf(err) != "channels" {
		t.Fatalf("channel mismatch should return E_META channels, got %v", err)
	}
}

func TestLowContrastSampleProducesReferenceEdges(t *testing.T) {
	spec := SampleSpec{Name: "low-contrast-reference", Width: 320, Height: 240, Channels: 1, Seed: 7788, Noisy: true, LowContrast: true}
	sample, err := GenerateSample(t.TempDir(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if sample.Manifest.Truth.EdgeCoverage < 0.002 {
		t.Fatalf("low-contrast reference coverage is too small: %f", sample.Manifest.Truth.EdgeCoverage)
	}
	for _, ref := range sample.Manifest.References {
		if ref.Coverage <= 0 {
			t.Fatalf("reference %s is empty", ref.Stage)
		}
	}
}

func TestParameterValidationFields(t *testing.T) {
	p := DefaultParams()
	p.LowThreshold = 90
	p.HighThreshold = 30
	if err := p.Validate(); CodeOf(err) != CodeParam || FieldOf(err) != "lowThreshold" {
		t.Fatalf("inverted thresholds: %v", err)
	}
	p = DefaultParams()
	p.ThinIterations = 99
	if err := p.Validate(); CodeOf(err) != CodeParam || FieldOf(err) != "thinIterations" {
		t.Fatalf("thin bounds: %v", err)
	}
	p = DefaultParams()
	p.OpenRadius = -1
	if err := p.Validate(); CodeOf(err) != CodeParam || FieldOf(err) != "openRadius" {
		t.Fatalf("open radius bounds: %v", err)
	}
}

func TestGradientAndThresholdPreserveChannels(t *testing.T) {
	for _, channels := range []int{1, 3} {
		sample := testSample(t, channels, true, false)
		params := DefaultParams()
		stages, err := ComputeStages(sample.Image, params)
		if err != nil {
			t.Fatal(err)
		}
		for _, stage := range stages {
			if stage.Width != sample.Image.Width || stage.Height != sample.Image.Height || stage.Channels != channels {
				t.Fatalf("%s shape=%dx%dx%d", stage.Stage, stage.Width, stage.Height, stage.Channels)
			}
		}
	}
}

func TestMorphologyChainAndBounds(t *testing.T) {
	src := NewImage(80, 64, 3)
	for c := 0; c < 3; c++ {
		for y := 16; y < 48; y++ {
			for x := 20; x < 60; x++ {
				src.Set(x, y, c, 255)
			}
		}
		src.Set(5, 5, c, 255)
	}
	opened, err := MorphOpen(src, 2)
	if err != nil {
		t.Fatal(err)
	}
	if opened.Width != src.Width || opened.Height != src.Height || opened.Channels != src.Channels {
		t.Fatal("open changed image shape")
	}
	if opened.At(40, 32, 0) != 255 {
		t.Fatal("open removed a thick region")
	}
	if opened.At(5, 5, 0) != 0 {
		t.Fatal("open failed to remove an isolated pixel")
	}
	closed, err := MorphClose(src, 2)
	if err != nil {
		t.Fatal(err)
	}
	thinned, err := Thin(closed, 2)
	if err != nil {
		t.Fatal(err)
	}
	pruned, err := PruneShortComponents(thinned, 4)
	if err != nil {
		t.Fatal(err)
	}
	for name, im := range map[string]Image{"open": opened, "close": closed, "thin": thinned, "prune": pruned} {
		if im.Width != src.Width || im.Height != src.Height || im.Channels != src.Channels {
			t.Fatalf("%s changed image shape", name)
		}
	}
	params := DefaultParams()
	params.OpenRadius = 2
	params.CloseRadius = 1
	params.ThinIterations = 1
	params.PruneLength = 6
	stages, err := ComputeStages(src, params)
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != len(StageOrder) {
		t.Fatalf("stage count=%d", len(stages))
	}
	for i, stage := range stages {
		if stage.Stage != StageOrder[i] {
			t.Fatalf("stage order mismatch at %d: %s", i, stage.Stage)
		}
		if stage.Width != src.Width || stage.Height != src.Height || stage.Channels != src.Channels {
			t.Fatalf("%s changed shape", stage.Stage)
		}
	}
	if _, err := MorphOpen(src, -1); CodeOf(err) != CodeParam || FieldOf(err) != "openRadius" {
		t.Fatalf("invalid open radius: %v", err)
	}
	if _, err := Thin(src, 33); CodeOf(err) != CodeParam || FieldOf(err) != "thinIterations" {
		t.Fatalf("invalid thin iterations: %v", err)
	}
	if _, err := PruneShortComponents(src, 257); CodeOf(err) != CodeParam || FieldOf(err) != "pruneLength" {
		t.Fatalf("invalid prune length: %v", err)
	}
}

func TestWorkerResourceParameterValidation(t *testing.T) {
	if _, err := NewWorkerPool(0, 4096); CodeOf(err) != CodeParam || FieldOf(err) != "workers" {
		t.Fatalf("invalid worker count: %v", err)
	}
	if _, err := NewWorkerPool(2, 1024); CodeOf(err) != CodeParam || FieldOf(err) != "perWorkerPixels" {
		t.Fatalf("invalid pixel buffer: %v", err)
	}
}

func TestWorkerPoolBoundsParallelismAndRecordsWaitReason(t *testing.T) {
	pool, err := NewWorkerPool(2, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	defer closeRelease()
	started := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = pool.Submit(context.Background(), "queued", 1024, func(buffer []byte) error {
				started <- struct{}{}
				<-release
				return nil
			})
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("workers did not start")
		}
	}
	var stats PoolStats
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stats = pool.Stats()
		if stats.WaitReasons["worker-slot-exhausted"] > 0 || stats.WaitReasons["pixel-buffer-exhausted"] > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	closeRelease()
	wg.Wait()
	stats = pool.Stats()
	if stats.MaxParallel > stats.WorkerCount {
		t.Fatalf("max parallel %d exceeded workers %d", stats.MaxParallel, stats.WorkerCount)
	}
	if stats.WaitReasons["worker-slot-exhausted"] == 0 && stats.WaitReasons["pixel-buffer-exhausted"] == 0 {
		t.Fatalf("queued work did not record a wait reason: %+v", stats)
	}
}

func TestPreviewLatencyAndTolerance(t *testing.T) {
	spec := SampleSpec{Name: "preview", Width: 640, Height: 480, Channels: 1, Seed: 8123, Noisy: true}
	sample, err := GenerateSample(t.TempDir(), spec)
	if err != nil {
		t.Fatal(err)
	}
	params := DefaultParams()
	preview, err := ComputePreview(sample.Image, params, sample.Manifest.Truth)
	if err != nil {
		t.Fatal(err)
	}
	if !raceEnabled && preview.ElapsedMS > 100 {
		t.Fatalf("preview took %.2fms > 100ms", preview.ElapsedMS)
	}
	stages, err := ComputeStages(sample.Image, params)
	if err != nil {
		t.Fatal(err)
	}
	exact := stages[len(stages)-1].Image()
	exactStats := AnalyzeGeometry(exact, sample.Manifest.Truth)
	divergence := ComparePreviewExact(preview.Mask, exact, preview.Stats, exactStats, DefaultTolerance())
	if !divergence.Pass {
		t.Fatalf("preview diverged: %+v", divergence)
	}
}

func TestStateMachineIllegalTransitionPreservesState(t *testing.T) {
	grid := BuildGrid(64, 64, 32, 32)
	machine := NewStateMachine(grid)
	if err := machine.Transition(StageGradient, 0, BlockCommitted); CodeOf(err) != CodeState {
		t.Fatalf("want E_STATE, got %v", err)
	}
	if state, _ := machine.State(StageGradient, 0); state != BlockPending {
		t.Fatalf("illegal transition changed state to %s", state)
	}
	if err := machine.Transition(StageGradient, 0, BlockProcessing); err != nil {
		t.Fatal(err)
	}
	if err := machine.Transition(StageGradient, 0, BlockFailed); err != nil {
		t.Fatal(err)
	}
	if err := machine.Transition(StageGradient, 0, BlockProcessing); CodeOf(err) != CodeState {
		t.Fatalf("failed must return to pending first, got %v", err)
	}
	if err := machine.Transition(StageGradient, 0, BlockPending); err != nil {
		t.Fatal(err)
	}
	if err := machine.Transition(StageGradient, 0, BlockProcessing); err != nil {
		t.Fatal(err)
	}
	if err := machine.Transition(StageGradient, 0, BlockCommitted); err != nil {
		t.Fatal(err)
	}
}

func TestThreeRunPathsAndCheckpointRestore(t *testing.T) {
	sample := testSample(t, 3, true, false)
	params := DefaultParams()
	grid := BuildGrid(sample.Image.Width, sample.Image.Height, 48, 48)
	dbPath := filepath.Join(t.TempDir(), "checkpoint.db")
	store, err := OpenCheckpointStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	opts := RunOptions{Grid: grid, Workers: 3, PerWorkerPixels: 32 * 1024, Checkpoint: store}
	opts.Mode = RunRealtime
	realtime, err := RunPipeline(context.Background(), sample.Image, params, sample.Manifest.Truth, opts)
	if err != nil {
		t.Fatal(err)
	}
	if realtime.Status != "complete" {
		t.Fatalf("realtime status=%s failed=%v", realtime.Status, realtime.FailedBlocks)
	}
	count, err := store.CommitCount(context.Background(), realtime.ImageFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if want := len(StageOrder) * len(grid.Blocks); count != want {
		t.Fatalf("checkpoint commits=%d want=%d", count, want)
	}
	rows, err := store.db.Query(`SELECT stage, block_index FROM commits WHERE image_fp=? ORDER BY seq`, realtime.ImageFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	var committed []CommitKey
	for rows.Next() {
		var key CommitKey
		if err := rows.Scan(&key.Stage, &key.Block); err != nil {
			t.Fatal(err)
		}
		committed = append(committed, key)
	}
	_ = rows.Close()
	wantCommits := allCommitKeys(grid)
	if len(committed) != len(wantCommits) {
		t.Fatalf("commit row count=%d want=%d", len(committed), len(wantCommits))
	}
	for i := range wantCommits {
		if committed[i] != wantCommits[i] {
			t.Fatalf("checkpoint commit order mismatch at %d: %v != %v", i, committed[i], wantCommits[i])
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenCheckpointStore(dbPath)
	if err != nil {
		t.Fatalf("reopen checkpoint store: %v", err)
	}
	snapshot, err := store.LoadSnapshot(context.Background(), realtime.ImageFingerprint, params, AlgorithmVersion, sample.Image.Width, sample.Image.Height, sample.Image.Channels, grid)
	if err != nil {
		var appErr *AppError
		_ = errors.As(err, &appErr)
		if appErr != nil {
			t.Fatalf("LoadSnapshot after reopen: %+v unwrap=%v", appErr, appErr.Err)
		}
		t.Fatalf("LoadSnapshot after reopen: %+v", err)
	}
	if !snapshot.Valid || !snapshot.Complete {
		t.Fatalf("snapshot invalid/incomplete: %+v", snapshot)
	}
	opts.Checkpoint = store
	opts.Mode = RunResume
	resumed, err := RunPipeline(context.Background(), sample.Image, params, sample.Manifest.Truth, opts)
	if err != nil {
		t.Fatal(err)
	}
	opts.Mode = RunFull
	opts.Checkpoint = nil
	full, err := RunPipeline(context.Background(), sample.Image, params, sample.Manifest.Truth, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareRuns(realtime, resumed); err != nil {
		t.Fatalf("realtime vs resume: %v", err)
	}
	if err := CompareRuns(realtime, full); err != nil {
		t.Fatalf("realtime vs full: %v", err)
	}
	if resumed.RestoredCommits != len(StageOrder)*len(grid.Blocks) {
		t.Fatalf("restored commits=%d", resumed.RestoredCommits)
	}
}

func TestCheckpointParameterVersionMismatchInvalidatesWholeSnapshot(t *testing.T) {
	sample := testSample(t, 1, true, false)
	params := DefaultParams()
	grid := BuildGrid(sample.Image.Width, sample.Image.Height, 64, 64)
	store, err := OpenCheckpointStore(filepath.Join(t.TempDir(), "checkpoint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	opts := RunOptions{Mode: RunRealtime, Grid: grid, Workers: 2, PerWorkerPixels: 32 * 1024, Checkpoint: store}
	run, err := RunPipeline(context.Background(), sample.Image, params, sample.Manifest.Truth, opts)
	if err != nil {
		t.Fatal(err)
	}
	rawDB, err := sql.Open("sqlite", "file:"+store.Path()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer rawDB.Close()
	if _, err := rawDB.Exec(`UPDATE snapshots SET param_version='stale-param-version' WHERE commit_seq=(SELECT MAX(commit_seq) FROM snapshots WHERE image_fp=?)`, run.ImageFingerprint); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.LoadSnapshot(context.Background(), run.ImageFingerprint, params, AlgorithmVersion, sample.Image.Width, sample.Image.Height, sample.Image.Channels, grid)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Valid || !snapshot.Invalidated {
		t.Fatalf("parameter version mismatch should invalidate snapshot: %+v", snapshot)
	}
}

func TestInterruptedCheckpointResumeMatchesFullRun(t *testing.T) {
	sample := testSample(t, 3, true, false)
	params := DefaultParams()
	grid := BuildGrid(sample.Image.Width, sample.Image.Height, 48, 48)
	store, err := OpenCheckpointStore(filepath.Join(t.TempDir(), "checkpoint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithCancel(context.Background())
	control := NewRunControl()
	opts := RunOptions{Mode: RunRealtime, Grid: grid, Workers: 3, PerWorkerPixels: 32 * 1024, Checkpoint: store, Control: control}
	var once sync.Once
	lastGradientBlock := len(grid.Blocks) - 1
	opts.OnEvent = func(ev EngineEvent) {
		if ev.Type == "transition" && ev.Stage == StageGradient && ev.Block == lastGradientBlock && ev.State == BlockCommitted {
			once.Do(func() {
				control.SetPaused(true)
				go func() {
					time.Sleep(20 * time.Millisecond)
					cancel()
				}()
			})
		}
	}
	if _, err := RunPipeline(ctx, sample.Image, params, sample.Manifest.Truth, opts); CodeOf(err) != CodeCanceled {
		t.Fatalf("interrupted run should be canceled, got %v", err)
	}
	partial, err := store.LoadSnapshot(context.Background(), FingerprintImage(sample.Image), params, AlgorithmVersion, sample.Image.Width, sample.Image.Height, sample.Image.Channels, grid)
	if err != nil {
		t.Fatal(err)
	}
	if !partial.Valid || partial.Complete || !partial.CompleteSet[StageGradient] {
		t.Fatalf("expected a valid gradient-only prefix, got %+v", partial)
	}
	opts.Mode = RunResume
	opts.Control = NewRunControl()
	opts.OnEvent = nil
	resumed, err := RunPipeline(context.Background(), sample.Image, params, sample.Manifest.Truth, opts)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != "complete" {
		t.Fatalf("resume failed: %+v", resumed.FailedBlocks)
	}
	full, err := RunPipeline(context.Background(), sample.Image, params, sample.Manifest.Truth, RunOptions{
		Mode: RunFull, Grid: grid, Workers: 4, PerWorkerPixels: 32 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareRuns(full, resumed); err != nil {
		t.Fatalf("resumed run differs from full run: %v", err)
	}
}

func TestRealtimeParameterSwitchWipesPreviousVersion(t *testing.T) {
	sample := testSample(t, 1, true, false)
	grid := BuildGrid(sample.Image.Width, sample.Image.Height, 64, 64)
	store, err := OpenCheckpointStore(filepath.Join(t.TempDir(), "checkpoint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first := DefaultParams()
	second := first
	second.LowThreshold = 4
	second.HighThreshold = 12
	for _, params := range []Params{first, second} {
		_, err := RunPipeline(context.Background(), sample.Image, params, sample.Manifest.Truth, RunOptions{
			Mode: RunRealtime, Grid: grid, Workers: 2, PerWorkerPixels: 32 * 1024, Checkpoint: store,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := store.LoadSnapshot(context.Background(), FingerprintImage(sample.Image), second, AlgorithmVersion, sample.Image.Width, sample.Image.Height, sample.Image.Channels, grid)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Valid || !snapshot.Complete {
		t.Fatalf("new realtime snapshot should be complete: %+v", snapshot)
	}
	var n int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM snapshots WHERE image_fp=?`, FingerprintImage(sample.Image)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if want := len(StageOrder) * len(grid.Blocks); n != want {
		t.Fatalf("old parameter rows leaked: rows=%d want=%d", n, want)
	}
}

func TestCheckpointCorruptionInvalidatesWholeSnapshot(t *testing.T) {
	sample := testSample(t, 1, true, false)
	params := DefaultParams()
	grid := BuildGrid(sample.Image.Width, sample.Image.Height, 64, 64)
	store, err := OpenCheckpointStore(filepath.Join(t.TempDir(), "checkpoint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	opts := RunOptions{Mode: RunRealtime, Grid: grid, Workers: 2, PerWorkerPixels: 32 * 1024, Checkpoint: store}
	run, err := RunPipeline(context.Background(), sample.Image, params, sample.Manifest.Truth, opts)
	if err != nil {
		t.Fatal(err)
	}
	rawDB, err := sql.Open("sqlite", "file:"+store.Path()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer rawDB.Close()
	if _, err := rawDB.Exec(`UPDATE snapshots SET content_hash='corrupted' WHERE commit_seq=(SELECT MIN(commit_seq) FROM snapshots WHERE image_fp=?)`, run.ImageFingerprint); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.LoadSnapshot(context.Background(), run.ImageFingerprint, params, AlgorithmVersion, sample.Image.Width, sample.Image.Height, sample.Image.Channels, grid)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Valid || !snapshot.Invalidated {
		t.Fatalf("corruption should invalidate snapshot: %+v", snapshot)
	}
	count, err := store.CommitCount(context.Background(), run.ImageFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("corrupt snapshot was not fully wiped: %d", count)
	}
}

func TestCheckpointVersionMismatchInvalidatesWholeSnapshot(t *testing.T) {
	sample := testSample(t, 1, false, false)
	params := DefaultParams()
	grid := BuildGrid(sample.Image.Width, sample.Image.Height, 64, 64)
	store, err := OpenCheckpointStore(filepath.Join(t.TempDir(), "checkpoint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	opts := RunOptions{Mode: RunRealtime, Grid: grid, Workers: 2, PerWorkerPixels: 32 * 1024, Checkpoint: store}
	run, err := RunPipeline(context.Background(), sample.Image, params, sample.Manifest.Truth, opts)
	if err != nil {
		t.Fatal(err)
	}
	rawDB, err := sql.Open("sqlite", "file:"+store.Path()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer rawDB.Close()
	if _, err := rawDB.Exec(`UPDATE snapshots SET algorithm_version='older-algorithm' WHERE commit_seq=(SELECT MAX(commit_seq) FROM snapshots WHERE image_fp=?)`, run.ImageFingerprint); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.LoadSnapshot(context.Background(), run.ImageFingerprint, params, AlgorithmVersion, sample.Image.Width, sample.Image.Height, sample.Image.Channels, grid)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Valid || !snapshot.Invalidated {
		t.Fatalf("version mismatch should invalidate snapshot: %+v", snapshot)
	}
}

func TestFaultIsolationAndDeterministicParallelism(t *testing.T) {
	sample := testSample(t, 3, true, false)
	params := DefaultParams()
	grid := BuildGrid(sample.Image.Width, sample.Image.Height, 32, 32)
	baseline := RunOptions{Mode: RunFull, Grid: grid, Workers: 1, PerWorkerPixels: 24 * 1024}
	expected, err := RunPipeline(context.Background(), sample.Image, params, sample.Manifest.Truth, baseline)
	if err != nil {
		t.Fatal(err)
	}
	parallel := baseline
	parallel.Workers = 6
	actual, err := RunPipeline(context.Background(), sample.Image, params, sample.Manifest.Truth, parallel)
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareRuns(expected, actual); err != nil {
		t.Fatalf("parallel output differs: %v", err)
	}
	if actual.PoolStats.MaxParallel > actual.PoolStats.WorkerCount {
		t.Fatalf("parallelism exceeded worker count")
	}

	faultKey := CommitKey{Stage: StageClose, Block: 3}
	faulted := baseline
	faulted.Faults = map[CommitKey]string{faultKey: "panic: injected isolated failure"}
	failedRun, err := RunPipeline(context.Background(), sample.Image, params, sample.Manifest.Truth, faulted)
	if err != nil {
		t.Fatal(err)
	}
	if failedRun.Status != "failed" {
		t.Fatalf("fault run status=%s", failedRun.Status)
	}
	foundFailed := false
	for _, matrix := range failedRun.BlockStates {
		if matrix.Stage == StageClose {
			if matrix.States[faultKey.Block] != BlockFailed {
				t.Fatalf("faulted block state=%s", matrix.States[faultKey.Block])
			}
			for i, state := range matrix.States {
				if i != faultKey.Block && state != BlockCommitted {
					t.Fatalf("fault polluted block %d state=%s", i, state)
				}
			}
			foundFailed = true
		}
	}
	if !foundFailed {
		t.Fatal("fault stage not found")
	}
	retried, err := RunPipeline(context.Background(), sample.Image, params, sample.Manifest.Truth, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareRuns(expected, retried); err != nil {
		t.Fatalf("retry differs: %v", err)
	}
}

func TestStateMatrixHasAllBlocksAndStages(t *testing.T) {
	sample := testSample(t, 1, false, false)
	grid := BuildGrid(sample.Image.Width, sample.Image.Height, 64, 64)
	run, err := RunPipeline(context.Background(), sample.Image, DefaultParams(), sample.Manifest.Truth, RunOptions{Mode: RunFull, Grid: grid, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(run.BlockStates) != len(StageOrder) {
		t.Fatalf("stage matrices=%d", len(run.BlockStates))
	}
	for _, matrix := range run.BlockStates {
		if len(matrix.States) != len(grid.Blocks) {
			t.Fatalf("block state count mismatch")
		}
	}
	if run.ElapsedMS <= 0 {
		t.Fatal("missing elapsed time")
	}
}

func TestBlockGridDoesNotAffectFinalPixels(t *testing.T) {
	sample := testSample(t, 3, true, false)
	params := DefaultParams()
	first, err := RunPipeline(context.Background(), sample.Image, params, sample.Manifest.Truth, RunOptions{
		Mode: RunFull, Grid: BuildGrid(sample.Image.Width, sample.Image.Height, 32, 24), Workers: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := RunPipeline(context.Background(), sample.Image, params, sample.Manifest.Truth, RunOptions{
		Mode: RunFull, Grid: BuildGrid(sample.Image.Width, sample.Image.Height, 96, 80), Workers: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Final.Width != second.Final.Width || first.Final.Height != second.Final.Height || first.Final.Channels != second.Final.Channels {
		t.Fatal("grid changed final shape")
	}
	for i := range first.Final.Pix {
		if first.Final.Pix[i] != second.Final.Pix[i] {
			t.Fatalf("grid changed pixel at index %d", i)
		}
	}
	if GeometryDeviation(first.Stats, second.Stats) != 0 {
		t.Fatalf("grid changed geometry: %f", GeometryDeviation(first.Stats, second.Stats))
	}
}

func TestRunControlPauseAndCancel(t *testing.T) {
	sample := testSample(t, 1, true, false)
	grid := BuildGrid(sample.Image.Width, sample.Image.Height, 48, 48)
	control := NewRunControl()
	control.SetPaused(true)
	done := make(chan error, 1)
	go func() {
		_, err := RunPipeline(context.Background(), sample.Image, DefaultParams(), sample.Manifest.Truth, RunOptions{
			Mode: RunFull, Grid: grid, Workers: 2, Control: control,
		})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("paused run completed early: %v", err)
	case <-time.After(80 * time.Millisecond):
	}
	control.SetPaused(false)
	if err := <-done; err != nil {
		t.Fatalf("resumed run failed: %v", err)
	}

	control.SetPaused(true)
	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan error, 1)
	go func() {
		_, err := RunPipeline(ctx, sample.Image, DefaultParams(), sample.Manifest.Truth, RunOptions{
			Mode: RunFull, Grid: grid, Workers: 2, Control: control,
		})
		done <- err
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	if err := <-done; CodeOf(err) != CodeCanceled {
		t.Fatalf("canceled run should return E_CANCELED, got %v", err)
	}
}

func TestPreviewLargeImageDownscaleRecorded(t *testing.T) {
	spec := SampleSpec{Name: "large-preview", Width: 2048, Height: 1152, Channels: 1, Seed: 992, Noisy: true}
	sample, err := GenerateSample(t.TempDir(), spec)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := ComputePreview(sample.Image, DefaultParams(), sample.Manifest.Truth)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Policy.DownsampleFactor <= 1 {
		t.Fatalf("large image should use downsampling, got %d", preview.Policy.DownsampleFactor)
	}
	if !raceEnabled && preview.ElapsedMS > 100 {
		t.Fatalf("large preview took %.2fms > 100ms", preview.ElapsedMS)
	}
}

func TestPreviewToleranceWithConfiguredMorphology(t *testing.T) {
	spec := SampleSpec{Name: "preview-morph", Width: 960, Height: 640, Channels: 3, Seed: 3344, Noisy: true}
	sample, err := GenerateSample(t.TempDir(), spec)
	if err != nil {
		t.Fatal(err)
	}
	params := DefaultParams()
	params.OpenRadius = 2
	params.CloseRadius = 2
	params.ThinIterations = 4
	params.PruneLength = 16
	preview, err := ComputePreview(sample.Image, params, sample.Manifest.Truth)
	if err != nil {
		t.Fatal(err)
	}
	if !raceEnabled && preview.ElapsedMS > 100 {
		t.Fatalf("configured preview took %.2fms > 100ms", preview.ElapsedMS)
	}
	stages, err := ComputeStages(sample.Image, params)
	if err != nil {
		t.Fatal(err)
	}
	exact := stages[len(stages)-1].Image()
	exactStats := AnalyzeGeometry(exact, sample.Manifest.Truth)
	divergence := ComparePreviewExact(preview.Mask, exact, preview.Stats, exactStats, DefaultTolerance())
	if !divergence.Pass {
		for i, l := range preview.Stats.Lines {
			t.Logf("preview line %d ch%d obs=(%.1f,%.1f)-(%.1f,%.1f) err=%.1f", i, l.Channel, l.X1, l.Y1, l.X2, l.Y2, l.EndpointErr)
		}
		for i, c := range preview.Stats.Circles {
			t.Logf("preview circle %d ch%d obs=(%.1f,%.1f) d=%.1f errs=%.1f/%.1f", i, c.Channel, c.CX, c.CY, c.Diameter, c.CenterErr, c.DiameterErr)
		}
		t.Fatalf("configured morphology diverged: %+v previewCoverage=%f exactCoverage=%f previewPolicy=%+v", divergence, preview.Stats.Coverage, exactStats.Coverage, preview.Policy)
	}
}

func mustJSON(v any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}

func TestMain(m *testing.M) {
	start := time.Now()
	code := m.Run()
	_ = start
	os.Exit(code)
}
