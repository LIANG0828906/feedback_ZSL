package session_test

import (
	"errors"
	"sync/atomic"
	"testing"

	"qkernel/internal/chain"
	"qkernel/internal/errs"
	"qkernel/internal/imagex"
	"qkernel/internal/scenario"
	"qkernel/internal/session"
	"qkernel/internal/synth"
	"qkernel/internal/verifier"
)

func TestSegmentSizeAndParallelismDoNotChangePixels(t *testing.T) {
	cfg := synth.Config{Width: 128, Height: 128, Frames: 1, Seed: 818, Channels: 3}
	set, err := synth.GenerateSet(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b := set.Benchmarks[0]
	c := chain.Chain{Steps: []chain.Step{
		{Type: chain.NoiseStep, Strength: 0.5, Region: chain.WholeRegion()},
		{Type: chain.BlurStep, Strength: 0.6, Region: chain.WholeRegion()},
		{Type: chain.BlockStep, Strength: 0.7, Region: chain.WholeRegion()},
		{Type: chain.ChromaStep, Strength: 0.65, Region: chain.WholeRegion()},
		{Type: chain.OverexposeStep, Strength: 0.45, Region: chain.WholeRegion()},
	}}

	root32 := t.TempDir()
	root64 := t.TempDir()
	saveBenchmark(t, root32, cfg, b)
	saveBenchmark(t, root64, cfg, b)
	eng32, err := session.NewEngine(root32)
	if err != nil {
		t.Fatal(err)
	}
	r32, err := eng32.RunFrame(session.RunOptions{Benchmark: b, Chain: c, ParamVersion: 1, SegmentSize: 32, MetricBlockSize: 32, Workers: 4})
	if err != nil && !errs.IsCode(err, errs.EAttr) {
		t.Fatal(err)
	}
	if r32 == nil {
		t.Fatal("missing 32px segment result")
	}
	_ = eng32.Close()
	eng64, err := session.NewEngine(root64)
	if err != nil {
		t.Fatal(err)
	}
	r64, err := eng64.RunFrame(session.RunOptions{Benchmark: b, Chain: c, ParamVersion: 1, SegmentSize: 64, MetricBlockSize: 32, Workers: 1})
	if err != nil && !errs.IsCode(err, errs.EAttr) {
		t.Fatal(err)
	}
	if r64 == nil {
		t.Fatal("missing 64px segment result")
	}
	_ = eng64.Close()
	if r32.Final.Hash() != r64.Final.Hash() {
		t.Fatalf("segment/parallel mismatch: 32/w4=%s 64/w1=%s", r32.Final.Hash(), r64.Final.Hash())
	}
}

func TestUnitFailureRetryAndStateIsolation(t *testing.T) {
	cfg := synth.Config{Width: 128, Height: 128, Frames: 1, Seed: 919, Channels: 3}
	set, err := synth.GenerateSet(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b := set.Benchmarks[0]
	root := t.TempDir()
	saveBenchmark(t, root, cfg, b)
	eng, err := session.NewEngine(root)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	c := chain.Chain{Steps: []chain.Step{{Type: chain.NoiseStep, Strength: 0.5, Region: chain.WholeRegion()}}}
	var attempts int32
	opts := session.RunOptions{
		Benchmark: b, Chain: c, ParamVersion: 1, SegmentSize: 64, MetricBlockSize: 32, Workers: 2,
		FailureHook: func(stepNo, segmentIndex int) error {
			if stepNo == 1 && segmentIndex == 1 && atomic.AddInt32(&attempts, 1) == 1 {
				return errors.New("injected unit failure")
			}
			return nil
		},
	}
	failedRes, err := eng.RunFrame(opts)
	if err == nil {
		t.Fatal("expected injected failure")
	}
	states, err := eng.Store.ListUnitStates(opts.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if failedRes == nil {
		t.Fatal("failed run should return partial result")
	}
	runID := failedRes.RunID
	states, err = eng.Store.ListUnitStates(runID)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[[2]int]session.UnitStateRecord{}
	for _, st := range states {
		byKey[[2]int{st.StepIndex, st.SegmentIndex}] = st
	}
	if byKey[[2]int{1, 0}].State != session.StateCommitted {
		t.Fatalf("segment 0 should remain committed: %+v", byKey[[2]int{1, 0}])
	}
	if byKey[[2]int{1, 1}].State != session.StateFailed {
		t.Fatalf("segment 1 should be failed: %+v", byKey[[2]int{1, 1}])
	}
	opts.RunID = runID
	retry, err := eng.RunFrame(opts)
	if err != nil {
		t.Fatalf("retry should recover: %v", err)
	}
	rep, err := verifier.VerifyRoot(root, retry.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK {
		t.Fatalf("verifier rejected retried run: %+v", rep.Violations)
	}
}

func newScenarioBenchmark(t *testing.T) (*synth.Config, *chain.Chain, *synth.Benchmark) {
	t.Helper()
	cfg, c := scenario.Default()
	cfg.Frames = 1
	set, err := synth.GenerateSet(cfg)
	if err != nil {
		t.Fatalf("generate set: %v", err)
	}
	return &cfg, &c, set.Benchmarks[0]
}

func saveBenchmark(t *testing.T, root string, cfg synth.Config, b *synth.Benchmark) {
	t.Helper()
	set := &synth.Set{Benchmarks: []*synth.Benchmark{b}}
	if err := scenario.SaveSet(root, set); err != nil {
		t.Fatalf("save scenario: %v", err)
	}
}

func runEngine(t *testing.T, root string, b *synth.Benchmark, c chain.Chain, version, workers, interrupt int) (*session.RunResult, *session.Engine) {
	t.Helper()
	eng, err := session.NewEngine(root)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	res, err := eng.RunFrame(session.RunOptions{
		Benchmark:             b,
		Chain:                 c,
		ParamVersion:          version,
		SegmentSize:           scenario.SegmentSize,
		MetricBlockSize:       scenario.MetricBlockSize,
		Workers:               workers,
		InterruptAfterCommits: interrupt,
	})
	if interrupt > 0 {
		if !errors.Is(err, session.ErrInterrupted) {
			t.Fatalf("expected interruption, got %v", err)
		}
		_ = eng.Close()
		return res, nil
	}
	if err != nil {
		t.Fatalf("run frame: %v", err)
	}
	return res, eng
}

func TestDefaultScenarioThreePathsAndVerify(t *testing.T) {
	cfg, c, b := newScenarioBenchmark(t)
	realRoot := t.TempDir()
	fullRoot := t.TempDir()
	resumeRoot := t.TempDir()
	saveBenchmark(t, realRoot, *cfg, b)
	saveBenchmark(t, fullRoot, *cfg, b)
	saveBenchmark(t, resumeRoot, *cfg, b)

	realRes, eng := runEngine(t, realRoot, b, *c, 1, 4, 0)
	_ = eng.Close()
	fullRes, eng := runEngine(t, fullRoot, b, *c, 1, 1, 0)
	_ = eng.Close()

	interrupted, _ := runEngine(t, resumeRoot, b, *c, 1, 4, 7)
	if interrupted == nil {
		t.Fatal("interrupted result should carry run id")
	}
	eng2, err := session.NewEngine(resumeRoot)
	if err != nil {
		t.Fatal(err)
	}
	resumeRes, err := eng2.RunFrame(session.RunOptions{
		Benchmark: b, Chain: *c, ParamVersion: 1,
		SegmentSize: scenario.SegmentSize, MetricBlockSize: scenario.MetricBlockSize, Workers: 2,
	})
	if err != nil {
		t.Fatalf("resume run: %v", err)
	}
	_ = eng2.Close()

	if realRes.Final.Hash() != fullRes.Final.Hash() || realRes.Final.Hash() != resumeRes.Final.Hash() {
		t.Fatalf("final hashes differ real=%s full=%s resume=%s", realRes.Final.Hash(), fullRes.Final.Hash(), resumeRes.Final.Hash())
	}
	if len(realRes.Commits) != len(fullRes.Commits) || len(realRes.Commits) != len(resumeRes.Commits) {
		t.Fatalf("commit counts real=%d full=%d resume=%d", len(realRes.Commits), len(fullRes.Commits), len(resumeRes.Commits))
	}

	cmp, err := verifier.CompareRoots(realRes.RunID, realRoot, fullRoot, resumeRoot)
	if err != nil {
		t.Fatalf("compare roots: %v", err)
	}
	if !cmp.OK {
		t.Fatalf("three-path verification failed: %+v reports=%+v", cmp.FirstViolation, cmp.Reports[0].Violations)
	}
}

func TestPreciseInvalidationAndRollback(t *testing.T) {
	cfg := synth.Config{Width: 128, Height: 128, Frames: 1, Seed: 77, Channels: 3, IncludeLowContrast: false}
	set, err := synth.GenerateSet(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b := set.Benchmarks[0]
	root := t.TempDir()
	saveBenchmark(t, root, cfg, b)
	eng, err := session.NewEngine(root)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	noiseRegion := chain.RectRegion(imagex.R(0, 0, 32, 32))
	blurRegion := chain.RectRegion(imagex.R(96, 96, 128, 128))
	c1 := chain.Chain{Steps: []chain.Step{
		{Type: chain.NoiseStep, Strength: 0.45, Region: noiseRegion},
		{Type: chain.BlurStep, Strength: 0.65, Region: blurRegion},
	}}
	r1, err := eng.RunFrame(session.RunOptions{Benchmark: b, Chain: c1, ParamVersion: 1, SegmentSize: 32, MetricBlockSize: 32, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}

	c2 := c1
	c2.Steps = append([]chain.Step(nil), c1.Steps...)
	c2.Steps[0].Strength = 0.9
	r2, err := eng.RunFrame(session.RunOptions{Benchmark: b, Chain: c2, ParamVersion: 2, SegmentSize: 32, MetricBlockSize: 32, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	if r2.Reuse[0].RecomputedUnits != 1 || r2.Reuse[0].DependencyReuseUnits < 15 {
		t.Fatalf("changed step did not invalidate precisely: %+v", r2.Reuse[0])
	}
	if r2.Reuse[1].RecomputedUnits != 0 || r2.Reuse[1].Status != "reused" {
		t.Fatalf("unaffected downstream step was recomputed: %+v", r2.Reuse[1])
	}

	rb1, err := eng.RunFrame(session.RunOptions{Benchmark: b, Chain: c1, ParamVersion: 1, SegmentSize: 32, MetricBlockSize: 32, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	if rb1.Final.Hash() != r1.Final.Hash() {
		t.Fatal("rollback to version 1 changed final pixels")
	}
	rb2, err := eng.RunFrame(session.RunOptions{Benchmark: b, Chain: c2, ParamVersion: 2, SegmentSize: 32, MetricBlockSize: 32, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	if rb2.Final.Hash() != r2.Final.Hash() {
		t.Fatal("forward redo to version 2 changed final pixels")
	}

	directRoot := t.TempDir()
	saveBenchmark(t, directRoot, cfg, b)
	directEng, err := session.NewEngine(directRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer directEng.Close()
	direct, err := directEng.RunFrame(session.RunOptions{Benchmark: b, Chain: c2, ParamVersion: 99, SegmentSize: 32, MetricBlockSize: 32, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	if rb2.Final.Hash() != direct.Final.Hash() {
		t.Fatalf("history forward pixel %s != direct final %s", rb2.Final.Hash(), direct.Final.Hash())
	}
}

func TestStateMachineIllegalTransition(t *testing.T) {
	root := t.TempDir()
	st, err := session.OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	err = st.TransitionState("state-run", 0, 1, session.StateCommitted, "bad")
	if !errs.IsCode(err, errs.EState) {
		t.Fatalf("expected E_STATE, got %v", err)
	}
	states, err := st.ListUnitStates("state-run")
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 0 {
		t.Fatalf("illegal transition mutated state: %+v", states)
	}
	if err := st.TransitionState("state-run", 0, 1, session.StateProcessing, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.TransitionState("state-run", 0, 1, session.StateCommitted, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.TransitionState("state-run", 0, 1, session.StateProcessing, ""); !errs.IsCode(err, errs.EState) {
		t.Fatalf("committed unit should be terminal, got %v", err)
	}
}

func TestOrderChangeReusesSpatiallyUnaffectedStep(t *testing.T) {
	cfg := synth.Config{Width: 128, Height: 128, Frames: 1, Seed: 31337, Channels: 3}
	set, err := synth.GenerateSet(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b := set.Benchmarks[0]
	root := t.TempDir()
	saveBenchmark(t, root, cfg, b)
	eng, err := session.NewEngine(root)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	a := chain.Step{Type: chain.NoiseStep, Strength: 0.5, Region: chain.RectRegion(imagex.R(0, 0, 32, 32))}
	bb := chain.Step{Type: chain.BlurStep, Strength: 0.6, Region: chain.RectRegion(imagex.R(96, 0, 128, 32))}
	cstep := chain.Step{Type: chain.BlockStep, Strength: 0.7, Region: chain.RectRegion(imagex.R(0, 96, 32, 128))}
	c1 := chain.Chain{Steps: []chain.Step{a, bb, cstep}}
	r1, err := eng.RunFrame(session.RunOptions{Benchmark: b, Chain: c1, ParamVersion: 1, SegmentSize: 32, MetricBlockSize: 32, Workers: 2})
	if err != nil && !errs.IsCode(err, errs.EAttr) {
		t.Fatal(err)
	}

	c2 := chain.Chain{Steps: []chain.Step{bb, a, cstep}}
	r2, err := eng.RunFrame(session.RunOptions{Benchmark: b, Chain: c2, ParamVersion: 2, SegmentSize: 32, MetricBlockSize: 32, Workers: 2})
	if err != nil && !errs.IsCode(err, errs.EAttr) {
		t.Fatal(err)
	}
	if r2.Reuse[2].RecomputedUnits != 0 || r2.Reuse[2].Status != "reused" {
		t.Fatalf("third step should reuse after disjoint reorder: %+v", r2.Reuse[2])
	}
	if r1.Final.Hash() != r2.Final.Hash() {
		t.Fatal("disjoint region reorder should preserve final pixels")
	}
}

func TestRegionChangeDoesNotInvalidateFarOperatorDomain(t *testing.T) {
	cfg := synth.Config{Width: 128, Height: 128, Frames: 1, Seed: 4242, Channels: 3}
	set, err := synth.GenerateSet(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b := set.Benchmarks[0]
	root := t.TempDir()
	saveBenchmark(t, root, cfg, b)
	eng, err := session.NewEngine(root)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	c1 := chain.Chain{Steps: []chain.Step{
		{Type: chain.NoiseStep, Strength: 0.5, Region: chain.RectRegion(imagex.R(0, 0, 32, 32))},
		{Type: chain.BlurStep, Strength: 0.65, Region: chain.RectRegion(imagex.R(96, 96, 128, 128))},
	}}
	if _, err := eng.RunFrame(session.RunOptions{Benchmark: b, Chain: c1, ParamVersion: 1, SegmentSize: 32, MetricBlockSize: 32}); err != nil {
		t.Fatal(err)
	}
	c2 := c1
	c2.Steps = append([]chain.Step(nil), c1.Steps...)
	c2.Steps[0].Region = chain.RectRegion(imagex.R(32, 0, 64, 32))
	r2, err := eng.RunFrame(session.RunOptions{Benchmark: b, Chain: c2, ParamVersion: 2, SegmentSize: 32, MetricBlockSize: 32})
	if err != nil && !errs.IsCode(err, errs.EAttr) {
		t.Fatal(err)
	}
	if r2.Reuse[0].RecomputedUnits != 1 {
		t.Fatalf("moved region should recompute exactly one active segment: %+v", r2.Reuse[0])
	}
	if r2.Reuse[1].RecomputedUnits != 0 || r2.Reuse[1].Status != "reused" {
		t.Fatalf("far blur operator should remain reused: %+v", r2.Reuse[1])
	}
}
