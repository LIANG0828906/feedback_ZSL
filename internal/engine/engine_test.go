package engine_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"

	"retargetlab/internal/engine"
	"retargetlab/internal/fsm"
	"retargetlab/internal/generator"
	"retargetlab/internal/geom"
	"retargetlab/internal/model"
	"retargetlab/internal/solver"
	"retargetlab/internal/store"
)

func testWorkspace(t *testing.T) model.Workspace {
	t.Helper()
	ws, err := generator.Generate(generator.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func testEngine(t *testing.T, ws model.Workspace, workers int) *engine.Engine {
	t.Helper()
	target, ok := ws.TargetByName("reference")
	if !ok {
		t.Fatal("reference target missing")
	}
	e, err := engine.New(engine.Config{
		Source: ws.Source, Clip: ws.Clip, Target: target, Mapping: ws.Mapping,
		Params: solver.DefaultParams(), MaxWorkers: workers, WindowSize: 4,
		QueuePerWorker: 2, BufferPerWorker: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func findRule(t *testing.T, m model.Mapping, target string) int {
	t.Helper()
	for i, r := range m.Rules {
		for _, name := range r.Targets {
			if name == target {
				return i
			}
		}
	}
	t.Fatalf("mapping rule for target %q not found", target)
	return -1
}

func TestIncrementalEditMatchesFullRecompute(t *testing.T) {
	ws := testWorkspace(t)
	e := testEngine(t, ws, 4)
	rule := findRule(t, ws.Mapping, "upperarm_l")
	replacement := ws.Mapping.Rules[rule]
	replacement.Source = "forearm_l"
	report, err := e.ApplyMappingRule(rule, replacement)
	if err != nil {
		t.Fatal(err)
	}
	if report.Frames == 0 {
		t.Fatal("expected affected frames after mapping edit")
	}
	if _, err := e.SolveAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CompareWithFull(context.Background()); err != nil {
		t.Fatalf("incremental and full recompute differ: %v", err)
	}
}

func TestPhaseScaleEditReusesCommittedCache(t *testing.T) {
	ctx := context.Background()
	ws := testWorkspace(t)
	e := testEngine(t, ws, 4)
	if _, err := e.SolveRange(ctx, 0, 23); err != nil {
		t.Fatal(err)
	}
	report, err := e.SetScale("gesture", 1.08)
	if err != nil {
		t.Fatal(err)
	}
	if report.Frames != 8 {
		t.Fatalf("expected 8 invalidated gesture frames, got %d", report.Frames)
	}
	for _, entry := range report.Entries {
		if entry.Frame < 24 || entry.Frame > 31 {
			t.Fatalf("unexpected invalidated frame %d", entry.Frame)
		}
	}
	run, err := e.SolveAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if run.CacheHits != 24 || run.CacheMisses != 8 {
		t.Fatalf("cache stats mismatch: hits=%d misses=%d", run.CacheHits, run.CacheMisses)
	}
	compare, err := e.CompareWithFull(ctx)
	if err != nil {
		t.Fatalf("active-config incremental/full comparison failed: %v", err)
	}
	if compare.FramesCompared != 8 || len(compare.FrozenSkipped) != 24 {
		t.Fatalf("expected 8 active comparisons and 24 frozen skips, got %d/%d", compare.FramesCompared, len(compare.FrozenSkipped))
	}
	snap := e.StateSnapshot()
	if snap.CommittedFrames != 32 {
		t.Fatalf("expected all frames committed, got %d", snap.CommittedFrames)
	}
	for _, frame := range snap.Frames {
		if frame.Frame < 24 && frame.MappingHash == "" {
			t.Fatalf("committed frame %d lost its frozen snapshot", frame.Frame)
		}
	}

	full := testEngine(t, ws, 1)
	if _, err := full.SetScale("gesture", 1.08); err != nil {
		t.Fatal(err)
	}
	if _, err := full.SolveRange(ctx, 24, 31); err != nil {
		t.Fatal(err)
	}
	incremental := e.CommittedResults()
	recomputed := full.CommittedResults()
	for frame := 24; frame < 32; frame++ {
		if err := solver.CompareFrameResults(incremental[frame], recomputed[frame]); err != nil {
			t.Fatalf("frame %d differs: %v", frame, err)
		}
	}
}

func TestMappingEditInvalidatesOnlyDependentFrames(t *testing.T) {
	ctx := context.Background()
	ws := testWorkspace(t)
	for i := range ws.Clip.Frames {
		for j := range ws.Clip.Frames[i].LocalRotations {
			ws.Clip.Frames[i].LocalRotations[j] = geom.IdentityQuat()
		}
	}
	upper := ws.Source.JointIndex("upperarm_l")
	fore := ws.Source.JointIndex("forearm_l")
	for frame := 24; frame < 32; frame++ {
		ws.Clip.Frames[frame].LocalRotations[upper] = geom.QuatFromAxisAngle(geom.Vec3{X: 1}, 0.2)
		ws.Clip.Frames[frame].LocalRotations[fore] = geom.QuatFromAxisAngle(geom.Vec3{Y: 1}, 0.3)
	}
	e := testEngine(t, ws, 4)
	if _, err := e.SolveRange(ctx, 0, 15); err != nil {
		t.Fatal(err)
	}
	rule := findRule(t, ws.Mapping, "upperarm_l")
	replacement := ws.Mapping.Rules[rule]
	replacement.Source = "forearm_l"
	report, err := e.ApplyMappingRule(rule, replacement)
	if err != nil {
		t.Fatal(err)
	}
	if report.Frames != 8 {
		t.Fatalf("expected 8 dependent frames, got %d: %+v", report.Frames, report.Entries)
	}
	for _, inv := range report.Entries {
		if inv.Frame < 24 || inv.Frame > 31 {
			t.Fatalf("unexpected invalidated frame %d", inv.Frame)
		}
	}
}

func TestIllegalFrameTransitionLeavesStateUnchanged(t *testing.T) {
	ws := testWorkspace(t)
	e := testEngine(t, ws, 1)
	if err := e.Transition(0, fsm.Committed); !model.IsCode(err, model.E_STATE) {
		t.Fatalf("expected E_STATE, got %v", err)
	}
	state, err := e.State(0)
	if err != nil {
		t.Fatal(err)
	}
	if state != fsm.Pending {
		t.Fatalf("state changed after illegal transition: %s", state)
	}
}

func TestFailureIsolatedAndRetryable(t *testing.T) {
	ctx := context.Background()
	ws := testWorkspace(t)
	target, _ := ws.TargetByName("reference")
	var fail atomic.Bool
	fail.Store(true)
	e, err := engine.New(engine.Config{
		Source: ws.Source, Clip: ws.Clip, Target: target, Mapping: ws.Mapping,
		Params: solver.DefaultParams(), MaxWorkers: 4, WindowSize: 4,
		QueuePerWorker: 2, BufferPerWorker: 2,
		FrameFault: func(frame int) error {
			if frame == 5 && fail.Load() {
				return errors.New("injected solver fault")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.SolveAll(ctx); err != nil {
		t.Fatal(err)
	}
	state, _ := e.State(5)
	if state != fsm.Failed {
		t.Fatalf("frame 5 state = %s", state)
	}
	for _, frame := range []int{0, 1, 2, 3, 4, 6, 7} {
		state, _ := e.State(frame)
		if state != fsm.Committed {
			t.Fatalf("frame %d was polluted, state=%s", frame, state)
		}
	}
	fail.Store(false)
	if _, err := e.RetryFrame(ctx, 5); err != nil {
		t.Fatal(err)
	}
	if state, _ := e.State(5); state != fsm.Committed {
		t.Fatalf("retried frame state = %s", state)
	}
	if _, err := e.CompareWithFull(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestParallelAndSerialEditsProduceIdenticalResults(t *testing.T) {
	ctx := context.Background()
	ws := testWorkspace(t)
	target, _ := ws.TargetByName("reference")
	newStored := func(path string, workers int) *engine.Engine {
		db, err := store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		e, err := engine.New(engine.Config{
			Source: ws.Source, Clip: ws.Clip, Target: target, Mapping: ws.Mapping,
			Params: solver.DefaultParams(), Store: db, WindowSize: 4, MaxWorkers: workers,
			QueuePerWorker: 2, BufferPerWorker: 2,
		})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	prepare := func(e *engine.Engine) {
		rule := findRule(t, ws.Mapping, "upperarm_r")
		replacement := ws.Mapping.Rules[rule]
		replacement.Source = "forearm_r"
		if _, err := e.ApplyMappingRule(rule, replacement); err != nil {
			t.Fatal(err)
		}
		if _, err := e.SetScale("stride", 1.03); err != nil {
			t.Fatal(err)
		}
	}
	serial := newStored(t.TempDir()+"/serial.db", 1)
	parallel := newStored(t.TempDir()+"/parallel.db", 4)
	prepare(serial)
	prepare(parallel)
	if _, err := serial.SolveAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := parallel.SolveAll(ctx); err != nil {
		t.Fatal(err)
	}
	serialResults := serial.CommittedResults()
	parallelResults := parallel.CommittedResults()
	for frame := range ws.Clip.Frames {
		if err := solver.CompareFrameResults(serialResults[frame], parallelResults[frame]); err != nil {
			t.Fatalf("frame %d differs between serial and parallel: %v", frame, err)
		}
	}
	ss := serial.StateSnapshot()
	ps := parallel.StateSnapshot()
	if len(ss.Windows) != len(ps.Windows) {
		t.Fatalf("window count differs: %d vs %d", len(ss.Windows), len(ps.Windows))
	}
	for i := range ss.Windows {
		if ss.Windows[i].StartFrame != ps.Windows[i].StartFrame ||
			ss.Windows[i].EndFrame != ps.Windows[i].EndFrame ||
			ss.Windows[i].ContentHash != ps.Windows[i].ContentHash ||
			ss.Windows[i].CommitSeq != ps.Windows[i].CommitSeq {
			t.Fatalf("window %d commit sequence differs", i)
		}
	}
}

func TestRecoveryLoadsOnlyMatchingVersionWindows(t *testing.T) {
	ctx := context.Background()
	ws := testWorkspace(t)
	target, _ := ws.TargetByName("reference")
	dbPath := t.TempDir() + "/checkpoints.db"
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := engine.Config{
		Source: ws.Source, Clip: ws.Clip, Target: target, Mapping: ws.Mapping,
		Params: solver.DefaultParams(), Store: db, MaxWorkers: 3, WindowSize: 4,
		QueuePerWorker: 2, BufferPerWorker: 2,
	}
	first, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.SolveRange(ctx, 0, 15); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	cfg.Store = db2
	second, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	report, err := second.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.LoadedFrames != 16 || report.Invalidated {
		t.Fatalf("unexpected recovery report: %+v", report)
	}
	for i := 0; i < 16; i++ {
		if state, _ := second.State(i); state != fsm.Committed {
			t.Fatalf("frame %d not recovered: %s", i, state)
		}
	}
	if state, _ := second.State(16); state != fsm.Pending {
		t.Fatalf("uncommitted frame 16 should be pending, got %s", state)
	}
	if _, err := second.SolveAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := second.CompareWithFull(ctx); err != nil {
		t.Fatal(err)
	}

	// A version mismatch invalidates all windows, never a prefix of them.
	badParams := solver.DefaultParams()
	badParams.ParamVersion = 99
	cfg.Params = badParams
	db3, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db3.Close()
	cfg.Store = db3
	third, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	badReport, err := third.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !badReport.Invalidated || badReport.LoadedFrames != 0 {
		t.Fatalf("version mismatch did not invalidate whole snapshot: %+v", badReport)
	}
	count, err := db3.Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected all checkpoint rows to be atomically deleted, got %d", count)
	}
	for i := range ws.Clip.Frames {
		if state, _ := third.State(i); state != fsm.Pending {
			t.Fatalf("frame %d should be pending after invalidation, got %s", i, state)
		}
	}
	if _, err := third.SolveAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := third.CompareWithFull(ctx); err != nil {
		t.Fatalf("post-invalidation full recompute differs: %v", err)
	}
}

func TestForcedProcessExitRestoresOnlyCommittedWindows(t *testing.T) {
	if os.Getenv("RETARGETLAB_KILL_CHILD") == "1" {
		ws, err := generator.Generate(generator.DefaultOptions())
		if err != nil {
			os.Exit(2)
		}
		target, _ := ws.TargetByName("reference")
		db, err := store.Open(os.Getenv("RETARGETLAB_KILL_DB"))
		if err != nil {
			os.Exit(3)
		}
		e, err := engine.New(engine.Config{
			Source: ws.Source, Clip: ws.Clip, Target: target, Mapping: ws.Mapping,
			Params: solver.DefaultParams(), Store: db, WindowSize: 4, MaxWorkers: 2,
			QueuePerWorker: 2, BufferPerWorker: 2,
		})
		if err != nil {
			os.Exit(4)
		}
		if _, err := e.SolveRange(context.Background(), 0, 11); err != nil {
			os.Exit(5)
		}
		// No Close and no further cleanup: emulate a forced process termination
		// after twelve frames were durably committed.
		os.Exit(23)
	}

	ctx := context.Background()
	dbPath := t.TempDir() + "/kill-recovery.db"
	cmd := exec.Command(os.Args[0], "-test.run=^TestForcedProcessExitRestoresOnlyCommittedWindows$")
	cmd.Env = append(os.Environ(),
		"RETARGETLAB_KILL_CHILD=1",
		"RETARGETLAB_KILL_DB="+dbPath,
	)
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 {
		t.Fatalf("helper did not terminate with code 23: %v", err)
	}

	ws, err := generator.Generate(generator.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	target, _ := ws.TargetByName("reference")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e, err := engine.New(engine.Config{
		Source: ws.Source, Clip: ws.Clip, Target: target, Mapping: ws.Mapping,
		Params: solver.DefaultParams(), Store: db, WindowSize: 4, MaxWorkers: 4,
		QueuePerWorker: 2, BufferPerWorker: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := e.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.LoadedFrames != 12 || report.LoadedWindows != 3 {
		t.Fatalf("unexpected recovered prefix: %+v", report)
	}
	for frame := 0; frame < 12; frame++ {
		if state, _ := e.State(frame); state != fsm.Committed {
			t.Fatalf("persisted frame %d not restored: %s", frame, state)
		}
	}
	for frame := 12; frame < len(ws.Clip.Frames); frame++ {
		if state, _ := e.State(frame); state != fsm.Pending {
			t.Fatalf("uncommitted frame %d should be pending: %s", frame, state)
		}
	}
	if _, err := e.SolveAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CompareWithFull(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryRejectsSkeletonHashMismatch(t *testing.T) {
	ctx := context.Background()
	ws := testWorkspace(t)
	target, _ := ws.TargetByName("reference")
	dbPath := t.TempDir() + "/skeleton-version.db"
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := engine.Config{
		Source: ws.Source, Clip: ws.Clip, Target: target, Mapping: ws.Mapping,
		Params: solver.DefaultParams(), Store: db, WindowSize: 4, MaxWorkers: 2,
	}
	e, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.SolveRange(ctx, 0, 7); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	target.Skeleton.Joints[1].LocalOffset.Y += 0.001
	db2, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	cfg.Store = db2
	cfg.Target = target
	changed, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	report, err := changed.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Invalidated || report.LoadedFrames != 0 {
		t.Fatalf("skeleton mismatch did not invalidate snapshot: %+v", report)
	}
	if count, _ := db2.Count(ctx); count != 0 {
		t.Fatalf("stale windows survived skeleton invalidation: %d", count)
	}
}
