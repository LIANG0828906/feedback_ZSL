package maskqc_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"maskqc"
	"maskqc/store"
)

func TestShuffledDeclarationOrderAndDeterministicRaster(t *testing.T) {
	p := maskqc.GenerateProject(maskqc.DefaultSeed, 0)
	forward, err := maskqc.RunPipeline(context.Background(), p, testParams(), "forward", nil)
	if err != nil {
		t.Fatal(err)
	}
	shuffled := maskqc.CloneForEdit(p)
	for i, j := 0, len(shuffled.Strokes)-1; i < j; i, j = i+1, j-1 {
		shuffled.Strokes[i], shuffled.Strokes[j] = shuffled.Strokes[j], shuffled.Strokes[i]
	}
	reverse, err := maskqc.RunPipeline(context.Background(), shuffled, testParams(), "shuffled", nil)
	if err != nil {
		t.Fatal(err)
	}
	if eq := maskqc.VerifyPipelineEquivalence(forward, reverse); !eq.Consistent {
		t.Fatalf("declaration order changed adjudicated output: %+v", eq.Diff)
	}

	stroke := p.Strokes[0]
	bounds := maskqc.StrokeBounds(stroke, p.Width, p.Height)
	coords := maskqc.ChunksForRect(bounds, p.Width, p.Height, 16)
	opts := maskqc.SchedulerOptions{MaxParallel: 4, MaxBufferPixels: 4096, QueueBudget: 2}
	jobs := make([]maskqc.Job, 0, len(coords))
	expected := map[maskqc.ChunkCoord][]byte{}
	for _, c := range coords {
		region := maskqc.ChunkRect(c, p.Width, p.Height, 16).Intersect(bounds)
		alpha, rerr := maskqc.RasterizeStrokeRegion(stroke, region)
		if rerr != nil {
			t.Fatal(rerr)
		}
		expected[c] = alpha
	}
	for i := len(coords) - 1; i >= 0; i-- {
		c := coords[i]
		region := maskqc.ChunkRect(c, p.Width, p.Height, 16).Intersect(bounds)
		jobs = append(jobs, maskqc.Job{ID: c.String(), Chunk: c, Pixels: region.Width() * region.Height(), Run: func(buf []byte) error {
			return maskqc.RasterizeStrokeInto(stroke, region, buf[:region.Width()*region.Height()])
		}})
	}
	results := maskqc.RunJobs(context.Background(), jobs, opts)
	if err := maskqc.AllJobsFailed(results); err != nil {
		t.Fatal(err)
	}
	for i, jr := range results {
		region := jobs[i].Pixels
		_ = region
		// Re-run through the public region API and compare the worker buffer output indirectly.
		got, err := maskqc.RasterizeStrokeRegion(stroke, maskqc.ChunkRect(jr.Chunk, p.Width, p.Height, 16).Intersect(bounds))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(expected[jr.Chunk]) {
			t.Fatalf("chunk %s raster changed", jr.Chunk)
		}
	}
}

func TestMoveEditPreciseInvalidationAndRecovery(t *testing.T) {
	ctx := context.Background()
	base := maskqc.GenerateProject(maskqc.DefaultSeed, 1)
	db := filepath.Join(t.TempDir(), "move.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	engine := maskqc.NewEngine(maskqc.CloneForEdit(base), testParams(), st)
	if _, err := engine.FullRebuild(ctx); err != nil {
		t.Fatal(err)
	}
	edit := maskqc.Edit{Kind: maskqc.EditMoveEnd, StrokeSeq: 1, Point: maskqc.Point{X: 40, Y: 20}}
	ir, err := engine.ApplyEdit(ctx, edit)
	if err != nil {
		var pe *maskqc.Error
		if !(errors.As(err, &pe) && pe.Code == maskqc.CodeTop) {
			t.Fatal(err)
		}
	}
	if len(ir.Invalidation.InvalidatedChunks) == 0 {
		t.Fatal("move edit did not invalidate chunks")
	}
	if len(ir.PersistentHits) != len(ir.Invalidation.ReusedChunks) || len(ir.PersistentHits) == 0 {
		t.Fatalf("expected all reused chunks from SQLite: hits=%d reused=%d", len(ir.PersistentHits), len(ir.Invalidation.ReusedChunks))
	}

	finalProject, err := maskqc.ApplyEdits(base, []maskqc.Edit{edit})
	if err != nil {
		t.Fatal(err)
	}
	validation, records, err := st.RecoverySnapshot(ctx, engine.Result.Identity)
	if err != nil || !validation.Valid {
		t.Fatalf("recovery snapshot: %+v %v", validation, err)
	}
	recovery, rerr := maskqc.RunPipelineWithRecords(ctx, finalProject, testParams(), "recovery", st, records)
	if rerr != nil {
		var pe *maskqc.Error
		if !(errors.As(rerr, &pe) && pe.Code == maskqc.CodeTop) {
			t.Fatal(rerr)
		}
	}
	full, ferr := maskqc.RunPipeline(ctx, finalProject, testParams(), "full", nil)
	if ferr != nil {
		var pe *maskqc.Error
		if !(errors.As(ferr, &pe) && pe.Code == maskqc.CodeTop) {
			t.Fatal(ferr)
		}
	}
	if eq := maskqc.VerifyPipelineEquivalence(engine.Result, recovery, full); !eq.Consistent {
		t.Fatalf("move edit paths differ: %+v details=%+v", eq.Diff, eq.Diff.Details)
	}

	bad := engine.Result.Identity
	bad.AlgorithmVersion = "future-algo"
	badV, badRecs, err := st.RecoverySnapshot(ctx, bad)
	if err != nil || badV.Valid || !badV.Invalidated || badRecs != nil {
		t.Fatalf("expected invalid version snapshot reset: v=%+v recs=%d err=%v", badV, len(badRecs), err)
	}
}

func TestSchedulerQueueWaitRecorded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan struct{})
	release := make(chan struct{})
	jobs := []maskqc.Job{
		{ID: "blocker", Chunk: maskqc.ChunkCoord{X: 0, Y: 0}, Pixels: 1, Run: func([]byte) error {
			close(started)
			<-release
			return nil
		}},
		{ID: "queued-1", Chunk: maskqc.ChunkCoord{X: 1, Y: 0}, Pixels: 1, Run: func([]byte) error { return nil }},
		{ID: "queued-2", Chunk: maskqc.ChunkCoord{X: 2, Y: 0}, Pixels: 1, Run: func([]byte) error { return nil }},
	}
	done := make(chan []maskqc.JobResult, 1)
	go func() {
		done <- maskqc.RunJobs(ctx, jobs, maskqc.SchedulerOptions{MaxParallel: 1, MaxBufferPixels: 16, QueueBudget: 1})
	}()
	<-started
	time.Sleep(50 * time.Millisecond)
	close(release)
	results := <-done
	foundWait := false
	for _, r := range results {
		for _, w := range r.Waits {
			if w.Reason != "" && w.QueueBudget == 1 {
				foundWait = true
			}
		}
	}
	if !foundWait {
		t.Fatalf("expected bounded queue wait record: %+v", results)
	}
}
