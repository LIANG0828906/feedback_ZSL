package engine

import (
	"context"
	"math"
	"path/filepath"
	"testing"

	"loudnessworkbench/apperr"
	"loudnessworkbench/audio"
	"loudnessworkbench/checkpoint"
	"loudnessworkbench/dsp"
	"loudnessworkbench/project"
)

func engineProgram() *project.Program {
	channelPattern := []int{1, 2, 1, 2}
	segments := make([]project.Segment, len(channelPattern))
	for i, channels := range channelPattern {
		signal := audio.New(48000, channels, int(0.8*48000)+i*1200)
		for frame := 0; frame < signal.Frames(); frame++ {
			t := float64(frame) / 48000
			envelope := 0.4 + 0.35*math.Sin(2*math.Pi*(0.4+0.1*float64(i))*t)
			value := envelope * (0.10*math.Sin(2*math.Pi*(440+220*float64(i))*t) +
				0.03*math.Sin(2*math.Pi*2200*t))
			for channel := 0; channel < channels; channel++ {
				signal.Set(frame, channel, value)
			}
		}
		signal = audio.QuantizeF32(signal)
		segments[i] = project.Segment{
			Index: i, ID: string(rune('a' + i)), Channels: channels,
			ContentSHA256: audio.ContentHash(signal), Source: signal,
		}
	}
	return &project.Program{
		ProjectID: "engine-fixture", SourceVersion: project.SourceV1, SampleRateHz: 48000,
		TargetLUFS: -23, FrameMs: 400, HopMs: 100,
		Gate:       dsp.GateConfig{AbsoluteLUFS: -70, RelativeLU: 10},
		Compressor: dsp.CompressorConfig{ThresholdDBFS: -20, Ratio: 3, AttackMs: 20, ReleaseMs: 180},
		Limiter:    dsp.LimiterConfig{CeilingDBTP: -1, LookaheadMs: 5},
		StageOrder: append([]string(nil), project.CanonicalStageOrder...),
		Segments:   segments,
	}
}

func TestParallelRealtimeAndResumeAreSampleIdentical(t *testing.T) {
	program := engineProgram()
	root := t.TempDir()
	fullStore := checkpoint.NewStore(filepath.Join(root, "full"), program.ProjectID)
	full, err := run(t, program, fullStore, ModeFull, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !full.AllCommitted() {
		t.Fatal("full run did not commit all segments")
	}
	if math.Abs(full.FinalProgramLoudnessLUFS-full.TargetLUFS) > 0.02 {
		t.Fatalf("final loudness %.4f not on target %.4f", full.FinalProgramLoudnessLUFS, full.TargetLUFS)
	}
	realtimeStore := checkpoint.NewStore(filepath.Join(root, "realtime"), program.ProjectID)
	realtime, err := run(t, program, realtimeStore, ModeRealtime, 8)
	if err != nil {
		t.Fatal(err)
	}
	if realtime.MaxParallel != 1 {
		t.Fatalf("realtime parallel = %d, want 1", realtime.MaxParallel)
	}
	diff, err := CompareResults(full, realtime)
	if err != nil || !diff.Identical || !diff.GainDecisionsEqual {
		t.Fatalf("full/realtime differ: %v %+v", err, diff)
	}

	snapshot, err := fullStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	resumeStore := checkpoint.NewStore(filepath.Join(root, "resume"), program.ProjectID)
	if err := resumeStore.Save(*snapshot); err != nil {
		t.Fatal(err)
	}
	resumed, err := run(t, program, resumeStore, ModeResume, 3)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Recovery.ReusedSegments != len(program.Segments) {
		t.Fatalf("reused %d segments", resumed.Recovery.ReusedSegments)
	}
	diff, err = CompareResults(full, resumed)
	if err != nil || !diff.Identical {
		t.Fatalf("full/resume differ: %v", err)
	}

	partial := *snapshot
	partial.Records = append([]checkpoint.Record(nil), snapshot.Records[:2]...)
	partialStore := checkpoint.NewStore(filepath.Join(root, "partial"), program.ProjectID)
	if err := partialStore.Save(partial); err != nil {
		t.Fatal(err)
	}
	resumedPartial, err := run(t, program, partialStore, ModeResume, 2)
	if err != nil {
		t.Fatal(err)
	}
	if resumedPartial.Recovery.ReusedSegments != 2 {
		t.Fatalf("partial reuse = %d, want 2", resumedPartial.Recovery.ReusedSegments)
	}
	diff, err = CompareResults(full, resumedPartial)
	if err != nil || !diff.Identical {
		t.Fatalf("full/partial-resume differ: %v", err)
	}
}

func TestHashMismatchInvalidatesWholeSnapshot(t *testing.T) {
	program := engineProgram()
	root := t.TempDir()
	fullStore := checkpoint.NewStore(filepath.Join(root, "full"), program.ProjectID)
	full, err := run(t, program, fullStore, ModeFull, 2)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := fullStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Records[0].OutputHash = "0000000000000000000000000000000000000000000000000000000000000000"
	badStore := checkpoint.NewStore(filepath.Join(root, "bad"), program.ProjectID)
	if err := badStore.Save(*snapshot); err != nil {
		t.Fatal(err)
	}
	recovered, err := run(t, program, badStore, ModeResume, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered.Recovery.Invalidated || recovered.Recovery.CheckpointUsed {
		t.Fatalf("unexpected recovery flags: %+v", recovered.Recovery)
	}
	if !recovered.AllCommitted() {
		t.Fatal("full recompute did not commit")
	}
	diff, err := CompareResults(full, recovered)
	if err != nil || !diff.Identical {
		t.Fatalf("recovered output diverged: %v", err)
	}
}

func TestRetryAfterPartialCommitKeepsCommittedSegment(t *testing.T) {
	program := engineProgram()
	root := t.TempDir()
	store := checkpoint.NewStore(filepath.Join(root, "store"), program.ProjectID)
	full, err := run(t, program, store, ModeFull, 2)
	if err != nil {
		t.Fatal(err)
	}
	retrySession, err := NewSession(program, store)
	if err != nil {
		t.Fatal(err)
	}
	retrySession.states[0].state = StateCommitted
	retrySession.states[1].state = StateFailed
	retried, err := retrySession.Run(context.Background(), RunOptions{
		Mode: ModeResume, MaxParallel: 2, PerWorkerBufferBytes: 16 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !retried.AllCommitted() {
		t.Fatal("retry did not commit every segment")
	}
	if retried.Recovery.ReusedSegments < 1 {
		t.Fatal("retry did not reuse the committed segment")
	}
	diff, err := CompareResults(full, retried)
	if err != nil || !diff.Identical {
		t.Fatalf("retry output differs: %v", err)
	}
}

func TestDivergenceLocatesFirstSampleAndSegment(t *testing.T) {
	program := engineProgram()
	result, err := run(t, program, nil, ModeFull, 2)
	if err != nil {
		t.Fatal(err)
	}
	other := *result
	other.Mix = result.Mix.Clone()
	other.Mix.Data[17] += 1e-6
	other.Segments = append([]*SegmentResult(nil), result.Segments...)
	report, err := CompareResults(result, &other)
	if !apperr.IsCode(err, apperr.CodeDivergence) {
		t.Fatalf("got %v, want E_DIVERGE", err)
	}
	if report.FirstDivergence == nil || report.FirstDivergence.SampleIndex != 17 ||
		report.FirstDivergence.SegmentID != program.Segments[0].ID {
		t.Fatalf("wrong divergence location: %+v", report.FirstDivergence)
	}
}

func TestStateMachineRejectsIllegalTransitionAndPreservesState(t *testing.T) {
	machine := newStateMachine(3, "seg")
	if err := machine.Transition(StateProcessing); err != nil {
		t.Fatal(err)
	}
	if err := machine.Transition(StateCommitted); err != nil {
		t.Fatal(err)
	}
	if err := machine.Transition(StateProcessing); !apperr.IsCode(err, apperr.CodeState) {
		t.Fatalf("got %v, want E_STATE", err)
	}
	if machine.Current() != StateCommitted {
		t.Fatalf("state changed to %s after illegal transition", machine.Current())
	}
	for _, transition := range []struct {
		from, to SegmentState
		legal    bool
	}{
		{StatePending, StateProcessing, true},
		{StateProcessing, StateFailed, true},
		{StateProcessing, StatePending, true},
		{StateFailed, StateProcessing, true},
		{StateFailed, StatePending, true},
		{StateCommitted, StateFailed, false},
	} {
		if got := LegalTransition(transition.from, transition.to); got != transition.legal {
			t.Fatalf("LegalTransition(%s,%s) = %v", transition.from, transition.to, got)
		}
	}
}

func TestPerWorkerBufferValidation(t *testing.T) {
	program := engineProgram()
	required := requiredBytes(program.Segments[0].Frames(), program.Segments[0].Channels)
	_, err := run(t, program, nil, ModeFull, 2, required-1)
	if !apperr.IsCode(err, apperr.CodeParameter) {
		t.Fatalf("got %v, want E_PARA", err)
	}
}

func TestSegmentDecodeFailureIsIsolatedAndRetryable(t *testing.T) {
	program := engineProgram()
	original := program.Segments[1].Source.Data[17]
	session, err := NewSession(program, nil)
	if err != nil {
		t.Fatal(err)
	}
	program.Segments[1].Source.Data[17] = math.NaN()
	options := RunOptions{Mode: ModeFull, MaxParallel: 2, PerWorkerBufferBytes: 16 << 20}
	result, err := session.Run(context.Background(), options)
	if !apperr.IsCode(err, apperr.CodeDecode) {
		t.Fatalf("got %v, want E_DECODE", err)
	}
	if result.Segments[1].State != StateFailed {
		t.Fatalf("failed segment state = %s", result.Segments[1].State)
	}
	for _, index := range []int{0, 2, 3} {
		if result.Segments[index].State != StateProcessing {
			t.Fatalf("healthy segment %d state = %s", index, result.Segments[index].State)
		}
	}
	program.Segments[1].Source.Data[17] = original
	retried, err := session.Run(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !retried.AllCommitted() {
		t.Fatal("retry did not commit every segment")
	}
}

func TestCommittedSessionRejectsParameterChange(t *testing.T) {
	program := engineProgram()
	session, err := NewSession(program, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = session.Run(context.Background(), RunOptions{Mode: ModeFull, MaxParallel: 2, PerWorkerBufferBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	chain := program.ChainConfig()
	chain.TargetLUFS = -20
	if err := session.UpdateChain(chain); !apperr.IsCode(err, apperr.CodeState) {
		t.Fatalf("got %v, want E_STATE", err)
	}
	if session.Statuses()[0].State != StateCommitted {
		t.Fatal("committed state was not preserved")
	}
}

func run(t *testing.T, program *project.Program, store *checkpoint.Store, mode Mode, parallel int, buffers ...int64) (*Result, error) {
	t.Helper()
	session, err := NewSession(program, store)
	if err != nil {
		t.Fatal(err)
	}
	buffer := int64(16 << 20)
	if len(buffers) > 0 {
		buffer = buffers[0]
	}
	return session.Run(context.Background(), RunOptions{
		Mode: mode, MaxParallel: parallel, PerWorkerBufferBytes: buffer,
	})
}
