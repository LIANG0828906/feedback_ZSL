package optim_test

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"diffrender/errs"
	"diffrender/optim"
	"diffrender/render"
	"diffrender/sample"
	"diffrender/scene"
)

type fixture struct {
	task    *sample.Task
	initial *scene.Scene
	cfg     optim.Config
}

func newFixture(t *testing.T, seed int64) fixture {
	t.Helper()
	task, err := sample.GenerateTask(seed)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := scene.Load(task.InitialVersions[scene.Version1], "initial-1.0.json")
	if err != nil {
		t.Fatal(err)
	}
	cfg := optim.DefaultConfig()
	cfg.Iterations = 48
	cfg.WindowSize = 4
	cfg.Tolerance = 1e-12
	cfg.Render = renderConfig(4)
	return fixture{task: task, initial: initial, cfg: cfg}
}

func renderConfig(workers int) render.Config {
	return render.Config{MaxWorkers: workers, BlockPixels: 128, QueueCapacity: 2, Retries: 1}
}

func runFixture(t *testing.T, f fixture, dir string, cfg optim.Config, ctx context.Context) (*optim.RunResult, error) {
	t.Helper()
	runner, err := optim.NewRunner(f.initial, f.task.Targets, cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	return runner.Run(ctx)
}

func TestOptimizationConvergesMonotonically(t *testing.T) {
	f := newFixture(t, 301)
	result, err := runFixture(t, f, t.TempDir(), f.cfg, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Converged {
		t.Fatalf("not converged: loss=%g iterations=%d", result.State.Loss, result.State.Iteration)
	}
	if result.State.Loss > f.cfg.Tolerance {
		t.Fatalf("loss %g > tolerance %g", result.State.Loss, f.cfg.Tolerance)
	}
	if len(result.Losses) != result.State.Iteration+1 {
		t.Fatalf("loss sequence length=%d iteration=%d", len(result.Losses), result.State.Iteration)
	}
	for i := 1; i < len(result.Losses); i++ {
		if result.Losses[i] > result.Losses[i-1] {
			t.Fatalf("loss increases at %d: %g > %g", i, result.Losses[i], result.Losses[i-1])
		}
	}
	if len(result.Windows) < 2 {
		t.Fatalf("expected multiple committed windows, got %d", len(result.Windows))
	}
	for i, w := range result.Windows {
		if w.Index != i || len(w.ContentHash) != 64 {
			t.Fatalf("window %d=%+v", i, w)
		}
	}
	final := result.FinalScene
	truth := f.task.Truth
	if dist(final.Spheres[0].Center, truth.Spheres[0].Center) > 0.01 {
		t.Fatalf("center error too large: %+v vs %+v", final.Spheres[0].Center, truth.Spheres[0].Center)
	}
	if math.Abs(final.Spheres[0].Radius-truth.Spheres[0].Radius) > 0.01 {
		t.Fatalf("radius error: %g", final.Spheres[0].Radius-truth.Spheres[0].Radius)
	}
	if dist(final.Spheres[0].Albedo, truth.Spheres[0].Albedo) > 0.03 {
		t.Fatalf("albedo error: %+v vs %+v", final.Spheres[0].Albedo, truth.Spheres[0].Albedo)
	}
	if math.Abs(final.Lights[0].RadiantPowerW-truth.Lights[0].RadiantPowerW) > 0.03*truth.Lights[0].RadiantPowerW {
		t.Fatalf("light power error: %g vs %g", final.Lights[0].RadiantPowerW, truth.Lights[0].RadiantPowerW)
	}
	snap, err := optim.NewStore(t.TempDir(), optim.Metadata{}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Valid {
		t.Fatal("empty store unexpectedly valid")
	}
}

func TestThreePathsAndParallelAreBitIdentical(t *testing.T) {
	f := newFixture(t, 302)
	oneshot, err := runFixture(t, f, t.TempDir(), f.cfg, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	live, err := runFixture(t, f, t.TempDir(), f.cfg, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := optim.CompareResults(oneshot, live); err != nil {
		t.Fatalf("live: %v", err)
	}
	resumeDir := t.TempDir()
	pausedCfg := f.cfg
	pausedCfg.PauseAfterWindow = 1
	paused, err := runFixture(t, f, resumeDir, pausedCfg, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !paused.Paused || len(paused.Windows) != 2 {
		t.Fatalf("pause state: paused=%v windows=%d", paused.Paused, len(paused.Windows))
	}
	cancelled, err := runFixture(t, f, resumeDir, f.cfg, cancelledContext())
	if err == nil {
		t.Fatal("expected cancellation")
	}
	code, _ := errs.CodeOf(err)
	if code != errs.CodeCancel {
		t.Fatalf("expected E_CANCEL, got %v", err)
	}
	if len(cancelled.Windows) != 2 {
		t.Fatalf("cancel changed committed windows: %d", len(cancelled.Windows))
	}
	resumed, err := runFixture(t, f, resumeDir, f.cfg, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.Resumed {
		t.Fatal("expected resumed run")
	}
	if err := optim.CompareResults(oneshot, resumed); err != nil {
		t.Fatalf("resume: %v", err)
	}
	serialCfg := f.cfg
	serialCfg.Render = renderConfig(1)
	serial, err := runFixture(t, f, t.TempDir(), serialCfg, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := optim.CompareResults(oneshot, serial); err != nil {
		t.Fatalf("serial/parallel: %v", err)
	}
}

func TestCheckpointSnapshotIsImmutableAndCorruptionRecovers(t *testing.T) {
	f := newFixture(t, 303)
	dir := t.TempDir()
	full, err := runFixture(t, f, dir, f.cfg, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	runner, err := optim.NewRunner(f.initial, f.task.Targets, f.cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := runner.Store().Load()
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Valid || len(snap.Checkpoints) != len(full.Windows) {
		t.Fatalf("snapshot valid=%v windows=%d", snap.Valid, len(snap.Checkpoints))
	}
	firstCenter := snap.Checkpoints[0].Scene.Spheres[0].Center
	snap.Checkpoints[0].Scene.Spheres[0].Center.X = 999
	snap.Checkpoints[0].Parameters[0] = 999
	again, err := runner.Store().Load()
	if err != nil {
		t.Fatal(err)
	}
	if again.Checkpoints[0].Scene.Spheres[0].Center != firstCenter {
		t.Fatal("mutating returned checkpoint changed committed snapshot")
	}
	path := filepath.Join(dir, "window-000000.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	idx := bytes.Index(data, []byte(`"contentHash": "`))
	if idx < 0 {
		t.Fatal("content hash field not found")
	}
	idx += len(`"contentHash": "`)
	if data[idx] == '0' {
		data[idx] = '1'
	} else {
		data[idx] = '0'
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	invalid, err := runner.Store().Load()
	if err != nil {
		t.Fatal(err)
	}
	if !invalid.Invalidated || invalid.Valid {
		t.Fatalf("corrupt snapshot not invalidated: %+v", invalid)
	}
	recovered, err := runFixture(t, f, dir, f.cfg, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !recovered.Invalidated || recovered.Resumed {
		t.Fatalf("recovery state: invalidated=%v resumed=%v", recovered.Invalidated, recovered.Resumed)
	}
	if err := optim.CompareResults(full, recovered); err != nil {
		t.Fatalf("recovery mismatch: %v", err)
	}
	meta := runner.Metadata()
	meta.ConfigHash = "mismatch"
	bad, err := optim.NewStore(dir, meta).Load()
	if err != nil {
		t.Fatal(err)
	}
	if !bad.Invalidated {
		t.Fatal("metadata mismatch was not invalidated")
	}
}

func TestNonFiniteGradientRollsBackRound(t *testing.T) {
	f := newFixture(t, 304)
	cfg := f.cfg
	cfg.Iterations = 3
	cfg.WindowSize = 2
	cfg.GradientHook = func(iteration int, gradient []float64) error {
		if iteration == 0 {
			gradient[0] = math.NaN()
		}
		return nil
	}
	dir := t.TempDir()
	runner, err := optim.NewRunner(f.initial, f.task.Targets, cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background())
	if err == nil {
		t.Fatal("expected E_GRAD")
	}
	code, ok := errs.CodeOf(err)
	if !ok || code != errs.CodeGradient {
		t.Fatalf("expected E_GRAD, got %v", err)
	}
	e, _ := errs.As(err)
	if e.Field != "sphere_main.center.x" {
		t.Fatalf("gradient field=%q", e.Field)
	}
	snap, err := runner.Store().Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Windows) != 0 {
		t.Fatalf("E_GRAD left %d committed windows", len(snap.Windows))
	}
	if result.FinalScene.Spheres[0].Center != f.initial.Spheres[0].Center ||
		result.FinalScene.Spheres[0].Radius != f.initial.Spheres[0].Radius ||
		result.FinalScene.Spheres[0].Albedo != f.initial.Spheres[0].Albedo ||
		result.FinalScene.Planes[0].Albedo != f.initial.Planes[0].Albedo ||
		result.FinalScene.Lights[0].RadiantPowerW != f.initial.Lights[0].RadiantPowerW {
		t.Fatal("E_GRAD polluted scene parameters")
	}
}

func TestCancellationMidWindowLeavesOnlyCommittedSnapshot(t *testing.T) {
	f := newFixture(t, 305)
	full, err := runFixture(t, f, t.TempDir(), f.cfg, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cfg := f.cfg
	cfg.GradientHook = func(iteration int, gradient []float64) error {
		if iteration == 1 {
			cancel()
		}
		return nil
	}
	runner, err := optim.NewRunner(f.initial, f.task.Targets, cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(ctx)
	if err == nil {
		t.Fatal("expected cancellation")
	}
	code, _ := errs.CodeOf(err)
	if code != errs.CodeCancel {
		t.Fatalf("expected E_CANCEL, got %v", err)
	}
	snap, err := runner.Store().Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Windows) != 0 {
		t.Fatalf("mid-window cancellation committed %d windows", len(snap.Windows))
	}
	if len(result.Windows) != 0 {
		t.Fatal("cancelled result exposes uncommitted windows")
	}
	recovered, err := runFixture(t, f, dir, f.cfg, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := optim.CompareResults(full, recovered); err != nil {
		t.Fatalf("cancel recovery: %v", err)
	}
}

func TestDivergenceReportsFirstParameterAndWindow(t *testing.T) {
	f := newFixture(t, 306)
	full, err := runFixture(t, f, t.TempDir(), f.cfg, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	altered := *full
	altered.FinalScene = full.FinalScene.Clone()
	altered.FinalScene.Spheres[0].Center.X += 1e-9
	err = optim.CompareResults(full, &altered)
	if err == nil {
		t.Fatal("expected E_DIVERGE")
	}
	code, _ := errs.CodeOf(err)
	if code != errs.CodeDiverge {
		t.Fatalf("expected E_DIVERGE, got %v", err)
	}
	e, _ := errs.As(err)
	diff, ok := e.Details.(optim.Divergence)
	if !ok {
		t.Fatalf("details=%#v", e.Details)
	}
	if diff.Field != "sphere_main.center.x" {
		t.Fatalf("divergence field=%q", diff.Field)
	}
	if diff.Window != len(full.Windows)-1 {
		t.Fatalf("divergence window=%d want %d", diff.Window, len(full.Windows)-1)
	}
	if diff.Left == diff.Right {
		t.Fatal("divergence values are equal")
	}
}

func TestTargetCorruptionIsEDecodeAtOptimizerBoundary(t *testing.T) {
	f := newFixture(t, 307)
	targets := make([]render.Image, len(f.task.Targets))
	for i := range targets {
		targets[i] = f.task.Targets[i].Clone()
	}
	targets[0].Pixels[0] = math.NaN()
	_, err := optim.NewRunner(f.initial, targets, f.cfg, t.TempDir())
	code, ok := errs.CodeOf(err)
	if !ok || code != errs.CodeDecode {
		t.Fatalf("expected E_DECODE, got %v", err)
	}
}

func TestConcurrentWindowCommitIsSerialized(t *testing.T) {
	f := newFixture(t, 308)
	dir := t.TempDir()
	full, err := runFixture(t, f, dir, f.cfg, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	meta := optim.Metadata{
		SourceVersion: full.SourceVersion, AlgorithmVersion: full.AlgorithmVersion,
		TaskHash: full.TaskHash, ConfigHash: full.ConfigHash,
	}
	store := optim.NewStore(t.TempDir(), meta)
	cp := full.Checkpoints[0]
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- store.Commit(cp)
		}()
	}
	wg.Wait()
	close(results)
	var successes, failures int
	for err := range results {
		if err == nil {
			successes++
		} else {
			failures++
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("concurrent commits: successes=%d failures=%d", successes, failures)
	}
	snap, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Valid || len(snap.Windows) != 1 {
		t.Fatalf("serialized snapshot valid=%v windows=%d", snap.Valid, len(snap.Windows))
	}
}

func cancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func dist(a, b scene.Vec3) float64 {
	d := a.Sub(b)
	return math.Sqrt(d.X*d.X + d.Y*d.Y + d.Z*d.Z)
}
