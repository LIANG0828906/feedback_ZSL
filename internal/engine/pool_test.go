package engine

import (
	"context"
	"testing"
	"time"

	"uvenhance/internal/apperr"
)

func TestBufferLimiterBlocksAndRecordsReason(t *testing.T) {
	limiter := newBufferLimiter(10)
	release, err := limiter.acquire(context.Background(), "first", 10)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = limiter.acquire(ctx, "second", 5)
	if apperr.Code(err) != apperr.CodeCanceled {
		t.Fatalf("expected blocked acquire to cancel, got %v", err)
	}
	events := limiter.snapshotEvents()
	if len(events) != 1 || events[0].Stage != "second" || events[0].Reason != "buffer_exhausted_waiting" {
		t.Fatalf("buffer wait not recorded: %+v", events)
	}
	release()
	release2, err := limiter.acquire(context.Background(), "third", 5)
	if err != nil {
		t.Fatal(err)
	}
	release2()
}
