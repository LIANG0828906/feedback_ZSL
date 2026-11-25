package parallel_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"lipsync/parallel"
)

func TestBoundedPoolQueuesAndChunksWithoutDropping(t *testing.T) {
	pool, err := parallel.New(parallel.Config{
		MaxWorkers: 2, QueueBudget: 1, CandidateBuffer: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var executed int64
	var chunked int64
	works := make([]parallel.Work, 0, 12)
	for i := 0; i < 12; i++ {
		works = append(works, parallel.Work{
			ID: "channel-" + string(rune('a'+i)), CandidateCount: 5,
			Run: func(_ context.Context, from, to int, last bool) (any, error) {
				time.Sleep(time.Millisecond)
				atomic.AddInt64(&executed, int64(to-from))
				if last {
					atomic.AddInt64(&chunked, 1)
				}
				if last {
					return "ok", nil
				}
				return nil, nil
			},
		})
	}
	outcomes, stats, err := pool.Execute(context.Background(), works)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != len(works) {
		t.Fatalf("dropped outcomes: %d != %d", len(outcomes), len(works))
	}
	for _, outcome := range outcomes {
		if outcome.Err != nil || outcome.Value != "ok" {
			t.Fatalf("work %s lost result: %+v", outcome.ID, outcome)
		}
		if outcome.Chunks != 3 {
			t.Fatalf("candidate buffer chunking not applied: %+v", outcome)
		}
	}
	if executed != int64(len(works)*5) {
		t.Fatalf("candidate chunks skipped work: %d", executed)
	}
	if stats.MaxRunning > 2 {
		t.Fatalf("worker cap exceeded: %d", stats.MaxRunning)
	}
	if stats.QueueWaits == 0 {
		t.Fatal("queue backpressure was not recorded")
	}
	if stats.CandidateWaits == 0 {
		t.Fatal("candidate buffer backpressure was not recorded")
	}
	if len(pool.WaitEvents()) == 0 {
		t.Fatal("wait evidence is empty")
	}
}
