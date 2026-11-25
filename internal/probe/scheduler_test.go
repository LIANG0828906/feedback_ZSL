package probe

import (
	"context"
	"testing"
	"time"
)

func TestRunBoundedJobsRecordsProbeBufferWait(t *testing.T) {
	cfg := Config{MaxParallelism: 1, ProbeBufferPerWorker: 1, SHBufferCoeffBudget: 27}
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	committed := make(chan string, 2)

	jobs := []Job{{ProbeID: "a", RoomID: "r", SHCost: 27}, {ProbeID: "b", RoomID: "r", SHCost: 27}}
	done := make(chan []QueueEvent, 1)
	go func() {
		events := runBoundedJobs(context.Background(), cfg, NewMetricsForTest(), jobs,
			func(_ context.Context, job Job) Result {
				if job.ProbeID == "a" {
					close(firstStarted)
					<-releaseFirst
				}
				return Result{ProbeID: job.ProbeID, RoomID: job.RoomID}
			},
			func(result Result) { committed <- result.ProbeID },
		)
		done <- events
	}()

	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first job did not start")
	}
	time.Sleep(25 * time.Millisecond)
	close(releaseFirst)

	select {
	case events := <-done:
		found := false
		for _, event := range events {
			if event.Resource == "probe_buffer" && event.ProbeID == "b" {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected probe buffer queue event, got %+v", events)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not finish")
	}

	if got := <-committed; got != "a" {
		t.Fatalf("first commit = %q, want a", got)
	}
	if got := <-committed; got != "b" {
		t.Fatalf("second commit = %q, want b", got)
	}
}

func TestRunBoundedJobsRecordsSHBudgetWait(t *testing.T) {
	cfg := Config{MaxParallelism: 2, ProbeBufferPerWorker: 2, SHBufferCoeffBudget: 27}
	releaseExecute := make(chan struct{})
	exhausted := make(chan string, 1)
	blockHook := make(chan struct{})
	cfg.beforeBudgetWait = func(resource, probeID string) {
		if resource == "sh_temp_buffer" {
			exhausted <- probeID
			<-blockHook
		}
	}
	jobs := []Job{{ProbeID: "a", SHCost: 27}, {ProbeID: "b", SHCost: 27}}
	done := make(chan []QueueEvent, 1)

	go func() {
		done <- runBoundedJobs(context.Background(), cfg, NewMetricsForTest(), jobs,
			func(_ context.Context, job Job) Result {
				<-releaseExecute
				return Result{ProbeID: job.ProbeID}
			},
			func(Result) {},
		)
	}()
	select {
	case <-exhausted:
	case <-time.After(time.Second):
		t.Fatal("second job did not exhaust SH budget")
	}
	close(blockHook)
	close(releaseExecute)

	select {
	case events := <-done:
		found := false
		for _, event := range events {
			if event.Resource == "sh_temp_buffer" && event.Reason == "sh_buffer_budget_exhausted" {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected SH budget queue event, got %+v", events)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SH budget test timed out")
	}
}

func NewMetricsForTest() *Metrics { return &Metrics{} }
