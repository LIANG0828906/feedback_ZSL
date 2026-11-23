package forensic

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func testSample(t *testing.T) *Image {
	t.Helper()
	dir := t.TempDir()
	generated, err := GenerateSamples(dir)
	if err != nil {
		t.Fatalf("generate samples: %v", err)
	}
	img, err := LoadSample(generated.Path, "game_tex_rgb_160x112")
	if err != nil {
		t.Fatalf("load sample: %v", err)
	}
	return img
}

func mustRun(t *testing.T, engine *Engine, p Params, opt RunOptions) RunResult {
	t.Helper()
	result, err := engine.Run(context.Background(), p, opt, NewControl())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !result.Completed {
		t.Fatal("run did not complete")
	}
	return result
}

func TestSampleGenerationAndValidation(t *testing.T) {
	img := testSample(t)
	if img.Width != 160 || img.Height != 112 || img.Channels != 3 {
		t.Fatalf("unexpected sample geometry: %dx%dx%d", img.Width, img.Height, img.Channels)
	}
	if len(img.Mask) != img.Width*img.Height {
		t.Fatalf("mask size %d", len(img.Mask))
	}
	p := DefaultParams()
	if err := p.Validate(img); err != nil {
		t.Fatalf("default params: %v", err)
	}
	p.BlockSize = 0
	err := p.Validate(img)
	fe, ok := AsError(err)
	if !ok || fe.Code != CodeParam || fe.Field != "blockSize" {
		t.Fatalf("want E_PARA blockSize, got %v", err)
	}
	p = DefaultParams()
	p.Window = 2
	err = p.Validate(img)
	fe, ok = AsError(err)
	if !ok || fe.Code != CodeParam || fe.Field != "window" {
		t.Fatalf("want E_PARA window, got %v", err)
	}
}

func TestDecodeMetaAndHashErrors(t *testing.T) {
	dir := t.TempDir()
	generated, err := GenerateSamples(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("missing manifest should fail")
	} else if fe, ok := AsError(err); !ok || fe.Code != CodeDecode {
		t.Fatalf("want E_DECODE, got %v", err)
	}

	raw, err := os.ReadFile(generated.Path)
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m.Images[0].Width++
	m.Images[0].MaskWidth++
	badMeta, _ := json.MarshalIndent(m, "", "  ")
	badMetaPath := filepath.Join(dir, "bad-meta.json")
	if err := os.WriteFile(badMetaPath, badMeta, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = LoadSample(badMetaPath, "game_tex_rgb_160x112")
	if fe, ok := AsError(err); !ok || fe.Code != CodeMetadata || fe.Field != "width" || fe.ImageID == "" {
		t.Fatalf("want E_META width with image id, got %v", err)
	}

	img, err := LoadSample(generated.Path, "game_tex_rgb_160x112")
	if err != nil {
		t.Fatal(err)
	}
	chunks, _ := MakeChunks(img.Width, img.Height, 16)
	expected := map[int]string{}
	for _, c := range chunks {
		expected[c.Index] = img.BlockDigest(c)
	}
	expected[5] = "broken"
	err = VerifyContentHashes(img, chunks, expected)
	if fe, ok := AsError(err); !ok || fe.Code != CodeHash || fe.Block != 5 || fe.Stage != StageHash {
		t.Fatalf("want E_HASH at block 5, got %v", err)
	}

	imagePath := filepath.Join(dir, "game_tex_rgb_160x112.png")
	corrupt, _ := os.ReadFile(imagePath)
	corrupt[len(corrupt)/2] ^= 0xff
	if err := os.WriteFile(imagePath, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = LoadSample(generated.Path, "game_tex_rgb_160x112")
	if fe, ok := AsError(err); !ok || fe.Code != CodeDecode {
		t.Fatalf("want E_DECODE for corrupted image, got %v", err)
	}
}

func TestFullRunTruthAndDeterminism(t *testing.T) {
	img := testSample(t)
	p := DefaultParams()
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "a.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := mustRun(t, NewEngine(img, store), p, RunOptions{})
	if run.TruthAgreement < 1-img.Entry.MaskTolerance {
		t.Fatalf("truth agreement %.3f below tolerance %.3f (confusion=%+v)", run.TruthAgreement, img.Entry.MaskTolerance, run.TruthConfusion)
	}
	if run.Summary.TamperedBlocks == 0 {
		t.Fatal("expected tampered blocks")
	}
	store2, err := OpenStore(filepath.Join(dir, "b.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	run2 := mustRun(t, NewEngine(img, store2), p, RunOptions{})
	cmp, err := CompareRuns(run, run2, run2)
	if err != nil || !cmp.Equal {
		t.Fatalf("repeat run differs: %v %+v", err, cmp)
	}
}

func TestAllGeneratedSamplesLoadAndMatchTruth(t *testing.T) {
	dir := t.TempDir()
	generated, err := GenerateSamples(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range generated.Manifest.Images {
		t.Run(entry.ID, func(t *testing.T) {
			img, err := LoadSample(generated.Path, entry.ID)
			if err != nil {
				t.Fatal(err)
			}
			if img.Channels != entry.Channels {
				t.Fatalf("channels=%d want %d", img.Channels, entry.Channels)
			}
			store, err := OpenStore(filepath.Join(t.TempDir(), "sample.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			run := mustRun(t, NewEngine(img, store), DefaultParams(), RunOptions{})
			if run.TruthAgreement < 1-img.Entry.MaskTolerance {
				t.Fatalf("agreement %.4f below tolerance %.2f: %+v",
					run.TruthAgreement, img.Entry.MaskTolerance, run.TruthConfusion)
			}
		})
	}
}

func TestThresholdIncrementalAndThreeWay(t *testing.T) {
	img := testSample(t)
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "incremental.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	engine := NewEngine(img, store)
	base := DefaultParams()
	base.Threshold = 0.70
	first := mustRun(t, engine, base, RunOptions{})
	loose := base
	loose.Threshold = 0.30
	second := mustRun(t, engine, loose, RunOptions{})
	if second.Summary.TamperedBlocks < first.Summary.TamperedBlocks {
		t.Fatalf("lower cutoff must not reduce tampered set: %d -> %d", first.Summary.TamperedBlocks, second.Summary.TamperedBlocks)
	}
	for _, inv := range second.Invalidations {
		if inv.Stage == StageHash {
			t.Fatalf("threshold-only update invalidated hash: %+v", inv)
		}
	}
	report, err := VerifyThreePaths(context.Background(), img, t.TempDir(), []RunStep{
		{Params: base}, {Params: loose},
	})
	if err != nil {
		if report != nil {
			for i := range report.Incremental.States {
				a, b := report.Incremental.States[i], report.Full.States[i]
				if a.Stats != b.Stats {
					t.Logf("block %d incremental=%+v full=%+v", i, a.Stats, b.Stats)
					break
				}
			}
		}
		t.Fatalf("three-way verify: %v", err)
	}
	if !report.Equal {
		t.Fatalf("three-way mismatch: %+v", report.Comparison)
	}
	if len(report.Comparison.IncrementalCommit) != len(report.Comparison.ReferenceCommit) ||
		len(report.Comparison.ReferenceCommit) != len(report.Comparison.SerialCommit) {
		t.Fatal("final commit sequences have different lengths")
	}
}

func TestTamperedSetMonotonicWithTightening(t *testing.T) {
	img := testSample(t)
	thresholds := []float64{0.8, 0.6, 0.4, 0.2}
	previous := map[int]bool{}
	for i, threshold := range thresholds {
		p := DefaultParams()
		p.Threshold = threshold
		store, err := OpenStore(filepath.Join(t.TempDir(), "t.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		run := mustRun(t, NewEngine(img, store), p, RunOptions{})
		store.Close()
		current := map[int]bool{}
		for _, state := range run.States {
			if state.Tampered {
				current[state.BlockIndex] = true
			}
		}
		if i > 0 {
			for idx := range previous {
				if !current[idx] {
					t.Fatalf("tightening threshold %v removed block %d from tampered set", threshold, idx)
				}
			}
		}
		previous = current
	}
}

func TestWindowChangeReusesHashAndMatchesCold(t *testing.T) {
	img := testSample(t)
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "inc.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	engine := NewEngine(img, store)
	p := DefaultParams()
	first := mustRun(t, engine, p, RunOptions{})
	p.Window = 5
	second := mustRun(t, engine, p, RunOptions{})
	if second.Summary.ReusedBlocks != 0 {
		t.Fatalf("window change should recompute every analysis block, reused=%d", second.Summary.ReusedBlocks)
	}
	sawHashReuse := false
	for _, r := range second.Reuses {
		if r.Stage == StageHash {
			sawHashReuse = true
		}
	}
	if !sawHashReuse {
		t.Fatal("window change should reuse persisted hash results")
	}
	fullStore, err := OpenStore(filepath.Join(dir, "full.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer fullStore.Close()
	cold := mustRun(t, NewEngine(img, fullStore), p, RunOptions{})
	cmp, err := CompareRuns(second, cold, cold)
	if err != nil || !cmp.Equal {
		t.Fatalf("incremental and cold differ: %v %+v", err, cmp)
	}
	if second.Summary.TamperedBlocks < first.Summary.TamperedBlocks {
		t.Log("window change altered detection count as expected")
	}
}

func TestScopedRetryInvalidatesOnlySelectedBlocks(t *testing.T) {
	img := testSample(t)
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "inc.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	engine := NewEngine(img, store)
	p := DefaultParams()
	first := mustRun(t, engine, p, RunOptions{})
	second := mustRun(t, engine, p, RunOptions{ScopeBlocks: []int{3}})
	affected := map[int]bool{}
	for _, inv := range second.Invalidations {
		if inv.BlockIndex >= 0 {
			affected[inv.BlockIndex] = true
		}
	}
	if len(affected) != 1 || !affected[3] {
		t.Fatalf("unexpected affected blocks: %v", affected)
	}
	foundRetryReason := false
	for _, inv := range second.Invalidations {
		if inv.BlockIndex == 3 && inv.Stage == StageAnalysis && inv.Reason == "block_retry_requested" {
			foundRetryReason = true
		}
	}
	if !foundRetryReason {
		t.Fatalf("missing precise retry invalidation: %+v", second.Invalidations)
	}
	if second.HeatmapReused {
		t.Fatal("an invalidated downstream heatmap must not be reported as reused")
	}
	if second.Summary.ReusedBlocks != len(first.States)-1 {
		t.Fatalf("reused=%d want=%d", second.Summary.ReusedBlocks, len(first.States)-1)
	}
	cmp, err := CompareRuns(second, first, first)
	if err != nil || !cmp.Equal {
		t.Fatalf("retry changed deterministic result: %v %+v", err, cmp)
	}
}

func TestPauseResumeAndCancel(t *testing.T) {
	img := testSample(t)
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "pause.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctl := NewControl()
	ctl.Pause()
	done := make(chan error, 1)
	go func() {
		_, err := NewEngine(img, store).Run(context.Background(), DefaultParams(), RunOptions{}, ctl)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("paused run returned early: %v", err)
	case <-time.After(120 * time.Millisecond):
	}
	ctl.Resume()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("resumed run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resumed run timed out")
	}

	cancelStore, err := OpenStore(filepath.Join(dir, "cancel.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer cancelStore.Close()
	cancelCtl := NewControl()
	cancelCtl.Cancel()
	_, err = NewEngine(img, cancelStore).Run(context.Background(), DefaultParams(), RunOptions{}, cancelCtl)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("want ErrCancelled, got %v", err)
	}
}

func TestParallelAndSerialProduceSameResult(t *testing.T) {
	img := testSample(t)
	dir := t.TempDir()
	p := DefaultParams()
	p.WorkerCount = 8
	p.QueueCapacity = 2
	p.PixelBufferLimit = maxBlockPixelBytes(img, p.BlockSize)
	parallelStore, err := OpenStore(filepath.Join(dir, "parallel.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer parallelStore.Close()
	parallel := mustRun(t, NewEngine(img, parallelStore), p, RunOptions{})

	serial := p
	serial.WorkerCount = 1
	serial.QueueCapacity = 1
	serialStore, err := OpenStore(filepath.Join(dir, "serial.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer serialStore.Close()
	serialRun := mustRun(t, NewEngine(img, serialStore), serial, RunOptions{})
	cmp, err := CompareRuns(parallel, serialRun, serialRun)
	if err != nil || !cmp.Equal {
		t.Fatalf("parallel and serial differ: %v %+v", err, cmp)
	}
	if !equalInts(parallel.CommitLedger, serialRun.CommitLedger) {
		t.Fatalf("commit ledgers differ:\nparallel=%v\nserial=%v", parallel.CommitLedger, serialRun.CommitLedger)
	}
	if len(parallel.QueueEvents) == 0 {
		t.Fatal("expected bounded resource queue events")
	}
}

func TestDivergenceLocatesFirstPixelAndReportExport(t *testing.T) {
	img := testSample(t)
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "run.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := mustRun(t, NewEngine(img, store), DefaultParams(), RunOptions{})
	altered := run
	altered.HeatmapRGBA = append([]byte(nil), run.HeatmapRGBA...)
	pixel := 3
	altered.HeatmapRGBA[pixel*4] ^= 0xff
	cmp, err := CompareRuns(altered, run, run)
	if fe, ok := AsError(err); !ok || fe.Code != CodeDiverge || fe.Stage != StageHeatmap {
		t.Fatalf("want E_DIVERGE heatmap, got %v", err)
	}
	if cmp.FirstPixel == nil || cmp.FirstPixel.X != 3 || cmp.FirstPixel.Y != 0 || cmp.FirstPixel.BlockIndex != 0 {
		t.Fatalf("unexpected first pixel: %+v", cmp.FirstPixel)
	}
	files, err := WriteReport(filepath.Join(dir, "report"), img, &run, &cmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{files.ReportJSON, files.ReportCSV, files.HeatmapPNG, files.OriginalPNG} {
		if info, err := os.Stat(path); err != nil || info.Size() == 0 {
			t.Fatalf("report asset missing: %s (%v)", path, err)
		}
	}
	changed := run.States[0]
	changed.Stats.TamperScore += 0.01
	changed.Finalize()
	_, err = store.CommitBlock(context.Background(), changed, "retry")
	if fe, ok := AsError(err); !ok || fe.Code != CodeDiverge {
		t.Fatalf("store accepted a silent exact-result replacement: %v", err)
	}
}

func TestForcedTerminationResume(t *testing.T) {
	if os.Getenv("FORENSIC_KILL_HELPER") == "1" {
		killHelper(t)
		return
	}
	img := testSample(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "kill.sqlite")
	cmd := exec.Command(os.Args[0], "-test.run=^TestForcedTerminationResume$")
	cmd.Env = append(os.Environ(),
		"FORENSIC_KILL_HELPER=1",
		"FORENSIC_KILL_DB="+dbPath,
		"FORENSIC_KILL_MANIFEST="+img.ManifestPath,
		"FORENSIC_KILL_AFTER=5",
	)
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 77 {
		t.Fatalf("helper did not terminate abruptly: %v", err)
	}
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	snap, states, err := store.LoadSnapshot(context.Background(), img.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snap == nil || snap.Complete {
		t.Fatalf("unexpected killed snapshot: %+v", snap)
	}
	if len(states) == 0 || len(states) >= 70 {
		t.Fatalf("unexpected persisted block count %d", len(states))
	}
	persisted := len(states)
	resumed := mustRun(t, NewEngine(img, store), DefaultParams(), RunOptions{})
	if resumed.Summary.ReusedBlocks != persisted {
		t.Fatalf("resume reused=%d want persisted=%d", resumed.Summary.ReusedBlocks, persisted)
	}
	if len(resumed.States) != 70 {
		t.Fatalf("resumed states=%d", len(resumed.States))
	}
	store.Close()

	wrongStore, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer wrongStore.Close()
	p := DefaultParams()
	p.Window = 5
	run := mustRun(t, NewEngine(img, wrongStore), p, RunOptions{})
	if run.SnapshotResetReason == "" {
		t.Fatal("version mismatch must invalidate the whole snapshot")
	}
}

func TestCancelThenResumeOnlyUsesPersistedBlocks(t *testing.T) {
	img := testSample(t)
	dbPath := filepath.Join(t.TempDir(), "cancel-resume.sqlite")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ctl := NewControl()
	count := 0
	_, err = NewEngine(img, store).Run(context.Background(), DefaultParams(), RunOptions{
		CommitHook: func(BlockState) error {
			count++
			if count == 5 {
				ctl.Cancel()
			}
			return nil
		},
	}, ctl)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("want cancellation, got %v", err)
	}
	store.Close()

	resumeStore, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resumeStore.Close()
	_, states, err := resumeStore.LoadSnapshot(context.Background(), img.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 5 {
		t.Fatalf("persisted=%d want 5", len(states))
	}
	run := mustRun(t, NewEngine(img, resumeStore), DefaultParams(), RunOptions{})
	if run.Summary.ReusedBlocks != 5 {
		t.Fatalf("resume reused=%d want 5", run.Summary.ReusedBlocks)
	}
}

func killHelper(t *testing.T) {
	dbPath := os.Getenv("FORENSIC_KILL_DB")
	manifest := os.Getenv("FORENSIC_KILL_MANIFEST")
	limit, _ := strconv.Atoi(os.Getenv("FORENSIC_KILL_AFTER"))
	img, err := LoadSample(manifest, "game_tex_rgb_160x112")
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	_, _ = NewEngine(img, store).Run(context.Background(), DefaultParams(), RunOptions{
		CommitHook: func(BlockState) error {
			count++
			if count >= limit {
				os.Exit(77)
			}
			return nil
		},
	}, NewControl())
	t.Fatal("helper did not terminate")
}
