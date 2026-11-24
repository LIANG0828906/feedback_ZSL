package session

import (
	"context"
	"reflect"
	"testing"

	"spatialworkbench/internal/generator"
	"spatialworkbench/internal/model"
	"spatialworkbench/internal/werr"
)

func sampleAndParams() (model.Sample, model.Params) {
	sample := generator.Generate(generator.Options{Seed: 123, Samples: 1, Prefix: "session"}).Samples[0]
	return sample, model.DefaultParams(sample)
}

func TestStateMachineAndIllegalTransition(t *testing.T) {
	sample, params := sampleAndParams()
	s, err := Open(sample, params, nil, Injection{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	before, err := s.Frame(0)
	if err != nil {
		t.Fatal(err)
	}
	if before.State != StatePending {
		t.Fatalf("initial state %s", before.State)
	}
	if _, err := s.CommitNext(context.Background()); err == nil || err.Code != werr.CodeState {
		t.Fatalf("commit pending must return E_STATE, got %#v", err)
	}
	after, _ := s.Frame(0)
	if after.State != StatePending {
		t.Fatalf("illegal commit changed state to %s", after.State)
	}
	if _, err := s.ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	estimated, _ := s.Frame(0)
	if estimated.State != StateEstimated {
		t.Fatalf("process state %s", estimated.State)
	}
	if _, err := s.CommitNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	committed, _ := s.Frame(0)
	if committed.State != StateCommitted || committed.Seq != 0 {
		t.Fatalf("commit state %s seq %d", committed.State, committed.Seq)
	}
	if got := s.Progress(); got.Committed != 1 || got.Total != params.TotalFrames(len(sample.Dry.Samples)) {
		t.Fatalf("progress %#v", got)
	}
}

func TestFailedFrameRetryDoesNotPolluteCommitted(t *testing.T) {
	sample, params := sampleAndParams()
	bad := 1
	s, err := Open(sample, params, nil, Injection{BadFrame: &bad})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.ProcessAndCommitNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	committedBefore, _ := s.Frame(0)
	_, err = s.ProcessNext(context.Background())
	if err == nil || err.Code != werr.CodeFrame || err.Frame != 1 {
		t.Fatalf("expected injected E_FRAME at frame 1, got %#v", err)
	}
	failed, _ := s.Frame(1)
	if failed.State != StateFailed {
		t.Fatalf("frame 1 state %s", failed.State)
	}
	committedAfter, _ := s.Frame(0)
	if !reflect.DeepEqual(committedBefore.Result, committedAfter.Result) {
		t.Fatal("failed frame polluted committed frame")
	}
	if err := s.RetryFrame(1); err != nil {
		t.Fatal(err)
	}
	s.SetInjection(Injection{})
	retried, _ := s.Frame(1)
	if retried.State != StatePending {
		t.Fatalf("retry state %s", retried.State)
	}
	if _, err := s.ProcessAndCommitNext(context.Background()); err != nil {
		t.Fatalf("retry processing: %v", err)
	}
	if got := s.Progress().Committed; got != 2 {
		t.Fatalf("committed %d, want 2", got)
	}
}

func TestCommittedFrameFreeze(t *testing.T) {
	sample, params := sampleAndParams()
	s, err := Open(sample, params, nil, Injection{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 2; i++ {
		if _, err := s.ProcessAndCommitNext(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, _ := s.Frame(1)
	if err := s.ReestimateUncommitted(1); err == nil || err.Code != werr.CodeFrozen || err.Frame != 1 {
		t.Fatalf("expected E_FROZEN at frame 1, got %#v", err)
	}
	if err := s.UpdateUncommittedParams(1, params); err == nil || err.Code != werr.CodeFrozen {
		t.Fatalf("expected E_FROZEN update, got %#v", err)
	}
	if _, err := s.CommitFrame(context.Background(), 1); err == nil || err.Code != werr.CodeFrozen {
		t.Fatalf("expected E_FROZEN recommit, got %#v", err)
	}
	after, _ := s.Frame(1)
	if !reflect.DeepEqual(snapshot.Result, after.Result) {
		t.Fatal("committed result changed")
	}
	if err := s.ReestimateUncommitted(2); err != nil {
		t.Fatalf("uncommitted reset should be allowed: %v", err)
	}
}

func TestInjectionChains(t *testing.T) {
	sample, params := sampleAndParams()
	if _, err := Open(sample, params, nil, Injection{MissingChannel: "C"}); err == nil || err.Code != werr.CodeChan || err.Channel != "C" {
		t.Fatalf("missing channel: %#v", err)
	}
	frame := 0
	s, err := Open(sample, params, nil, Injection{InvertArrivalChannel: "FL", InvertArrivalFrame: &frame})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.ProcessNext(context.Background())
	if err == nil || err.Code != werr.CodeOrder || err.Frame != 0 || err.Channel != "FL" {
		t.Fatalf("order injection: %#v", err)
	}
	badSample := sample.Clone()
	badSample.Channels[0].Truth.Reflections[0].Arrival = badSample.Channels[0].Truth.DirectArrival
	if _, err := Open(badSample, params, nil, Injection{}); err == nil || err.Code != werr.CodeOrder {
		t.Fatalf("input order: %#v", err)
	}
	clipSample := sample.Clone()
	clipSample.Channels[0].Samples[clipSample.Channels[0].Truth.DirectArrival] = 1.5
	if _, err := Open(clipSample, params, nil, Injection{}); err == nil || err.Code != werr.CodeClip || err.Channel == "" {
		t.Fatalf("input clip: %#v", err)
	}
	s2, err := Open(sample, params, nil, Injection{GainOverflowChannel: "FL", GainOverflowFrame: &frame})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	_, err = s2.ProcessNext(context.Background())
	if err == nil || err.Code != werr.CodeClip || err.Frame != 0 || err.Channel != "FL" {
		t.Fatalf("clip injection: %#v", err)
	}
}

func TestParameterGenerationResetBeforeFirstCommit(t *testing.T) {
	sample, params := sampleAndParams()
	s, err := Open(sample, params, nil, Injection{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	changed := params.Clone()
	changed.ErrorTolerance += 1
	if err := s.UpdateUncommittedParams(0, changed); err != nil {
		t.Fatal(err)
	}
	for _, frame := range s.Frames() {
		if frame.State != StatePending || frame.Result != nil {
			t.Fatalf("frame %d not reset after generation change: %#v", frame.Index, frame)
		}
	}
}

func TestParallelBatchMatchesSequential(t *testing.T) {
	sample, params := sampleAndParams()
	params.MaxWorkers = 3
	sequential, err := Open(sample, params, nil, Injection{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sequential.ProcessAndCommitAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	seqOutput, err := sequential.Output()
	if err != nil {
		t.Fatal(err)
	}
	sequential.Close()
	batch, err := Open(sample, params, nil, Injection{})
	if err != nil {
		t.Fatal(err)
	}
	for {
		progress := batch.Progress()
		if progress.Committed == progress.Total {
			break
		}
		if _, err := batch.ProcessAndCommitBatch(context.Background(), 4); err != nil {
			t.Fatal(err)
		}
	}
	batchOutput, err := batch.Output()
	if err != nil {
		t.Fatal(err)
	}
	batch.Close()
	if !reflect.DeepEqual(seqOutput, batchOutput) {
		t.Fatal("parallel batch differs from sequential path")
	}
}
