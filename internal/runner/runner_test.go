package runner_test

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"semanticshading/internal/apierr"
	"semanticshading/internal/checkpoint"
	"semanticshading/internal/model"
	"semanticshading/internal/runner"
	"semanticshading/internal/sample"
)

func openTestStore(t *testing.T, name string, s *sample.Sample) *checkpoint.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".sqlite")
	store, _, err := checkpoint.Open(path, runner.IdentityFor(s, s.Truth.TruthFingerprint))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestSerialAndParallelBlockRecordsEqual(t *testing.T) {
	s, err := sample.Generate(sample.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	serialStore := openTestStore(t, "serial", s)
	parallelStore := openTestStore(t, "parallel", s)

	serial, err := runner.Run(ctx, s, serialStore, runner.Config{Parallelism: 1, QueueBudget: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	parallel, err := runner.Run(ctx, s, parallelStore, runner.Config{Parallelism: 4, QueueBudget: 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(serial.CommitSequence, parallel.CommitSequence) {
		t.Fatalf("sequences differ: %v vs %v", serial.CommitSequence, parallel.CommitSequence)
	}
	serialRecords, err := serialStore.LoadBlockRecords()
	if err != nil {
		t.Fatal(err)
	}
	parallelRecords, err := parallelStore.LoadBlockRecords()
	if err != nil {
		t.Fatal(err)
	}
	for i := range serialRecords {
		if !reflect.DeepEqual(serialRecords[i], parallelRecords[i]) {
			t.Fatalf("block %d differs between serial and parallel", i)
		}
	}
}

func TestResourceConfigOutOfRange(t *testing.T) {
	s, _ := sample.Generate(sample.DefaultConfig())
	_, _, err := runner.ValidateConfig(s.Manifest, runner.Config{Parallelism: 128})
	ae, ok := apierr.As(err)
	if !ok || ae.Code != apierr.EPara || ae.Field != "parallelism" {
		t.Fatalf("parallelism error = %#v", err)
	}
	_, _, err = runner.ValidateConfig(s.Manifest, runner.Config{QueueBudget: -1})
	ae, ok = apierr.As(err)
	if !ok || ae.Code != apierr.EPara || ae.Field != "queue_budget" {
		t.Fatalf("queue error = %#v", err)
	}
	_, _, err = runner.ValidateConfig(s.Manifest, runner.Config{ThreadPixelBufferLimit: 1})
	ae, ok = apierr.As(err)
	if !ok || ae.Code != apierr.EPara || ae.Field != "thread_pixel_buffer_limit" {
		t.Fatalf("buffer error = %#v", err)
	}
}

func TestQueueBudgetExhaustedRecordsWait(t *testing.T) {
	s, _ := sample.Generate(sample.DefaultConfig())
	store := openTestStore(t, "queue", s)
	entered := make(chan int, 1)
	release := make(chan struct{})
	full := make(chan int, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan struct {
		report *runner.Report
		err    error
	}, 1)
	go func() {
		report, err := runner.Run(ctx, s, store, runner.Config{
			Parallelism: 1,
			QueueBudget: 1,
			BeforeEvaluate: func(index int) {
				if index == 0 {
					entered <- index
					<-release
				}
			},
			OnQueueFull: func(index int) {
				select {
				case full <- index:
				default:
				}
			},
		}, nil)
		done <- struct {
			report *runner.Report
			err    error
		}{report, err}
	}()

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	select {
	case <-full:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("queue budget exhaustion was not observed")
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	foundExhausted := false
	foundLongWait := false
	for _, wait := range result.report.QueueWaits {
		if wait.Reason == "queue_budget_exhausted" {
			foundExhausted = true
			if wait.WaitNanos >= int64(10*time.Millisecond) {
				foundLongWait = true
			}
		}
	}
	if !foundExhausted {
		t.Fatalf("no exhausted queue wait recorded: %#v", result.report.QueueWaits)
	}
	if !foundLongWait {
		t.Fatalf("no queue wait covered the blocked worker window: %#v", result.report.QueueWaits)
	}
}

func TestStageFailureRetryDoesNotPolluteCommittedBlocks(t *testing.T) {
	s, _ := sample.Generate(sample.DefaultConfig())
	store := openTestStore(t, "stage", s)
	policy := &runner.FailurePolicy{FailTimes: map[int]int{4: 1}}
	firstReport, err := runner.Run(context.Background(), s, store, runner.Config{Parallelism: 2, QueueBudget: 2, FailurePolicy: policy}, nil)
	ae, ok := apierr.As(err)
	if !ok || ae.Code != apierr.EStage {
		t.Fatalf("first run error = %#v", err)
	}
	if len(firstReport.FailedBlocks) != 1 || firstReport.FailedBlocks[0].BlockIndex != 4 {
		t.Fatalf("failed blocks = %#v", firstReport.FailedBlocks)
	}
	if firstReport.CommittedCount != 4 {
		t.Fatalf("committed before failed gap = %d, want 4", firstReport.CommittedCount)
	}
	retryReport, err := runner.Run(context.Background(), s, store, runner.Config{Parallelism: 1, QueueBudget: 1, FailurePolicy: policy}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(retryReport.CommitSequence) != s.Manifest.BlockCount() {
		t.Fatalf("sequence = %v", retryReport.CommitSequence)
	}
}

func TestStateMachineIllegalTransitionKeepsState(t *testing.T) {
	s, _ := sample.Generate(sample.DefaultConfig())
	machine := runner.NewMachine(s.Manifest.Block(0))
	err := machine.Transition(runner.StateCommitted)
	ae, ok := apierr.As(err)
	if !ok || ae.Code != apierr.EState {
		t.Fatalf("err = %#v", err)
	}
	if machine.State().State != runner.StatePending {
		t.Fatalf("state changed to %s after illegal transition", machine.State().State)
	}
}

var _ = model.FixedScale
