package dialoguecal

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
)

func TestDualStateMachinesRejectIllegalTransitionsWithoutDrift(t *testing.T) {
	g := mustGenerate(t, 404)
	sm, err := NewStateMachine(g.Project)
	if err != nil {
		t.Fatal(err)
	}
	intervals, err := BuildIntervals(g.Project)
	if err != nil {
		t.Fatal(err)
	}
	segKey := SegmentKey(intervals[0].DialogueID, intervals[0].SegmentID)
	if err := sm.TransitionSegment(segKey, SegmentExtracting); err != nil {
		t.Fatal(err)
	}
	before, _ := sm.Record(segKey, true)
	err = sm.TransitionSegment(segKey, SegmentCommitted)
	ke := requireCode(t, err, CodeState)
	if ke.From != string(SegmentExtracting) || ke.To != string(SegmentCommitted) {
		t.Fatalf("state error locators: %+v", ke)
	}
	after, _ := sm.Record(segKey, true)
	if before.Segment != after.Segment || before.Progress != after.Progress || before.Sequence != after.Sequence {
		t.Fatalf("illegal transition changed state/progress: before=%+v after=%+v", before, after)
	}
	if err := sm.TransitionSegment(segKey, SegmentCalibrating); err != nil {
		t.Fatal(err)
	}
	if err := sm.TransitionSegment(segKey, SegmentCommitted); err != nil {
		t.Fatal(err)
	}

	iv := intervals[0]
	if err := sm.TransitionInterval(iv.ID, IntervalProcessing); err != nil {
		t.Fatal(err)
	}
	ib, _ := sm.Record(iv.ID, false)
	err = sm.TransitionInterval(iv.ID, IntervalPending)
	requireCode(t, err, CodeState)
	ia, _ := sm.Record(iv.ID, false)
	if ib.Interval != ia.Interval || ib.Progress != ia.Progress {
		t.Fatalf("illegal interval transition changed state: before=%+v after=%+v", ib, ia)
	}
}

func TestSQLiteCheckpointsSurviveReopenWithStableMetadata(t *testing.T) {
	g := mustGenerate(t, 600)
	store, path := mustStore(t)
	engine := NewEngine(store)
	first := mustRun(t, engine, g.Project, ModeFull)
	if len(first.PhysicalCommitOrder) != len(first.Intervals) {
		t.Fatalf("not every interval committed")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		t.Fatalf("checkpoint file missing/empty: %v", err)
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	checkpoints, err := reopened.LoadCheckpoints(context.Background(), g.Project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpoints) != len(first.Intervals) {
		t.Fatalf("loaded %d checkpoints, want %d", len(checkpoints), len(first.Intervals))
	}
	lastSeq := int64(-1)
	for _, cp := range checkpoints {
		if cp.Sequence <= lastSeq {
			t.Fatalf("checkpoint sequence not strictly increasing: %d after %d", cp.Sequence, lastSeq)
		}
		lastSeq = cp.Sequence
		if cp.Result.FeatureKey == "" || cp.Result.CalibrationKey == "" || cp.Result.ContentHash == "" ||
			cp.Result.SourceVersion != g.Project.SourceVersion || cp.Result.ConversionVersion == "" ||
			cp.Result.AlgorithmVersion == "" || cp.Result.FeatureDigest == "" ||
			cp.Result.CalibrationDigest == "" {
			t.Fatalf("checkpoint metadata incomplete: %+v", cp.Result)
		}
		if cp.Result.ConsumedOffset < cp.Result.StartSample {
			t.Fatalf("consumed offset is not monotonic: %+v", cp.Result)
		}
	}
	// Reopening a fresh store must not reset the persisted sequence.
	maxSeq := lastSeq
	second := mustRun(t, NewEngine(reopened), g.Project, ModeRealTime)
	seqs := make(map[int64]bool)
	for _, iv := range second.Intervals {
		if iv.CommitSequence <= maxSeq {
			t.Fatalf("sequence was reset after reopen: got %d, previous max %d", iv.CommitSequence, maxSeq)
		}
		if seqs[iv.CommitSequence] {
			t.Fatalf("duplicate commit sequence %d", iv.CommitSequence)
		}
		seqs[iv.CommitSequence] = true
	}
}

func TestConcurrentStoreHandlesSerializeCommitSequence(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/shared.db"
	a, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var wg sync.WaitGroup
	seqCh := make(chan int64, 12)
	errCh := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store := a
			if i%2 == 0 {
				store = b
			}
			seq, err := store.CommitInterval(context.Background(), "concurrent", IntervalResult{
				IntervalID: fmt.Sprintf("interval-%02d", i),
				DialogueID: "dlg", SegmentID: "seg", StartFrame: i, EndFrame: i + 1,
			})
			if err != nil {
				errCh <- err
				return
			}
			seqCh <- seq
		}(i)
	}
	wg.Wait()
	close(seqCh)
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	seen := make(map[int64]bool)
	for seq := range seqCh {
		if seen[seq] {
			t.Fatalf("duplicate sequence %d across stores", seq)
		}
		seen[seq] = true
	}
	if len(seen) != 12 {
		t.Fatalf("got %d sequences, want 12", len(seen))
	}
}
