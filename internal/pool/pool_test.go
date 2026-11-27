package pool

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestBoundedPoolOrderAndIsolation(t *testing.T) {
	p := New(2, 1) // 总缓冲 2，制造阻塞排队
	var ok int64
	for i := 0; i < 8; i++ {
		idx := i
		p.Submit(Job{
			ID: "j",
			Apply: func() error {
				time.Sleep(2 * time.Millisecond)
				if idx == 3 {
					return errors.New("boom")
				}
				atomic.AddInt64(&ok, 1)
				return nil
			},
		})
	}
	results := p.CloseAndWait()
	if len(results) != 8 {
		t.Fatalf("results=%d want 8", len(results))
	}
	// 按提交序号稳定排序。
	for i := 1; i < len(results); i++ {
		if results[i].Seq <= results[i-1].Seq {
			t.Fatal("results not in submission order")
		}
	}
	failures := 0
	buffered := 0
	for _, r := range results {
		if r.Err != "" {
			failures++
		}
		if r.QueueReason == "per_worker_buffer_full" {
			buffered++
		}
	}
	if failures != 1 {
		t.Fatalf("single task failure should be isolated, got %d", failures)
	}
	if ok != 7 {
		t.Fatalf("successful jobs=%d want 7", ok)
	}
	if buffered == 0 {
		t.Fatal("expected at least one buffer-full queue event")
	}
}
