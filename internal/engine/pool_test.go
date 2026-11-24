package engine

import (
	"context"
	"testing"
	"time"

	"fixedsampler/internal/model"
)

func TestWorkerPoolBufferFullBlocksAndRecords(t *testing.T) {
	pool := NewWorkerPool(1, 1)
	started := make(chan struct{})
	release := make(chan struct{})

	d0, err := pool.Submit(context.Background(), 0, 1, 0, 0, func() (*model.LevelArtifact, error) {
		close(started)
		<-release
		return &model.LevelArtifact{Seq: 1}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started

	d1, err := pool.Submit(context.Background(), 1, 1, 1, 0, func() (*model.LevelArtifact, error) {
		return &model.LevelArtifact{Seq: 1}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	type submitResult struct {
		done <-chan nodeResult
		err  error
	}
	third := make(chan submitResult, 1)
	go func() {
		done, err := pool.Submit(context.Background(), 2, 1, 2, 0, func() (*model.LevelArtifact, error) {
			return &model.LevelArtifact{Seq: 1}, nil
		})
		third <- submitResult{done: done, err: err}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(pool.Events()) == 0 {
		time.Sleep(time.Millisecond)
	}
	events := pool.Events()
	if len(events) == 0 || events[0].Reason == "" {
		t.Fatalf("expected buffer-full queue event, got %+v", events)
	}

	close(release)
	if res := <-d0; res.err != nil {
		t.Fatal(res.err)
	}
	if res := <-d1; res.err != nil {
		t.Fatal(res.err)
	}
	select {
	case sr := <-third:
		if sr.err != nil {
			t.Fatal(sr.err)
		}
		if res := <-sr.done; res.err != nil {
			t.Fatal(res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked submit did not proceed after release")
	}
	pool.Close()
}
