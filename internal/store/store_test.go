package store_test

import (
	"fmt"
	"sync"
	"testing"

	"replayaudit/internal/model"
	"replayaudit/internal/store"
)

func TestConcurrentCommitsAreSerialAndIntact(t *testing.T) {
	st, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	const n = 40
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			meta := store.Meta{
				ReplayID: "r1", ContentHash: "h", FormatVersion: model.FormatVersion,
				ParamsVersion: "pv", AlgoVersion: model.AlgoVersion,
			}
			rec := store.SegmentRecord{
				SegmentCommit: model.SegmentCommit{
					Segment: i, CommitSeq: i, SegmentHash: fmt.Sprintf("seg-%d", i),
					StateHash: "state", EventIDs: []string{fmt.Sprintf("e%d", i)},
				},
			}
			if err := st.CommitSegment(meta, rec); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	recs, meta, err := st.LoadSegments("r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != n {
		t.Fatalf("stored %d/%d segments", len(recs), n)
	}
	if meta == nil || meta.SegmentCount != n {
		t.Fatalf("bad meta: %+v", meta)
	}
	for i, rec := range recs {
		if rec.Segment != i || rec.SegmentHash != fmt.Sprintf("seg-%d", i) {
			t.Fatalf("order/payload broken at %d: %+v", i, rec.SegmentCommit)
		}
	}
}

func TestCheckCompatibleLifecycle(t *testing.T) {
	st, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ok, err := st.CheckCompatible("r", "h", "fv", "pv", "av")
	if err != nil || ok {
		t.Fatalf("empty store should be incompatible: ok=%v err=%v", ok, err)
	}
	meta := store.Meta{ReplayID: "r", ContentHash: "h", FormatVersion: "fv", ParamsVersion: "pv", AlgoVersion: "av"}
	if err := st.CommitSegment(meta, store.SegmentRecord{
		SegmentCommit: model.SegmentCommit{Segment: 0, CommitSeq: 0},
	}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.CheckCompatible("r", "h", "fv", "pv", "av"); !ok {
		t.Fatal("matching envelope should be compatible")
	}
	if ok, _ := st.CheckCompatible("r", "h2", "fv", "pv", "av"); ok {
		t.Fatal("content hash mismatch must be incompatible")
	}
	recs, _, _ := st.LoadSegments("r")
	if len(recs) != 0 {
		t.Fatal("mismatch must purge segments")
	}
}
