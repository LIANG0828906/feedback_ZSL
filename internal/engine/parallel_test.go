package engine_test

import (
	"context"
	"testing"

	"mvccworkbench/internal/domain"
	"mvccworkbench/internal/engine"
)

func TestParallelResultsMatchTruthAcrossWorkerBudgets(t *testing.T) {
	suite := testSuite(t)
	for _, workers := range []int{1, 2, 4, 8} {
		for _, batchID := range suite.Order {
			batch := suite.Batches[batchID]
			name := batchID + "/workers=" + itoa(workers)
			t.Run(name, func(t *testing.T) {
				report, err := engine.RunParallel(context.Background(), batch.Project, engine.Options{
					Workers:         workers,
					ItemBufferLimit: 128,
					ChainCap:        1024,
				})
				if err != nil {
					t.Fatal(err)
				}
				assertReadsMatch(t, batch.Truth, report)
				assertWritesMatch(t, batch.Truth, report)
				assertCommitsMatch(t, batch.Truth, report)
				assertGCMatch(t, batch.Truth, report)
				if len(report.Metrics) != len(batch.Project.Items) {
					t.Fatalf("metrics items=%d want %d", len(report.Metrics), len(batch.Project.Items))
				}
				for _, item := range batch.Project.Items {
					if _, ok := report.Metrics[item.ID]; !ok {
						t.Fatalf("missing elapsed observation for %s", item.ID)
					}
				}
			})
		}
	}
}

func TestParallelChainCapReturnsStableBackpressure(t *testing.T) {
	suite := testSuite(t)
	batch := suite.Batches["B1-visibility"]
	_, err := engine.RunParallel(context.Background(), batch.Project, engine.Options{
		Workers: 1, ItemBufferLimit: 4, ChainCap: 1,
	})
	if domain.CodeOf(err) != domain.EBackpressureChainCap {
		t.Fatalf("code=%s err=%v", domain.CodeOf(err), err)
	}
}

func TestSchedulerExhaustionCodesAreStable(t *testing.T) {
	scheduler := engine.NewScheduler(1, 1)
	release, err := scheduler.TryAcquireWorker()
	if err != nil {
		t.Fatal(err)
	}
	_, err = scheduler.TryAcquireWorker()
	if domain.CodeOf(err) != domain.EBackpressureWorkers {
		t.Fatalf("worker code=%s err=%v", domain.CodeOf(err), err)
	}
	release()

	noop := func(context.Context) {}
	if err := scheduler.Submit("alpha", noop); err != nil {
		t.Fatal(err)
	}
	err = scheduler.Submit("alpha", noop)
	if domain.CodeOf(err) != domain.EBackpressureItemBuf {
		t.Fatalf("buffer code=%s err=%v", domain.CodeOf(err), err)
	}
	if scheduler.Pending("alpha") != 1 {
		t.Fatalf("pending=%d want 1", scheduler.Pending("alpha"))
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	negative := v < 0
	if negative {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
