package engine_test

import (
	"testing"

	"replayaudit/internal/engine"
	"replayaudit/internal/gen"
	"replayaudit/internal/model"
)

func TestAllSamplesMatchTruth(t *testing.T) {
	p := model.DefaultParams()
	for _, b := range gen.BuildAll() {
		rep, err := engine.SettleFull(b.Replay, p)
		if err != nil {
			t.Fatalf("%s settle: %v", b.Replay.ReplayID, err)
		}
		if mm := engine.CompareTruth(b.Truth, rep, p.Tolerance); mm != nil {
			t.Fatalf("%s truth mismatch: %v", b.Replay.ReplayID, mm)
		}
		if rep.Winner != b.Truth.Winner {
			t.Fatalf("%s winner %q want %q", b.Replay.ReplayID, rep.Winner, b.Truth.Winner)
		}
	}
}

func TestSettlementDeterministicAndPooledEqual(t *testing.T) {
	p := model.DefaultParams()
	p.MaxParallel = 4
	p.WorkerBuffer = 2
	for _, b := range gen.BuildAll() {
		r1, err := engine.SettleFull(b.Replay, p)
		if err != nil {
			t.Fatal(err)
		}
		r2, err := engine.SettleFull(b.Replay, p)
		if err != nil {
			t.Fatal(err)
		}
		if mm := engine.CompareReports(r1, r2); mm != nil {
			t.Fatalf("non deterministic: %v", mm)
		}
		r3, peak, _, err := engine.SettleFullPooled(b.Replay, p)
		if err != nil {
			t.Fatal(err)
		}
		if mm := engine.CompareReports(r1, r3); mm != nil {
			t.Fatalf("pooled path differs: %v", mm)
		}
		if peak > p.MaxParallel {
			t.Fatalf("peak parallelism %d exceeds bound %d", peak, p.MaxParallel)
		}
	}
}

func TestCommitSequenceStable(t *testing.T) {
	p := model.DefaultParams()
	for _, b := range gen.BuildAll() {
		rep, err := engine.SettleFull(b.Replay, p)
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.CommitSequence) == 0 {
			t.Fatalf("%s: empty commit sequence", b.Replay.ReplayID)
		}
		for i, c := range rep.CommitSequence {
			if c.Segment != i || c.CommitSeq != i {
				t.Fatalf("%s: non contiguous commit at %d: %+v", b.Replay.ReplayID, i, c)
			}
			if c.SegmentHash == "" || c.StateHash == "" {
				t.Fatalf("%s: empty hashes in commit %d", b.Replay.ReplayID, i)
			}
		}
	}
}
