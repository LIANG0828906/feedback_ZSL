package pool

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPoolLimitsMaximumParallelism(t *testing.T) {
	p := New(2, 1, nil)
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	var completed atomic.Int32

	task := func(ctx context.Context, buffer *Buffer) error {
		started <- struct{}{}
		<-release
		completed.Add(1)
		return nil
	}

	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() { errs <- p.Execute(context.Background(), "s", task) }()
	}

	waitActive(t, p, 2)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("workers did not start")
	}
	close(release)
	deadline := time.After(2 * time.Second)
	for i := 0; i < 4; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatal(err)
			}
		case <-deadline:
			t.Fatal("tasks did not finish")
		}
	}
	if p.Peak() > 2 {
		t.Fatalf("parallelism exceeded limit: peak=%d", p.Peak())
	}
	if completed.Load() != 4 {
		t.Fatalf("expected all tasks to complete, got %d", completed.Load())
	}
}

func TestFullStateBufferBlocksInsteadOfDroppingRecords(t *testing.T) {
	var written atomic.Int32
	firstWrite := make(chan struct{})
	releaseFirstWrite := make(chan struct{})
	var firstOnce sync.Once
	writer := func(record StateRecord) error {
		firstOnce.Do(func() {
			close(firstWrite)
			<-releaseFirstWrite
		})
		written.Add(1)
		return nil
	}
	p := New(1, 1, writer)

	executeErr := make(chan error, 1)
	thirdPushed := make(chan struct{})
	releaseThirdPushCheck := make(chan struct{})
	go func() {
		executeErr <- p.Execute(context.Background(), "buffer-session", func(ctx context.Context, buffer *Buffer) error {
			if err := buffer.Push(StateRecord{Payload: "one"}); err != nil {
				return err
			}
			if err := buffer.Push(StateRecord{Payload: "two"}); err != nil {
				return err
			}
			go func() {
				_ = buffer.Push(StateRecord{Payload: "three"})
				close(thirdPushed)
			}()
			<-releaseThirdPushCheck
			return nil
		})
	}()

	select {
	case <-firstWrite:
	case <-time.After(time.Second):
		t.Fatal("writer did not receive first record")
	}
	time.Sleep(50 * time.Millisecond)
	select {
	case <-thirdPushed:
		t.Fatal("third push succeeded while per-thread buffer was full")
	default:
	}
	close(releaseFirstWrite)
	select {
	case <-thirdPushed:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked third record was not drained after writer resumed")
	}
	close(releaseThirdPushCheck)

	select {
	case err := <-executeErr:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked buffer did not drain")
	}
	if written.Load() != 3 {
		t.Fatalf("expected all three records to be written, got %d", written.Load())
	}
}

func waitActive(t *testing.T, p *Pool, expected int32) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if p.Active() == expected {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("active workers = %d, want %d", p.Active(), expected)
}
