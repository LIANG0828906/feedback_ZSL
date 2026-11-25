package kernel

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBoundedPoolQueuesWhenExhausted(t *testing.T) {
	pool := NewBoundedPool(1, 4096)
	started := make(chan string, 2)
	release := make(chan struct{})
	jobs := []TileJob{
		{TileID: "tile_a", Run: func([]byte) (*TileResult, error) {
			started <- "tile_a"
			<-release
			return &TileResult{TileID: "tile_a"}, nil
		}},
		{TileID: "tile_b", Run: func([]byte) (*TileResult, error) {
			started <- "tile_b"
			<-release
			return &TileResult{TileID: "tile_b"}, nil
		}},
	}

	done := make(chan []TileJobResult, 1)
	go func() { done <- pool.Run(context.Background(), jobs) }()

	first := <-started
	if first != "tile_a" && first != "tile_b" {
		t.Fatalf("unexpected first start=%q", first)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		events := pool.AuditEvents()
		if len(events) > 0 && events[len(events)-1].Event == "queued" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	foundQueued := false
	for _, event := range pool.AuditEvents() {
		other := "tile_b"
		if first == "tile_b" {
			other = "tile_a"
		}
		if event.Event == "queued" && event.Reason == "worker_pool_exhausted" && event.TileID == other {
			foundQueued = true
		}
		if event.ActiveWorkers > int32(pool.MaxWorkers) {
			t.Fatalf("parallelism exceeded: %+v", event)
		}
	}
	if !foundQueued {
		t.Fatalf("missing queue exhaustion audit: %+v", pool.AuditEvents())
	}
	close(release)
	results := <-done
	if len(results) != 2 || results[0].Err != nil || results[1].Err != nil {
		t.Fatalf("pool results=%+v", results)
	}
}

func TestBoundedPoolGeometryBufferLimit(t *testing.T) {
	pool := NewBoundedPool(2, 16)
	results := pool.Run(context.Background(), []TileJob{{
		TileID:        "tx_bad",
		EstimateBytes: 17,
		Run:           func([]byte) (*TileResult, error) { return &TileResult{}, nil },
	}})
	if len(results) != 1 {
		t.Fatalf("results=%d", len(results))
	}
	var kernelErr *Error
	if !errors.As(results[0].Err, &kernelErr) || kernelErr.Code != CodeBlock {
		t.Fatalf("error=%v want E_BLOCK", results[0].Err)
	}
}
