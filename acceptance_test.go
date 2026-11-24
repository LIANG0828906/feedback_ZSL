package main_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"audiocheck/internal/bound"
	"audiocheck/internal/eval"
	"audiocheck/internal/generator"
	"audiocheck/internal/model"
	"audiocheck/internal/pipeline"
	"audiocheck/internal/store"
	"audiocheck/internal/verify"
)

func testManifest() model.Manifest {
	return model.Manifest{
		SchemaVersion: "audio-manifest-v1", SampleRate: 48000, FrameSize: 64, HopSize: 32,
		Channels: 1, FrameCount: 12, QualityGear: 2, Seed: 20261125, SampleBits: 16,
	}
}

func testParams() model.EvalParams {
	return model.EvalParams{WindowSizeSamples: 32, StepSamples: 8}
}

func generateTest(t *testing.T, m model.Manifest) (*model.Sequence, *generator.Truth, []model.FrameQuantRecord) {
	t.Helper()
	seq, truth, quant, err := generator.Generate(m)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return seq, truth, quant
}

func openTestStore(t *testing.T, name string, id model.Identity) *store.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), name)
	res, err := store.Open(dbPath, id)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = res.Store.Close() })
	return res.Store
}

func TestSchemaDecodeAndParameterErrors(t *testing.T) {
	m := testManifest()
	b, _ := json.Marshal(m)
	var raw map[string]any
	_ = json.Unmarshal(b, &raw)
	delete(raw, "hop_size")
	missing, _ := json.Marshal(raw)
	_, err := model.ParseManifest(missing)
	assertCodeField(t, err, model.CodeSchema, "hop_size")

	badDiv := m
	badDiv.HopSize = 30
	bad, _ := json.Marshal(badDiv)
	_, err = model.ParseManifest(bad)
	assertCodeField(t, err, model.CodeSchema, "frame_size")

	badGear := m
	badGear.QualityGear = 9
	bad, _ = json.Marshal(badGear)
	_, err = model.ParseManifest(bad)
	assertCodeField(t, err, model.CodePara, "quality_gear")

	seq, _, _, err := generator.Generate(m)
	if err != nil {
		t.Fatal(err)
	}
	short := seq.Original[:len(seq.Original)-m.Channels*5]
	err = model.ValidateSequence(m, short, seq.Reconstructed)
	var me *model.Error
	if !errors.As(err, &me) || me.Code != model.CodeDecode || me.Frame == nil {
		t.Fatalf("expected E_DECODE with frame, got %#v", err)
	}
}

func assertCodeField(t *testing.T, err error, code, field string) {
	t.Helper()
	var me *model.Error
	if !errors.As(err, &me) {
		t.Fatalf("expected *model.Error, got %T %v", err, err)
	}
	if me.Code != code {
		t.Fatalf("code = %s, want %s (%v)", me.Code, code, err)
	}
	if me.Field != field {
		t.Fatalf("field = %q, want %q", me.Field, field)
	}
}

func TestMetricKernelDeterministic(t *testing.T) {
	m := testManifest()
	seq, _, quant := generateTest(t, m)
	p := testParams()
	a, err := eval.EvaluateFull(m, seq.Original, seq.Reconstructed, p, quant)
	if err != nil {
		t.Fatal(err)
	}
	b, err := eval.EvaluateFull(m, seq.Original, seq.Reconstructed, p, quant)
	if err != nil {
		t.Fatal(err)
	}
	aj, _ := json.Marshal(a.Frames)
	bj, _ := json.Marshal(b.Frames)
	if string(aj) != string(bj) {
		t.Fatalf("metric kernel is not deterministic:\n%s\n%s", aj, bj)
	}

	zero := make([]int32, m.TotalInterleavedSamples())
	perfect, err := eval.EvaluateFull(m, zero, zero, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range perfect.Frames {
		if !f.PSNRInfinite || !f.SNRInfinite || f.SSIMFixed != model.MetricScale {
			t.Fatalf("unexpected perfect metric frame %d: %+v", f.Frame, f)
		}
	}
}

func TestChunkedMatchesMonolithicAndIndependentVerifier(t *testing.T) {
	m := testManifest()
	seq, truth, quant := generateTest(t, m)
	p := testParams()
	id, err := pipeline.BuildIdentity(m, seq, truth.Fingerprint, p)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "chunk.db")
	res, err := store.Open(dbPath, id)
	if err != nil {
		t.Fatal(err)
	}
	st := res.Store

	cfg := pipeline.Config{
		Manifest: m, Sequence: seq, Truth: truth, Quant: quant, Params: p, Store: st,
		Budget:         model.Budget{ChunkFrames: 3, MaxWorkers: 3, MaxPendingTasks: 1, PerThreadSamples: 262144},
		PersistSamples: true,
	}
	chunkReport, err := pipeline.RunFull(context.Background(), cfg)
	if err != nil {
		t.Fatalf("chunked run: %v", err)
	}
	mono, err := eval.EvaluateFull(m, seq.Original, seq.Reconstructed, p, quant)
	if err != nil {
		t.Fatal(err)
	}
	monoReport := &model.Report{Frames: mono.Frames, Blocks: []model.BlockResult{{Block: model.BlockCoord{Index: 0}, Frames: mono.Frames}}}
	monoReport.CommitOrder = chunkReport.CommitOrder
	if mm := verify.CompareReports(monoReport, chunkReport); mm != nil {
		t.Fatalf("chunk/mono mismatch: %+v", mm)
	}
	vr, err := verify.VerifyDatabase(dbPath, truth.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if !vr.OK || vr.CheckedFrames != m.FrameCount {
		t.Fatalf("verifier failed: %+v", vr)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestThreePathsAndResumeAfterStageFailure(t *testing.T) {
	m := testManifest()
	m.FrameCount = 8
	seq, truth, quant := generateTest(t, m)
	p := testParams()
	id, _ := pipeline.BuildIdentity(m, seq, truth.Fingerprint, p)
	budget := model.Budget{ChunkFrames: 2, MaxWorkers: 2, MaxPendingTasks: 1, PerThreadSamples: 262144}

	fullDB := filepath.Join(t.TempDir(), "full.db")
	fullStore, err := store.Open(fullDB, id)
	if err != nil {
		t.Fatal(err)
	}
	fullReport, err := pipeline.RunFull(context.Background(), pipeline.Config{Manifest: m, Sequence: seq, Truth: truth, Quant: quant, Params: p, Budget: budget, Store: fullStore.Store, PersistSamples: true})
	if err != nil {
		t.Fatalf("full: %v", err)
	}
	_ = fullStore.Store.Close()

	rtDB := filepath.Join(t.TempDir(), "rt.db")
	rtOpen, err := store.Open(rtDB, id)
	if err != nil {
		t.Fatal(err)
	}
	rtReport, err := pipeline.RunRealtime(context.Background(), pipeline.Config{Manifest: m, Sequence: seq, Truth: truth, Quant: quant, Params: p, Budget: budget, Store: rtOpen.Store, PersistSamples: true})
	if err != nil {
		t.Fatalf("realtime: %v", err)
	}
	_ = rtOpen.Store.Close()
	if mm := verify.CompareReports(fullReport, rtReport); mm != nil {
		t.Fatalf("realtime mismatch: %+v", mm)
	}

	resumeDB := filepath.Join(t.TempDir(), "resume.db")
	firstOpen, err := store.Open(resumeDB, id)
	if err != nil {
		t.Fatal(err)
	}
	injector := pipeline.NewFailureInjector()
	injector.FailAttempts = map[int]int{1: 1}
	firstReport, firstErr := pipeline.RunFull(context.Background(), pipeline.Config{
		Manifest: m, Sequence: seq, Truth: truth, Quant: quant, Params: p, Budget: budget,
		Store: firstOpen.Store, Failure: injector, MaxAttempts: 1, PersistSamples: true,
	})
	var pe *model.Error
	if !errors.As(firstErr, &pe) || pe.Code != model.CodeStage {
		t.Fatalf("expected E_STAGE, got %v", firstErr)
	}
	if pe.Block == nil || pe.Block.Index != 1 || pe.Frame == nil || *pe.Frame != pe.Block.FirstFrame {
		t.Fatalf("stage error location wrong: %+v", pe)
	}
	if len(firstReport.CommitOrder) != 1 || firstReport.CommitOrder[0] != 0 {
		t.Fatalf("only block 0 should commit, got %v", firstReport.CommitOrder)
	}
	matrix, _ := firstOpen.Store.StateMatrix(context.Background())
	if matrix[1][1].State != pipeline.StateFailed {
		t.Fatalf("block 1 evaluate state = %+v", matrix[1])
	}
	committed, _ := firstOpen.Store.CommitOrder(context.Background())
	if len(committed) != 1 || committed[0] != 0 {
		t.Fatalf("db committed order = %v", committed)
	}
	_ = firstOpen.Store.Close()

	reopen, err := store.Open(resumeDB, id)
	if err != nil {
		t.Fatal(err)
	}
	if reopen.Reset {
		t.Fatalf("same identity should not reset: %s", reopen.ResetReason)
	}
	resumeReport, err := pipeline.Resume(context.Background(), pipeline.Config{
		Manifest: m, Sequence: seq, Truth: truth, Quant: quant, Params: p, Budget: budget,
		Store: reopen.Store, PersistSamples: true,
	})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if mm := verify.CompareReports(fullReport, resumeReport); mm != nil {
		t.Fatalf("resume mismatch: %+v", mm)
	}
	vr, err := verify.VerifyDatabase(resumeDB, truth.Fingerprint)
	if err != nil || !vr.OK {
		t.Fatalf("resume verifier err=%v result=%+v", err, vr)
	}
	_ = reopen.Store.Close()
}

func TestStateMachineIllegalTransitionKeepsState(t *testing.T) {
	mc := pipeline.NewMachine(2)
	err := mc.Transition(pipeline.StageEvaluate, pipeline.StateCommitted, "skip")
	assertCodeField(t, err, model.CodeState, "state")
	if mc.States[pipeline.StageEvaluate].State != pipeline.StatePending {
		t.Fatalf("illegal transition changed state to %s", mc.States[pipeline.StageEvaluate].State)
	}
	if err := mc.Transition(pipeline.StageEvaluate, pipeline.StateProcessing, ""); err != nil {
		t.Fatal(err)
	}
	if err := mc.Transition(pipeline.StageEvaluate, pipeline.StateFailed, "boom"); err != nil {
		t.Fatal(err)
	}
	if err := mc.Transition(pipeline.StageEvaluate, pipeline.StateProcessing, "retry"); err != nil {
		t.Fatal(err)
	}
	if err := mc.Transition(pipeline.StageEvaluate, pipeline.StateCommitted, ""); err != nil {
		t.Fatal(err)
	}
	err = mc.Transition(pipeline.StageEvaluate, pipeline.StateFailed, "cannot fail committed directly")
	assertCodeField(t, err, model.CodeState, "state")
}

func TestBandShortageReturnsCoordinateAndMinimum(t *testing.T) {
	m := testManifest()
	m.HopSize = 64
	p := testParams()
	p.SideBandSamples = 1
	_, err := eval.PlanBlock(m, p, 1, 1)
	var pe *model.Error
	if !errors.As(err, &pe) || pe.Code != model.CodeBand {
		t.Fatalf("expected E_BAND, got %v", err)
	}
	if pe.Block == nil || pe.Block.Index != 1 || pe.Block.MinBandSamples < 16 {
		t.Fatalf("bad E_BAND coordinate/min band: %+v", pe.Block)
	}
}

func TestEvalParameterOutOfRange(t *testing.T) {
	m := testManifest()
	p := testParams()
	p.WindowSizeSamples = m.SamplesPerChannel() + 1
	assertCodeField(t, p.Validate(m), model.CodePara, "window_size_samples")

	p = testParams()
	p.StepSamples = m.SamplesPerChannel() + 1
	assertCodeField(t, p.Validate(m), model.CodePara, "step_samples")
}

func TestAffectedBlockPlanning(t *testing.T) {
	m := testManifest()
	budget := model.Budget{ChunkFrames: 3}
	all := pipeline.AffectedBlocks(m, budget, pipeline.ParameterChange{QualityChanged: true})
	if len(all) != 4 {
		t.Fatalf("quality change affects all 4 blocks, got %v", all)
	}
	none := pipeline.AffectedBlocks(m, budget, pipeline.ParameterChange{
		OldParams: model.EvalParams{WindowSizeSamples: 32, StepSamples: 8, SideBandSamples: 0},
		NewParams: model.EvalParams{WindowSizeSamples: 32, StepSamples: 8, SideBandSamples: 64},
	})
	if len(none) != 0 {
		t.Fatalf("sufficient side-band allocation alone must not recompute metrics, got %v", none)
	}
}

func TestBoundViolation(t *testing.T) {
	b := bound.Build(bound.Input{Frame: 3, QuantStep: 12, FrameSize: 64, WindowSize: 32, Peak: 32767})
	err := bound.CheckFrame(3, 999*model.MetricScale, false, 1, b)
	assertCodeField(t, err, model.CodeBound, "snr_db_fixed")
	var pe *model.Error
	_ = errors.As(err, &pe)
	if pe.Frame == nil || *pe.Frame != 3 {
		t.Fatalf("bound error missing frame: %+v", pe)
	}

	err = bound.CheckFrame(3, 60*model.MetricScale, false, b.RMSErrorUpperFixed+1, b)
	assertCodeField(t, err, model.CodeBound, "noise_floor_std_fixed")
}

func TestCheckpointVersionAndContentMismatchResets(t *testing.T) {
	m := testManifest()
	seq, truth, quant := generateTest(t, m)
	p := testParams()
	id, _ := pipeline.BuildIdentity(m, seq, truth.Fingerprint, p)
	dbPath := filepath.Join(t.TempDir(), "version.db")
	st, err := store.Open(dbPath, id)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pipeline.RunFull(context.Background(), pipeline.Config{
		Manifest: m, Sequence: seq, Truth: truth, Quant: quant, Params: p,
		Budget: model.Budget{ChunkFrames: 12, MaxWorkers: 1, MaxPendingTasks: 1, PerThreadSamples: 262144}, Store: st.Store,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Store.Close()

	badAlgo := id
	badAlgo.AlgorithmVersion = "metric-core-v0.broken"
	reopen, err := store.Open(dbPath, badAlgo)
	if err != nil {
		t.Fatal(err)
	}
	if !reopen.Reset {
		t.Fatal("algorithm version mismatch must reset snapshot")
	}
	blocks, _ := reopen.Store.ListBlocks(context.Background())
	if len(blocks) != 0 {
		t.Fatalf("reset snapshot has %d blocks", len(blocks))
	}
	_ = reopen.Store.Close()

	badContent := id
	badContent.ContentHash = "sha256:tampered"
	reopen2, err := store.Open(dbPath, badContent)
	if err != nil {
		t.Fatal(err)
	}
	if !reopen2.Reset {
		t.Fatal("content hash mismatch must reset snapshot")
	}
	_ = reopen2.Store.Close()
}

func TestBudgetParallelismAndQueueWait(t *testing.T) {
	m := testManifest()
	m.FrameCount = 12
	seq, truth, quant := generateTest(t, m)
	p := testParams()
	id, _ := pipeline.BuildIdentity(m, seq, truth.Fingerprint, p)
	st := openTestStore(t, "budget.db", id)

	var active int32
	var maxActive int32
	var entries int32
	gate := make(chan struct{})
	cfg := pipeline.Config{
		Manifest: m, Sequence: seq, Truth: truth, Quant: quant, Params: p, Store: st,
		Budget: model.Budget{ChunkFrames: 1, MaxWorkers: 2, MaxPendingTasks: 1, PerThreadSamples: 262144},
		Hooks: pipeline.Hooks{OnWorkerStart: func(blockIndex, workerID int) {
			n := atomic.AddInt32(&entries, 1)
			if n <= 2 {
				cur := atomic.AddInt32(&active, 1)
				for {
					old := atomic.LoadInt32(&maxActive)
					if cur <= old || atomic.CompareAndSwapInt32(&maxActive, old, cur) {
						break
					}
				}
				<-gate
				atomic.AddInt32(&active, -1)
			}
		}},
		PersistSamples: true,
	}
	done := make(chan error, 1)
	go func() {
		_, err := pipeline.RunFull(context.Background(), cfg)
		done <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt32(&active) < 2 && atomic.LoadInt32(&entries) < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if atomic.LoadInt32(&active) != 2 {
		t.Fatalf("workers did not saturate: active=%d", active)
	}
	time.Sleep(50 * time.Millisecond)
	close(gate)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pipeline did not finish after gate release")
	}
	if maxActive > 2 {
		t.Fatalf("parallelism exceeded budget: max=%d", maxActive)
	}
	blocks, err := st.ListBlocks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sawBackpressure := false
	for _, b := range blocks {
		if b.Queue.BufferSamples > b.Queue.MaxBufferSamples {
			t.Fatalf("buffer budget exceeded: %d > %d", b.Queue.BufferSamples, b.Queue.MaxBufferSamples)
		}
		if b.Queue.Reason == "pending_task_queue_full" && b.Queue.TotalWaitNS > 0 {
			sawBackpressure = true
		}
	}
	if !sawBackpressure {
		t.Fatalf("expected recorded pending queue backpressure, blocks=%+v", blocks)
	}
}

func TestVerifierLocatesTamperedField(t *testing.T) {
	m := testManifest()
	seq, truth, quant := generateTest(t, m)
	p := testParams()
	id, _ := pipeline.BuildIdentity(m, seq, truth.Fingerprint, p)
	dbPath := filepath.Join(t.TempDir(), "tamper.db")
	openRes, err := store.Open(dbPath, id)
	if err != nil {
		t.Fatal(err)
	}
	report, err := pipeline.RunFull(context.Background(), pipeline.Config{
		Manifest: m, Sequence: seq, Truth: truth, Quant: quant, Params: p,
		Budget: model.Budget{ChunkFrames: 3, MaxWorkers: 2, MaxPendingTasks: 2, PerThreadSamples: 262144},
		Store:  openRes.Store, PersistSamples: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	report.Blocks[0].Frames[0].SSIMFixed += 2
	if _, err := openRes.Store.CommitBlock(context.Background(), report.Blocks[0]); err != nil {
		t.Fatal(err)
	}
	_ = openRes.Store.Close()
	vr, err := verify.VerifyDatabase(dbPath, truth.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if vr.OK || len(vr.Mismatches) == 0 {
		t.Fatal("expected verifier mismatch")
	}
	mm := vr.Mismatches[0]
	if mm.Field != "ssim_fixed" || mm.Frame == nil || *mm.Frame != 0 || mm.BlockIndex != 0 {
		t.Fatalf("unexpected first mismatch: %+v", mm)
	}
}
