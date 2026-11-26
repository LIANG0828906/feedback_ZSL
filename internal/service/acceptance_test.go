package service

import (
	"context"
	"path/filepath"
	"testing"

	"lip-pipeline-prototype/internal/domain"
)

func newTestService(t *testing.T, name string) *Service {
	t.Helper()
	db := filepath.Join(t.TempDir(), name+".db")
	svc, err := New(context.Background(), db)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

func TestRealtimeResumeAndFullPathsMatch(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, "paths")

	full, _, err := svc.FullRecompute()
	if err != nil {
		t.Fatal(err)
	}
	realtime, err := svc.RunRealtime(ctx, RunRequest{MaxParallelism: 4, CandidateBufferPerWorker: 2, FrameQueueBudget: 128})
	if err != nil {
		t.Fatal(err)
	}
	if len(realtime.Receipts) != len(svc.order) {
		t.Fatalf("committed shards=%d want %d", len(realtime.Receipts), len(svc.order))
	}
	if err := CompareDispatchSequences(full, realtime.Scheduler.Records); err != nil {
		t.Fatalf("realtime/full divergence: %v", err)
	}

	breakpointDB := filepath.Join(t.TempDir(), "breakpoint.db")
	breakSvc, err := New(ctx, breakpointDB)
	if err != nil {
		t.Fatal(err)
	}
	first := breakSvc.order[0]
	if _, err := breakSvc.store.CommitShard(ctx, breakSvc.results[first], domain.SchemaV3); err != nil {
		t.Fatal(err)
	}
	if err := breakSvc.Close(); err != nil {
		t.Fatal(err)
	}

	resumeSvc, err := New(ctx, breakpointDB)
	if err != nil {
		t.Fatal(err)
	}
	defer resumeSvc.Close()
	resume, err := resumeSvc.Resume(ctx, RunRequest{MaxParallelism: 2, CandidateBufferPerWorker: 2, FrameQueueBudget: 128})
	if err != nil {
		t.Fatal(err)
	}
	if len(resume.Recovered) != 1 || resume.Recovered[0] != first {
		t.Fatalf("recovered=%v want only %s", resume.Recovered, first)
	}
	if len(resume.Recomputed) != len(resumeSvc.order)-1 {
		t.Fatalf("recomputed=%v", resume.Recomputed)
	}
	fullAfterRestart, _, err := resumeSvc.FullRecompute()
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareDispatchSequences(fullAfterRestart, resume.Records); err != nil {
		t.Fatalf("resume/full divergence: %v", err)
	}

	if err := resumeSvc.CorruptCheckpoint(ctx, first); err != nil {
		t.Fatal(err)
	}
	resumeAfterCorrupt, err := resumeSvc.Resume(ctx, RunRequest{MaxParallelism: 2, FrameQueueBudget: 128})
	if err != nil {
		t.Fatal(err)
	}
	if len(resumeAfterCorrupt.Invalidated) != 1 || resumeAfterCorrupt.Invalidated[0] != first {
		t.Fatalf("invalidated=%v want %s", resumeAfterCorrupt.Invalidated, first)
	}
	if err := CompareDispatchSequences(fullAfterRestart, resumeAfterCorrupt.Records); err != nil {
		t.Fatalf("post-corruption resume divergence: %v", err)
	}
}

func TestEditInvalidatesOnlyAffectedDependencies(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, "edit")
	shardID := svc.order[0]
	_, shard, err := svc.findShard(shardID)
	if err != nil {
		t.Fatal(err)
	}

	weightEdit, err := svc.Edit(ctx, EditRequest{PhonemeID: shard.Phonemes[0].ID, Field: "weight_permyriad", Value: 1111})
	if err != nil {
		t.Fatal(err)
	}
	report := weightEdit.Reports[0]
	if len(report.Invalidated) == 0 || len(report.Reused) == 0 {
		t.Fatalf("expected both invalidated and reused nodes: inv=%d reuse=%d", len(report.Invalidated), len(report.Reused))
	}
	persistentHits := 0
	for _, item := range report.Reused {
		if item.PersistentHit {
			persistentHits++
		}
	}
	if persistentHits == 0 {
		t.Fatalf("expected persistent cache reuse: %+v", report.Reused)
	}

	last := shard.Phonemes[len(shard.Phonemes)-1]
	boundaryEdit, err := svc.Edit(ctx, EditRequest{PhonemeID: last.ID, Field: "end_sample", Value: last.EndSample + 4800})
	if err != nil {
		t.Fatalf("boundary edit: %v", err)
	}
	if len(boundaryEdit.Reports[0].Invalidated) == 0 {
		t.Fatal("boundary edit should invalidate downstream nodes")
	}

	fpsEdit, err := svc.Edit(ctx, EditRequest{FPS: 24})
	if err != nil {
		t.Fatal(err)
	}
	if len(fpsEdit.Reports) != len(svc.order) {
		t.Fatalf("fps reports=%d want %d", len(fpsEdit.Reports), len(svc.order))
	}
	totalInvalidated := 0
	for _, r := range fpsEdit.Reports {
		totalInvalidated += len(r.Invalidated)
	}
	if totalInvalidated == 0 {
		t.Fatal("fps switch should invalidate frame/dispatch nodes")
	}
}

func TestFourInjectedFaultCodesAndLocations(t *testing.T) {
	svc := newTestService(t, "faults")
	tests := []struct {
		kind string
		code domain.ErrorCode
	}{
		{"order", domain.CodeOrder},
		{"decode", domain.CodeDecode},
		{"val", domain.CodeVal},
		{"stage", domain.CodeStage},
	}
	for _, tc := range tests {
		t.Run(tc.kind, func(t *testing.T) {
			err := svc.Fault(tc.kind)
			de, ok := domain.AsError(err)
			if !ok || de.Code != tc.code {
				t.Fatalf("fault %s = %v", tc.kind, err)
			}
			if de.DialogueID == "" || de.ShardIndex == nil {
				t.Fatalf("fault %s missing dialogue/shard location: %+v", tc.kind, de)
			}
			switch tc.code {
			case domain.CodeOrder, domain.CodeStage:
				if de.Frame == nil {
					t.Fatalf("fault %s missing frame location: %+v", tc.kind, de)
				}
			case domain.CodeDecode:
				if de.OperandA == "" || de.OperandB == "" {
					t.Fatalf("E_DECODE should include count operands: %+v", de)
				}
			}
		})
	}
}

func TestDivergenceFaultContainsFirstFrameAndPack(t *testing.T) {
	svc := newTestService(t, "diverge")
	err := svc.Fault("diverge")
	de, ok := domain.AsError(err)
	if !ok || de.Code != domain.CodeDiverge || de.Frame == nil || de.PackIndex == nil {
		t.Fatalf("expected E_DIVERGE with frame and pack, got %+v", err)
	}
}

func TestSingleShardFailureIsIsolatedAndRecoverable(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, "isolation")
	faultShard := svc.order[2]
	run, err := svc.RunRealtime(ctx, RunRequest{
		MaxParallelism: 4, CandidateBufferPerWorker: 2, FrameQueueBudget: 128,
		FaultShardID: faultShard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := run.FailedShards[faultShard]; !ok {
		t.Fatalf("fault shard should be marked failed: %+v", run.FailedShards)
	}
	if len(run.Receipts) != len(svc.order)-1 {
		t.Fatalf("successful commits=%d want %d", len(run.Receipts), len(svc.order)-1)
	}
	for _, receipt := range run.Receipts {
		if receipt.ShardID == faultShard {
			t.Fatal("failed shard must not be atomically committed")
		}
	}

	resume, err := svc.Resume(ctx, RunRequest{MaxParallelism: 2, FrameQueueBudget: 128})
	if err != nil {
		t.Fatal(err)
	}
	foundRecompute := false
	for _, id := range resume.Recomputed {
		if id == faultShard {
			foundRecompute = true
		}
	}
	if !foundRecompute {
		t.Fatalf("fault shard should be recomputed on resume: %v", resume.Recomputed)
	}
	full, _, err := svc.FullRecompute()
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareDispatchSequences(full, resume.Records); err != nil {
		t.Fatal(err)
	}
}
