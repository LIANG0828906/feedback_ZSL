package pool_test

import (
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gameaudio/workbench/internal/model"
	"github.com/gameaudio/workbench/internal/pool"
)

func TestPoolProcessesAllJobs(t *testing.T) {
	var computed int32
	pl := pool.New(pool.Config{MaxWorkers: 4, PerWorkerBuffer: 2},
		func(sourceID, listenerID string) model.Result {
			atomic.AddInt32(&computed, 1)
			return model.Result{SourceID: sourceID, ListenerID: listenerID}
		})
	const n = 100
	go func() {
		for i := 0; i < n; i++ {
			pl.Submit(pool.Job{SourceID: "S", ListenerID: "L"})
		}
		pl.CloseAndWait()
	}()
	got := 0
	for range pl.Result() {
		got++
	}
	if got != n || atomic.LoadInt32(&computed) != n {
		t.Fatalf("processed=%d computed=%d want %d (no silent drop)", got, computed, n)
	}
}

func TestBackpressureRecordedWhenBufferSaturated(t *testing.T) {
	block := make(chan struct{})
	// 容量 = MaxWorkers * PerWorkerBuffer = 1，提交超过 1 个任务必然打满。
	pl := pool.New(pool.Config{MaxWorkers: 1, PerWorkerBuffer: 1},
		func(sourceID, listenerID string) model.Result {
			<-block // 人为占满唯一工作线程，迫使提交方阻塞排队
			return model.Result{}
		})
	const total = 4
	consumed := make(chan struct{})
	go func() {
		for i := 0; i < total; i++ {
			<-pl.Result()
		}
		close(consumed)
	}()
	producerDone := make(chan struct{})
	firstAccepted := make(chan struct{})
	go func() {
		pl.Submit(pool.Job{SourceID: "S", ListenerID: "L"})
		close(firstAccepted)
		for i := 1; i < total; i++ {
			pl.Submit(pool.Job{SourceID: "S", ListenerID: "L"})
		}
		close(producerDone)
	}()
	<-firstAccepted
	// 第二个任务起缓冲必然饱和，轮询等待背压事件被记录。
	deadline := time.Now().Add(2 * time.Second)
	for len(pl.Backpressure()) == 0 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	close(block)
	<-producerDone
	pl.CloseAndWait()
	<-consumed
	if len(pl.Backpressure()) == 0 {
		t.Fatal("buffer saturation must record backpressure reason, not drop jobs")
	}
	if pl.Backpressure()[0].Reason == "" {
		t.Fatal("backpressure event must carry a reason")
	}
}
