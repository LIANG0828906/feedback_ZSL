package schedule

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"lip-pipeline-prototype/internal/domain"
)

func TestCandidateBufferFullBlocksAndRecordsWait(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	opts := Options{MaxParallelism: 1, CandidateBufferPerWorker: 1, FrameQueueBudget: 16}
	candidates := []Candidate{
		{ID: "c1", ShardID: "s1", FrameCount: 1, Work: func(ctx context.Context, emit func(domain.DispatchRecord)) error {
			started <- struct{}{}
			<-release
			return nil
		}},
		{ID: "c2", ShardID: "s1", FrameCount: 1, Work: func(ctx context.Context, emit func(domain.DispatchRecord)) error { return nil }},
		{ID: "c3", ShardID: "s1", FrameCount: 1, Work: func(ctx context.Context, emit func(domain.DispatchRecord)) error { return nil }},
	}

	done := make(chan *Report, 1)
	go func() {
		report, err := Run(context.Background(), candidates, opts)
		if err != nil {
			t.Error(err)
		}
		done <- report
	}()
	<-started
	time.Sleep(50 * time.Millisecond)
	close(release)

	select {
	case report := <-done:
		if len(report.Outcomes) != 3 {
			t.Fatalf("outcomes=%d", len(report.Outcomes))
		}
		found := false
		for _, wait := range report.Waits {
			if wait.Kind == "candidate_buffer" && wait.CandidateID == "c3" && wait.WaitedMicros > 0 {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing blocking candidate wait: %+v", report.Waits)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not finish after releasing worker")
	}
}

func TestFrameQueueBackpressureDoesNotDrop(t *testing.T) {
	rec := &Recorder{}
	queue := NewFrameQueue(1, rec)
	ctx := context.Background()
	if err := queue.Send(ctx, domain.DispatchRecord{FrameIndex: 1}, 0); err != nil {
		t.Fatal(err)
	}
	sendDone := make(chan error, 1)
	go func() {
		sendDone <- queue.Send(ctx, domain.DispatchRecord{FrameIndex: 2}, 0)
	}()
	time.Sleep(30 * time.Millisecond)
	record, ok, err := queue.Next(ctx)
	if err != nil || !ok || record.FrameIndex != 1 {
		t.Fatalf("next = %+v %v %v", record, ok, err)
	}
	select {
	case err := <-sendDone:
		t.Fatalf("second send should remain blocked until budget slot drains: %v", err)
	default:
	}
	record, ok, err = queue.Next(ctx)
	if err != nil || !ok || record.FrameIndex != 2 {
		t.Fatalf("drain second = %+v %v %v", record, ok, err)
	}
	if err := <-sendDone; err != nil {
		t.Fatal(err)
	}
	found := false
	for _, wait := range rec.Waits() {
		if wait.Kind == "frame_queue" && wait.WaitedMicros > 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing frame queue wait: %+v", rec.Waits())
	}
}

func TestChannelSlotsLimitParallelism(t *testing.T) {
	var active atomic.Int32
	var maxActive atomic.Int32
	opts := Options{
		MaxParallelism: 4, CandidateBufferPerWorker: 8, FrameQueueBudget: 64,
		ChannelSlots: map[string]int{"limited": 1},
	}
	candidates := make([]Candidate, 6)
	for i := range candidates {
		candidates[i] = Candidate{
			ID: "c", ChannelID: "limited", FrameCount: 1,
			Work: func(ctx context.Context, emit func(domain.DispatchRecord)) error {
				current := active.Add(1)
				for {
					max := maxActive.Load()
					if current <= max || maxActive.CompareAndSwap(max, current) {
						break
					}
				}
				time.Sleep(10 * time.Millisecond)
				active.Add(-1)
				return nil
			},
		}
	}
	report, err := Run(context.Background(), candidates, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Outcomes) != 6 {
		t.Fatalf("outcomes=%d", len(report.Outcomes))
	}
	if got := maxActive.Load(); got != 1 {
		t.Fatalf("channel slot allowed max active=%d, want 1", got)
	}
}

func TestCandidateOverBudgetReturnsEVal(t *testing.T) {
	opts := DefaultOptions()
	opts.FrameQueueBudget = 2
	report, err := Run(context.Background(), []Candidate{{
		ID: "big", ShardIndex: 3, FrameCount: 3,
		Work: func(ctx context.Context, emit func(domain.DispatchRecord)) error { return nil },
	}}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Outcomes) != 1 || report.Outcomes[0].ErrorCode != domain.CodeVal {
		t.Fatalf("expected E_VAL outcome, %+v", report.Outcomes)
	}
}
