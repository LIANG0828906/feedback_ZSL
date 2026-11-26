package scheduler_test

import (
	"context"
	"testing"
	"time"

	"wardrobekernel/internal/model"
	"wardrobekernel/internal/samplegen"
	"wardrobekernel/internal/scheduler"
)

func TestBoundedSchedulerRecoversTransientAndIsolatesPersistentFailure(t *testing.T) {
	project, err := samplegen.Generate(model.FixedSeed)
	if err != nil {
		t.Fatal(err)
	}
	ids := project.CandidateAssembly
	result, err := scheduler.Prepare(context.Background(), project, ids, scheduler.Options{
		MaxParallelism: 2, CandidateBufferPerWorker: 1, PendingQueueBudget: 2,
		WorkDelay:        2 * time.Millisecond,
		FailFirstAttempt: map[string]bool{"waist_belt": true},
		PersistentFail:   map[string]bool{"ghost_failure": true},
	})
	if err != nil {
		t.Fatal(err)
	}

	foundRecovered := false
	for _, failure := range result.Failures {
		if failure.PartID == "waist_belt" && failure.Recovered && failure.Retryable {
			foundRecovered = true
		}
	}
	if !foundRecovered {
		t.Fatalf("expected recovered waist_belt failure, got %#v", result.Failures)
	}

	// ghost_failure is not a project part, so inject persistent failure on a
	// real losing part in a second run and verify all other parts still prepare.
	result2, err := scheduler.Prepare(context.Background(), project, ids, scheduler.Options{
		MaxParallelism: 1, CandidateBufferPerWorker: 1, PendingQueueBudget: 1,
		PersistentFail: map[string]bool{"ring_left": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result2.Prepared) != len(ids)-1 {
		t.Fatalf("expected only failed part omitted, prepared=%d total=%d failures=%#v", len(result2.Prepared), len(ids), result2.Failures)
	}
	for _, p := range result2.Prepared {
		if p.Part.ID == "ring_left" {
			t.Fatal("persistent failed part must not be prepared")
		}
	}
	foundFinalFailure := false
	for _, f := range result2.Failures {
		if f.PartID == "ring_left" && !f.Retryable {
			foundFinalFailure = true
		}
	}
	if !foundFinalFailure {
		t.Fatalf("expected final non-retryable failure: %#v", result2.Failures)
	}
}

func TestBlockingWaitIsRecorded(t *testing.T) {
	project, err := samplegen.Generate(model.FixedSeed)
	if err != nil {
		t.Fatal(err)
	}
	// One worker and tiny buffers with a long delay deterministically saturate.
	result, err := scheduler.Prepare(context.Background(), project, project.CandidateAssembly[:6], scheduler.Options{
		MaxParallelism: 1, CandidateBufferPerWorker: 1, PendingQueueBudget: 1,
		WorkDelay: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Waits) == 0 {
		t.Fatal("expected at least one blocking wait record")
	}
	var maxNS int64
	for _, w := range result.Waits {
		if w.DurationNS > maxNS {
			maxNS = w.DurationNS
		}
		if w.Capacity <= 0 || w.Reason == "" {
			t.Fatalf("incomplete wait record %#v", w)
		}
	}
	if maxNS < int64(10*time.Millisecond) {
		t.Fatalf("expected a real blocking wait, max_ns=%d", maxNS)
	}
}
