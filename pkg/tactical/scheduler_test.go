package tactical_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"tacticalworkbench/pkg/tactical"
)

func TestSchedulerParallelAndSerialCommitSameOrder(t *testing.T) {
	windows := make([]tactical.WindowSpec, 10)
	for i := range windows {
		windows[i] = tactical.WindowSpec{
			Index: i, MemberIDs: []string{fmt.Sprintf("M-%d", i)},
			StartTick: i * 10, EndTick: i*10 + 9, Label: fmt.Sprintf("window-%d", i),
		}
	}
	run := func(parallel, buffer int) ([]string, []string, tactical.SchedulerStats, int64) {
		scheduler, err := tactical.NewWindowScheduler(parallel, buffer)
		if err != nil {
			t.Fatal(err)
		}
		var commits []string
		var concurrent atomic.Int64
		var maxConcurrent atomic.Int64
		results, stats, err := tactical.RunWindows(
			context.Background(), scheduler, windows,
			func(ctx context.Context, window tactical.WindowSpec, _ *tactical.StateBuffer) (string, error) {
				time.Sleep(time.Duration((9-window.Index)%4) * time.Millisecond)
				return fmt.Sprintf("result-%d", window.Index), nil
			},
			func(ctx context.Context, window tactical.WindowSpec, value string) error {
				current := concurrent.Add(1)
				for {
					previous := maxConcurrent.Load()
					if current <= previous || maxConcurrent.CompareAndSwap(previous, current) {
						break
					}
				}
				time.Sleep(time.Millisecond)
				commits = append(commits, value)
				concurrent.Add(-1)
				return nil
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		return results, commits, stats, maxConcurrent.Load()
	}
	parallelResults, parallelCommits, parallelStats, maxCommitConcurrency := run(4, 2)
	serialResults, serialCommits, _, _ := run(1, 2)
	if maxCommitConcurrency != 1 {
		t.Fatalf("commit callback ran concurrently %d times", maxCommitConcurrency)
	}
	if parallelStats.PeakParallel < 2 {
		t.Fatalf("expected parallel workers, got stats %+v", parallelStats)
	}
	if parallelStats.PeakParallel > 4 {
		t.Fatalf("parallelism exceeded budget: %+v", parallelStats)
	}
	if fmt.Sprint(parallelResults) != fmt.Sprint(serialResults) {
		t.Fatalf("parallel and serial results differ:\nparallel=%v\nserial=%v", parallelResults, serialResults)
	}
	if fmt.Sprint(parallelCommits) != fmt.Sprint(serialCommits) {
		t.Fatalf("parallel and serial commit order differ:\nparallel=%v\nserial=%v", parallelCommits, serialCommits)
	}
}

func TestStateBufferBlocksInsteadOfDropping(t *testing.T) {
	buffer, err := tactical.NewStateBuffer(2)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := buffer.Push(context.Background(), tactical.TickState{Tick: i}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	err = buffer.Push(ctx, tactical.TickState{Tick: 2})
	if err == nil {
		t.Fatal("expected buffer push to block until timeout")
	}
	if buffer.Len() != 2 {
		t.Fatalf("buffer dropped or admitted states unexpectedly: len=%d", buffer.Len())
	}
	if buffer.BlockedPushes() == 0 {
		t.Fatal("expected blocked push to be counted")
	}
	if _, ok := buffer.Pop(); !ok {
		t.Fatal("expected buffered state")
	}
	if err := buffer.Push(context.Background(), tactical.TickState{Tick: 2}); err != nil {
		t.Fatalf("push after drain failed: %v", err)
	}
}

func TestSchedulerRejectsInvalidWindowOrder(t *testing.T) {
	scheduler, _ := tactical.NewWindowScheduler(2, 1)
	_, _, err := tactical.RunWindows(
		context.Background(), scheduler,
		[]tactical.WindowSpec{{Index: 1}, {Index: 0}},
		func(context.Context, tactical.WindowSpec, *tactical.StateBuffer) (int, error) { return 0, nil },
		func(context.Context, tactical.WindowSpec, int) error { return nil },
	)
	if err == nil {
		t.Fatal("expected invalid window order to fail")
	}
	if err := tactical.StableCommitOrder([]tactical.WindowSpec{{Index: 0}}, []int{0}); err != nil {
		t.Fatal(err)
	}
}
