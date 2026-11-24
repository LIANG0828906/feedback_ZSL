package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"audioeval/internal/compare"
	"audioeval/internal/metrics"
	"audioeval/internal/model"
	"audioeval/internal/report"
	"audioeval/internal/sample"
	"audioeval/internal/verify"
)

func testParams() model.EvalParams {
	return model.EvalParams{
		Window:                 32,
		WindowHop:              8,
		BlocksPerFrame:         4,
		MaxWorkers:             1,
		PerThreadBufferSamples: 4096,
		QueueBudget:            0,
	}
}

func testSample(t *testing.T) sample.Loaded {
	t.Helper()
	dir := t.TempDir()
	if _, err := sample.Generate(sample.GenerateOptions{
		Dir:         dir,
		SampleID:    "engine-test",
		Seed:        12345,
		SampleRate:  48000,
		Channels:    2,
		FrameLength: 128,
		FrameHop:    64,
		FrameCount:  4,
		Quality:     model.QualityQ3,
	}); err != nil {
		t.Fatal(err)
	}
	loaded, err := sample.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func TestThreePathsBoundaryMergeAndIndependentVerification(t *testing.T) {
	loaded := testSample(t)
	params := testParams()
	full, err := Evaluate(context.Background(), loaded, RunOptions{Mode: ModeFull, RunDir: t.TempDir(), Params: params})
	if err != nil {
		t.Fatal(err)
	}
	parallelParams := params
	parallelParams.MaxWorkers = 4
	parallelParams.QueueBudget = 1
	parallel, err := Evaluate(context.Background(), loaded, RunOptions{Mode: ModeRealtime, RunDir: t.TempDir(), Params: parallelParams})
	if err != nil {
		t.Fatal(err)
	}
	if mismatch := compare.Reports(full.Report, parallel.Report); mismatch != nil {
		t.Fatal(mismatch)
	}

	verifyRunDir := t.TempDir()
	verifiedRun, err := Evaluate(context.Background(), loaded, RunOptions{Mode: ModeFull, RunDir: verifyRunDir, Params: params})
	if err != nil {
		t.Fatal(err)
	}
	verification, err := verify.Verify(verify.Options{SampleDir: loaded.Dir, RunDir: verifyRunDir})
	if err != nil {
		t.Fatal(err)
	}
	if !verification.OK {
		t.Fatalf("independent verification failed: %+v", verification.FirstViolation)
	}

	for frame := 0; frame < loaded.Manifest.FrameCount; frame++ {
		start := frame * loaded.Manifest.FrameHop * loaded.Manifest.Channels
		end := start + loaded.Manifest.FrameLength*loaded.Manifest.Channels
		whole := metrics.EvaluateWholeFrame(
			loaded.Original[start:end],
			loaded.Reconstruction[start:end],
			loaded.Manifest.Channels,
			params.Window,
			params.WindowHop,
		)
		merged := verifiedRun.Report.Frames[frame]
		if whole.PSNRUDb != merged.PSNRUDb || whole.SSIMQ16 != merged.SSIMQ16 ||
			whole.OriginalNoiseStdQ16 != merged.OriginalNoiseStdQ16 ||
			whole.ReconstructionNoiseStdQ16 != merged.ReconstructionNoiseStdQ16 {
			t.Fatalf("frame %d chunk merge differs from whole-frame evaluation: whole=%+v merged=%+v", frame, whole, merged)
		}
	}
}

func TestCancelThenResumeMatchesFullRun(t *testing.T) {
	loaded := testSample(t)
	params := testParams()
	full, err := Evaluate(context.Background(), loaded, RunOptions{Mode: ModeFull, RunDir: t.TempDir(), Params: params})
	if err != nil {
		t.Fatal(err)
	}
	resumeDir := t.TempDir()
	cancelParams := params
	cancelParams.MaxWorkers = 4
	cancelParams.QueueBudget = 1
	cancelParams.CancelAfterCommits = 3
	partial, err := Evaluate(context.Background(), loaded, RunOptions{Mode: ModeResume, RunDir: resumeDir, Params: cancelParams})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if partial.Report.Complete {
		t.Fatal("cancelled run must not claim completeness")
	}
	if _, err := os.Stat(filepath.Join(resumeDir, "report.json")); !os.IsNotExist(err) {
		t.Fatal("cancelled run must not publish a complete report")
	}
	if _, err := os.Stat(filepath.Join(resumeDir, "partial-report.json")); err != nil {
		t.Fatal("cancelled run must persist a partial report")
	}
	if partial.Report.Resume.CommittedBlocks <= 0 || partial.Report.Resume.CommittedBlocks >= partial.Report.Resume.TotalBlocks {
		t.Fatalf("unexpected partial commit count %d", partial.Report.Resume.CommittedBlocks)
	}

	resumeParams := cancelParams
	resumeParams.CancelAfterCommits = 0
	resumed, err := Evaluate(context.Background(), loaded, RunOptions{Mode: ModeResume, RunDir: resumeDir, Params: resumeParams})
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.Report.Resume.Resumed || resumed.Report.Resume.LoadedBlocks == 0 {
		t.Fatal("resume did not load persisted blocks")
	}
	if mismatch := compare.Reports(full.Report, resumed.Report); mismatch != nil {
		t.Fatal(mismatch)
	}
}

func TestSnapshotVersionMismatchInvalidatesWholeSnapshot(t *testing.T) {
	loaded := testSample(t)
	params := testParams()
	runDir := t.TempDir()
	first, err := Evaluate(context.Background(), loaded, RunOptions{Mode: ModeRealtime, RunDir: runDir, Params: params})
	if err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(runDir, "meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	var meta model.SnapshotMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	meta.ParamVersion = "stale-parameter-version"
	data, _ = json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(metaPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := Evaluate(context.Background(), loaded, RunOptions{Mode: ModeResume, RunDir: runDir, Params: params})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Report.Resume.Invalidated {
		t.Fatal("snapshot version mismatch did not invalidate the snapshot")
	}
	if mismatch := compare.Reports(first.Report, second.Report); mismatch != nil {
		t.Fatal(mismatch)
	}
}

func TestBandAndParameterErrors(t *testing.T) {
	loaded := testSample(t)
	bandParams := testParams()
	bandParams.BandWidth = 1
	_, err := Evaluate(context.Background(), loaded, RunOptions{Mode: ModeFull, RunDir: t.TempDir(), Params: bandParams})
	if model.CodeOf(err) != model.CodeBand {
		t.Fatalf("expected E_BAND, got %v", err)
	}
	var evalErr *model.EvalError
	if !errors.As(err, &evalErr) || !evalErr.HasBlock || evalErr.RequiredBand <= 0 {
		t.Fatalf("E_BAND must include block coordinate and required band: %+v", evalErr)
	}

	windowParams := testParams()
	windowParams.Window = 0
	_, err = Evaluate(context.Background(), loaded, RunOptions{Mode: ModeFull, RunDir: t.TempDir(), Params: windowParams})
	if model.CodeOf(err) != model.CodePara {
		t.Fatalf("expected E_PARA for window, got %v", err)
	}
	if !errors.As(err, &evalErr) || evalErr.Field != "window" {
		t.Fatalf("E_PARA must identify field window: %+v", evalErr)
	}

	bufferParams := testParams()
	bufferParams.PerThreadBufferSamples = 10
	_, err = Evaluate(context.Background(), loaded, RunOptions{Mode: ModeFull, RunDir: t.TempDir(), Params: bufferParams})
	if model.CodeOf(err) != model.CodePara {
		t.Fatalf("expected E_PARA for buffer budget, got %v", err)
	}
	if !errors.As(err, &evalErr) || evalErr.Field != "per_thread_buffer_samples" {
		t.Fatalf("E_PARA must identify per_thread_buffer_samples: %+v", evalErr)
	}
}

func TestSchedulerBlocksAtQueueBudgetWithoutDroppingTasks(t *testing.T) {
	const taskCount = 6
	started := make(chan int, taskCount)
	release := make(chan struct{})
	var active atomic.Int32
	var maxActive atomic.Int32
	tasks := make([]Task, taskCount)
	for sequence := 0; sequence < taskCount; sequence++ {
		sequence := sequence
		tasks[sequence] = Task{
			Sequence:        sequence,
			Coord:           model.BlockCoord{Frame: 0, Block: sequence},
			RequiredSamples: 1,
			Run: func(_ context.Context, _ *WorkerBuffer) (model.BlockResult, error) {
				current := active.Add(1)
				for {
					old := maxActive.Load()
					if current <= old || maxActive.CompareAndSwap(old, current) {
						break
					}
				}
				started <- sequence
				<-release
				active.Add(-1)
				return model.BlockResult{Sequence: sequence, Frame: 0, Block: sequence}, nil
			},
		}
	}
	scheduler := Scheduler{MaxWorkers: 2, PerThreadBufferSamples: 16, QueueBudget: 0}
	type outcome struct {
		summary SchedulerSummary
		err     error
	}
	done := make(chan outcome, 1)
	var committed []int
	go func() {
		summary, err := scheduler.Run(context.Background(), tasks, func(result model.BlockResult) error {
			committed = append(committed, result.Sequence)
			return nil
		})
		done <- outcome{summary: summary, err: err}
	}()
	<-started
	<-started
	time.Sleep(30 * time.Millisecond)
	close(release)
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	if len(result.summary.Committed) != taskCount {
		t.Fatalf("scheduler dropped tasks: committed %d/%d", len(result.summary.Committed), taskCount)
	}
	for i, sequence := range committed {
		if sequence != i {
			t.Fatalf("commit order is not stable: position %d has %d", i, sequence)
		}
	}
	if len(result.summary.QueueEvents) == 0 {
		t.Fatal("expected queue-budget wait events")
	}
	for _, event := range result.summary.QueueEvents {
		if event.Reason != "queue_budget_full" {
			t.Fatalf("unexpected queue reason %q", event.Reason)
		}
	}
	if maxActive.Load() > 2 {
		t.Fatalf("maximum parallelism exceeded: %d", maxActive.Load())
	}
}

func TestSchedulerRejectsOversizedWorkerBuffer(t *testing.T) {
	scheduler := Scheduler{MaxWorkers: 1, PerThreadBufferSamples: 4, QueueBudget: 1}
	tasks := []Task{{
		Sequence:        0,
		Coord:           model.BlockCoord{Frame: 0, Block: 0},
		RequiredSamples: 5,
		Run: func(context.Context, *WorkerBuffer) (model.BlockResult, error) {
			t.Fatal("oversized task must not run")
			return model.BlockResult{}, nil
		},
	}}
	_, err := scheduler.Run(context.Background(), tasks, func(model.BlockResult) error { return nil })
	if model.CodeOf(err) != model.CodePara {
		t.Fatalf("expected E_PARA for oversized buffer, got %v", err)
	}
}

func TestIndependentVerifierLocatesTamperedMetric(t *testing.T) {
	loaded := testSample(t)
	runDir := t.TempDir()
	result, err := Evaluate(context.Background(), loaded, RunOptions{Mode: ModeFull, RunDir: runDir, Params: testParams()})
	if err != nil {
		t.Fatal(err)
	}
	result.Report.Frames[1].PSNRUDb++
	if err := report.WriteJSON(filepath.Join(runDir, "report.json"), result.Report); err != nil {
		t.Fatal(err)
	}
	verification, err := verify.Verify(verify.Options{SampleDir: loaded.Dir, RunDir: runDir})
	if err != nil {
		t.Fatal(err)
	}
	if verification.OK || verification.FirstViolation == nil {
		t.Fatal("tampered metric was not detected")
	}
	if verification.FirstViolation.Frame != 1 || verification.FirstViolation.Field != "psnr_udb" {
		t.Fatalf("verifier did not locate the first tampered field: %+v", verification.FirstViolation)
	}
}

func TestQualityTierPSNRIsMonotonic(t *testing.T) {
	qualities := []model.QualityLevel{model.QualityQ1, model.QualityQ3, model.QualityQ5}
	means := make([]int64, len(qualities))
	for i, quality := range qualities {
		dir := t.TempDir()
		if _, err := sample.Generate(sample.GenerateOptions{
			Dir:         dir,
			SampleID:    "monotonic-" + string(quality),
			Seed:        987,
			SampleRate:  48000,
			Channels:    2,
			FrameLength: 128,
			FrameHop:    64,
			FrameCount:  4,
			Quality:     quality,
		}); err != nil {
			t.Fatal(err)
		}
		loaded, err := sample.Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		result, err := Evaluate(context.Background(), loaded, RunOptions{Mode: ModeFull, RunDir: t.TempDir(), Params: testParams()})
		if err != nil {
			t.Fatal(err)
		}
		for _, frame := range result.Report.Frames {
			means[i] += frame.PSNRUDb
		}
		means[i] /= int64(len(result.Report.Frames))
	}
	if !(means[0] < means[1] && means[1] < means[2]) {
		t.Fatalf("PSNR must improve monotonically as quality tightens: %v", means)
	}
}
