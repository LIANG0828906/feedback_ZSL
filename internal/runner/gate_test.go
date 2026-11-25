package runner

import (
	"context"
	"testing"
	"time"
)

func TestGatePauseAndResume(t *testing.T) {
	g := NewGate()
	g.Pause()
	done := make(chan error, 1)
	go func() {
		done <- g.Check(context.Background())
	}()
	select {
	case err := <-done:
		t.Fatalf("check returned while paused: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	g.Resume()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("resume did not release gate")
	}
}
