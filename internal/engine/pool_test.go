package engine

import (
	"testing"
	"time"

	"combatjudge/internal/model"
)

func TestBoundedPoolRecordsQueueWait(t *testing.T) {
	pool := NewPool(1, 1, DefaultBufferBytes)
	entered := make(chan struct{})
	release := make(chan struct{})

	f1 := pool.SubmitGeometry(GeometryJob{IntervalNo: 1, Run: func(buf *GeomBuffer) (*GeometryResult, error) {
		close(entered)
		<-release
		return &GeometryResult{IntervalNo: 1}, nil
	}})
	<-entered

	f2 := pool.SubmitGeometry(GeometryJob{IntervalNo: 2, Run: func(buf *GeomBuffer) (*GeometryResult, error) {
		return &GeometryResult{IntervalNo: 2}, nil
	}})

	f3Ready := make(chan *Future, 1)
	go func() {
		f3Ready <- pool.SubmitGeometry(GeometryJob{IntervalNo: 3, Run: func(buf *GeomBuffer) (*GeometryResult, error) {
			return &GeometryResult{IntervalNo: 3}, nil
		}})
	}()
	time.Sleep(20 * time.Millisecond)
	close(release)

	if _, err := f1.Get(); err != nil {
		t.Fatal(err)
	}
	if _, err := f2.Get(); err != nil {
		t.Fatal(err)
	}
	f3 := <-f3Ready
	if _, err := f3.Get(); err != nil {
		t.Fatal(err)
	}
	pool.Close()

	waits := pool.QueueWaits()
	found := false
	for _, w := range waits {
		if w.IntervalNo == 3 && w.Reason == "queue_budget_exhausted" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing queue budget wait for interval 3: %+v", waits)
	}
}

func TestGeometryBufferLimitReturnsBudgetCode(t *testing.T) {
	buf := NewGeomBuffer(8)
	err := buf.Resize(2)
	if err == nil {
		t.Fatal("expected buffer budget failure")
	}
	var coded *model.Error
	if !asError(err, &coded) || coded.Code != model.CodeBudget {
		t.Fatalf("err=%v want E_BUDGET", err)
	}
}

func asError(err error, target **model.Error) bool {
	var me *model.Error
	if e, ok := err.(*model.Error); ok {
		me = e
	}
	*target = me
	return me != nil
}
