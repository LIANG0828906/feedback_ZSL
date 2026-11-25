package maskqc_test

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"sync/atomic"
	"testing"

	"maskqc"
	"maskqc/store"
)

func testParams() maskqc.Params {
	p := maskqc.DefaultParams()
	p.Scheduler = maskqc.SchedulerOptions{MaxParallel: 2, MaxBufferPixels: 4096, QueueBudget: 8}
	return p
}

func requireDomainTopology(t *testing.T, p *maskqc.Project) *maskqc.PipelineResult {
	t.Helper()
	res, err := maskqc.RunPipeline(context.Background(), p, testParams(), "test-full", nil)
	if err != nil {
		var pe *maskqc.Error
		if errors.As(err, &pe) {
			t.Fatalf("topology verification failed: %s details=%+v", pe.Error(), pe.Details)
		}
		t.Fatalf("RunPipeline: %v", err)
	}
	if len(res.Topology) == 0 {
		t.Fatal("expected at least one topology domain")
	}
	return res
}

func TestDeterministicGeneratorAndTopology(t *testing.T) {
	a := maskqc.GenerateProject(maskqc.DefaultSeed, 0)
	b := maskqc.GenerateProject(maskqc.DefaultSeed, 0)
	if maskqc.ProjectContentHash(a) != maskqc.ProjectContentHash(b) {
		t.Fatal("generator is not deterministic")
	}
	for i, p := range maskqc.GenerateSamples(3) {
		res := requireDomainTopology(t, p)
		t.Logf("batch=%d domains=%d chunks=%d commits=%d", i, len(res.Topology), len(res.Chunks), len(res.StrokeCommits))
		for _, rec := range res.StrokeCommits {
			assertTruthComponentRegion(t, p, rec)
		}
	}
}

func assertTruthComponentRegion(t *testing.T, p *maskqc.Project, rec maskqc.ComponentRecord) {
	t.Helper()
	comp := p.Truth.StrokeComponents[rec.StrokeSeq]
	if comp == nil {
		t.Fatalf("missing truth component for stroke %d", rec.StrokeSeq)
	}
	w := comp.Bounds.Width()
	rw := rec.Bounds.Width()
	for y := rec.Bounds.MinY; y < rec.Bounds.MaxY; y++ {
		for x := rec.Bounds.MinX; x < rec.Bounds.MaxX; x++ {
			truthIdx := (y-comp.Bounds.MinY)*w + (x - comp.Bounds.MinX)
			gotIdx := (y-rec.Bounds.MinY)*rw + (x - rec.Bounds.MinX)
			if truthIdx < 0 || truthIdx >= len(comp.Alpha) || gotIdx < 0 || gotIdx >= len(rec.Alpha) {
				t.Fatalf("component index out of range stroke=%d at %d,%d", rec.StrokeSeq, x, y)
			}
			if comp.Alpha[truthIdx] != rec.Alpha[gotIdx] {
				t.Fatalf("stroke %d component mismatch at %d,%d: truth=%d got=%d", rec.StrokeSeq, x, y, comp.Alpha[truthIdx], rec.Alpha[gotIdx])
			}
		}
	}
}

func TestThreeSchemaRoundTripAndErrors(t *testing.T) {
	p := maskqc.GenerateProject(maskqc.DefaultSeed, 1)
	for _, target := range []string{maskqc.SchemaV1, maskqc.SchemaV2, maskqc.SchemaV3} {
		res, err := maskqc.ExportProject(p, target)
		if target == maskqc.SchemaV3 {
			if err != nil {
				t.Fatalf("export %s: %v", target, err)
			}
		} else {
			var pe *maskqc.Error
			if !errors.As(err, &pe) || pe.Code != maskqc.CodeLossy || res == nil {
				t.Fatalf("expected E_LOSSY result for %s, got res=%v err=%v", target, res != nil, err)
			}
			if len(res.Loss.Details["items"].([]maskqc.LossItem)) == 0 {
				t.Fatalf("expected loss items for %s", target)
			}
		}
		loaded, lerr := maskqc.LoadProjectDocument(res.Data)
		if lerr != nil {
			t.Fatalf("load exported %s: %v", target, lerr)
		}
		bySeq := map[int]maskqc.Stroke{}
		for _, s := range loaded.Project.Strokes {
			bySeq[s.Seq] = s
		}
		for _, want := range p.Strokes {
			got, ok := bySeq[want.Seq]
			if !ok {
				t.Fatalf("%s missing seq %d", target, want.Seq)
			}
			if got.Start != want.Start || got.End != want.End {
				t.Fatalf("%s seq %d coordinates changed: got %+v -> %+v want %+v -> %+v", target, want.Seq, got.Start, got.End, want.Start, want.End)
			}
			if got.WeightPermille != want.WeightPermille {
				t.Fatalf("%s seq %d weight changed: got %d want %d", target, want.Seq, got.WeightPermille, want.WeightPermille)
			}
			if got.RadiusTick != want.RadiusTick {
				t.Fatalf("%s seq %d radius changed: got %d want %d", target, want.Seq, got.RadiusTick, want.RadiusTick)
			}
		}
	}

	if _, err := maskqc.LoadProjectDocument([]byte(`{"schema":"icon.project/v9"}`)); err == nil {
		t.Fatal("expected unknown schema error")
	} else {
		var pe *maskqc.Error
		if !errors.As(err, &pe) || pe.Code != maskqc.CodeSchema || pe.SchemaVersion != "icon.project/v9" || pe.Path != "/schema" {
			t.Fatalf("unexpected unknown schema error: %#v", err)
		}
	}
	if _, err := maskqc.LoadProjectDocument([]byte(`{"schema":"` + maskqc.SchemaV3 + `"}`)); err == nil {
		t.Fatal("expected missing required field error")
	} else {
		var pe *maskqc.Error
		if !errors.As(err, &pe) || pe.Code != maskqc.CodeSchema || pe.Path == "" || pe.SchemaVersion != maskqc.SchemaV3 {
			t.Fatalf("unexpected missing field error: %#v", err)
		}
	}
}

func TestThreePathsIncrementalRecoveryAndDiff(t *testing.T) {
	ctx := context.Background()
	base := maskqc.GenerateProject(maskqc.DefaultSeed, 0)
	edits := []maskqc.Edit{
		{Kind: maskqc.EditPriority, StrokeSeq: 1, Value: 4},
		{Kind: maskqc.EditWeight, StrokeSeq: 6, Value: 900},
	}
	finalProject, err := maskqc.ApplyEdits(base, edits)
	if err != nil {
		t.Fatal(err)
	}
	full, err := maskqc.RunPipeline(ctx, finalProject, testParams(), "one-shot-full", nil)
	if err != nil {
		var pe *maskqc.Error
		if errors.As(err, &pe) && pe.Code == maskqc.CodeTop {
			t.Logf("edited project topology diagnostic: %+v", pe.Details)
		} else {
			t.Fatalf("full pipeline: %v", err)
		}
	}

	dbPath := filepath.Join(t.TempDir(), "maskqc.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	engine := maskqc.NewEngine(maskqc.CloneForEdit(base), testParams(), st)
	if _, err := engine.FullRebuild(ctx); err != nil {
		t.Fatalf("engine rebuild: %v", err)
	}
	var last *maskqc.IncrementalResult
	for _, edit := range edits {
		last, err = engine.ApplyEdit(ctx, edit)
		if err != nil {
			var pe *maskqc.Error
			if errors.As(err, &pe) && pe.Code == maskqc.CodeTop {
				t.Logf("edit produced expected topology diagnostic: %v", pe.Details)
			} else if err != nil {
				t.Fatalf("apply edit: %v", err)
			}
		}
	}
	if len(last.Invalidation.InvalidatedChunks) == 0 || len(last.Invalidation.ReusedChunks) == 0 {
		t.Fatalf("expected both invalidated and reused chunks: %+v", last.Invalidation)
	}
	if len(last.PersistentHits) != len(last.Invalidation.ReusedChunks) {
		t.Fatalf("persistent hits=%d reused=%d", len(last.PersistentHits), len(last.Invalidation.ReusedChunks))
	}
	if len(last.Invalidation.InvalidatedTopo) == 0 {
		t.Fatal("expected downstream topology verification invalidation")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	validation, err := reopened.ValidateSnapshot(ctx, finalProject.ID, full.Identity)
	if err != nil || !validation.Valid {
		t.Fatalf("snapshot validation: valid=%v err=%v changed=%v", validation != nil && validation.Valid, err, validation)
	}
	records, err := reopened.LoadComponentRecords(ctx, finalProject.ID)
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := maskqc.RunPipelineWithRecords(ctx, finalProject, testParams(), "breakpoint-recovery", reopened, records)
	if err != nil {
		var pe *maskqc.Error
		if !(errors.As(err, &pe) && pe.Code == maskqc.CodeTop) {
			t.Fatalf("recovery pipeline: %v", err)
		}
	}
	equivalence := maskqc.VerifyPipelineEquivalence(engine.Result, recovery, full)
	if !equivalence.Consistent {
		t.Logf("diff details: %+v", equivalence.Diff.Details)
		t.Fatalf("three paths differ: %+v", equivalence.Diff)
	}

	mutated := *full
	mutated.Chunks = append([]maskqc.ChunkResult(nil), full.Chunks...)
	mutated.Chunks[0].OutputHash = "changed"
	diff := maskqc.VerifyPipelineEquivalence(full, &mutated)
	if diff.Consistent || diff.Diff.Code != maskqc.CodeDiff || diff.Diff.Field != "output_hash" {
		t.Fatalf("expected E_DIFF output_hash, got %+v", diff)
	}
}

type crashSink struct {
	inner     maskqc.CheckpointSink
	committed int32
	failAfter int32
}

func (c *crashSink) CommitStrokeCheckpoint(ctx context.Context, cp maskqc.StrokeCheckpoint) (int64, error) {
	n := atomic.AddInt32(&c.committed, 1)
	if n > c.failAfter {
		return 0, errors.New("simulated forced termination before durable commit")
	}
	return c.inner.CommitStrokeCheckpoint(ctx, cp)
}

func TestSQLiteCrashBoundaryAndVersionInvalidation(t *testing.T) {
	ctx := context.Background()
	p := maskqc.GenerateProject(maskqc.DefaultSeed, 2)
	full := requireDomainTopology(t, p)
	dbPath := filepath.Join(t.TempDir(), "crash.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	sink := &crashSink{inner: st, failAfter: 2}
	if _, err := maskqc.RunPipeline(ctx, p, testParams(), "crashed", sink); err == nil {
		t.Fatal("expected simulated crash error")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	validation, err := st2.ValidateSnapshot(ctx, p.ID, full.Identity)
	if err != nil || !validation.Valid {
		t.Fatalf("post-crash validation failed: %+v %v", validation, err)
	}
	records, err := st2.LoadComponentRecords(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	for _, rec := range records {
		seen[rec.StrokeSeq] = true
	}
	if len(seen) != 2 {
		t.Fatalf("expected exactly two durable strokes, got %d (%v)", len(seen), seen)
	}
	recovery, err := maskqc.RunPipelineWithRecords(ctx, p, testParams(), "recovery", st2, records)
	if err != nil {
		t.Fatal(err)
	}
	eq := maskqc.VerifyPipelineEquivalence(full, recovery)
	if !eq.Consistent {
		t.Fatalf("recovery/full mismatch: %+v", eq.Diff)
	}

	badIdentity := full.Identity
	badIdentity.AlgorithmVersion = "raster-v0-old"
	versionValidation, err := st2.ValidateSnapshot(ctx, p.ID, badIdentity)
	if err != nil || versionValidation.Valid {
		t.Fatalf("expected version mismatch invalidation: %+v %v", versionValidation, err)
	}
	foundVersion := false
	for _, f := range versionValidation.ChangedFields {
		if f == "algorithm_version" {
			foundVersion = true
		}
	}
	if !foundVersion {
		t.Fatalf("expected algorithm_version changed field, got %v", versionValidation.ChangedFields)
	}

	first := full.StrokeCommits[0]
	if err := st2.CorruptComponentForTest(ctx, p.ID, first.StrokeSeq, first.Chunk.X, first.Chunk.Y); err != nil {
		t.Fatal(err)
	}
	corruptValidation, err := st2.ValidateSnapshot(ctx, p.ID, full.Identity)
	if err != nil || corruptValidation.Valid {
		t.Fatalf("expected content hash mismatch: %+v %v", corruptValidation, err)
	}
	if err := st2.ResetProject(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	afterReset, err := st2.LoadComponentRecords(ctx, p.ID)
	if err != nil || len(afterReset) != 0 {
		t.Fatalf("expected reset snapshot to be empty: %d %v", len(afterReset), err)
	}
}

func TestParameterOverflowAndTopologyCodes(t *testing.T) {
	bad := maskqc.Stroke{Seq: 9, Start: maskqc.Point{X: 0, Y: 0}, End: maskqc.Point{X: 1, Y: 1}, WeightPermille: 1001, RadiusTick: 2, HardnessPermille: 500}
	if _, err := maskqc.RasterizeStroke(bad, 16, 16); err == nil {
		t.Fatal("expected E_PARA weight")
	} else {
		var pe *maskqc.Error
		if !errors.As(err, &pe) || pe.Code != maskqc.CodePara || pe.Field == "" || len(pe.LegalRange) != 2 {
			t.Fatalf("unexpected parameter error: %#v", err)
		}
	}
	bad.HardnessPermille = 1200
	bad.WeightPermille = 500
	if _, err := maskqc.RasterizeStroke(bad, 16, 16); err == nil {
		t.Fatal("expected E_PARA hardness")
	} else {
		var pe *maskqc.Error
		if !errors.As(err, &pe) || pe.Code != maskqc.CodePara {
			t.Fatalf("unexpected hardness error: %#v", err)
		}
	}
	if _, _, err := maskqc.ConvertScale(math.MaxInt, math.MaxInt, math.MaxInt, "overflow"); err == nil {
		t.Fatal("expected E_OVF")
	} else if err.Code != maskqc.CodeOvf || len(err.Operands) != 2 {
		t.Fatalf("unexpected overflow error: %#v", err)
	}

	domains := []maskqc.DomainMetric{{DomainID: 1, Holes: 1, BandWidth: 6, AngleDeg: 30}}
	wrong := []maskqc.DomainMetric{{DomainID: 1, Holes: 4, BandWidth: 6, AngleDeg: 30}}
	topErr := maskqc.VerifyTopology(wrong, domains, maskqc.DefaultTopologyTolerance())
	if topErr == nil || topErr.Code != maskqc.CodeTop {
		t.Fatalf("expected E_TOP, got %v", topErr)
	}
	if _, ok := topErr.Details["domains"]; !ok {
		t.Fatalf("E_TOP must list domains and measured values: %+v", topErr.Details)
	}
}

func TestSchedulerIsolationAndRetry(t *testing.T) {
	ctx := context.Background()
	var attempts int32
	jobs := []maskqc.Job{
		{ID: "ok", Chunk: maskqc.ChunkCoord{X: 0, Y: 0}, Pixels: 1, Run: func([]byte) error { return nil }},
		{ID: "flaky", Chunk: maskqc.ChunkCoord{X: 1, Y: 0}, Pixels: 1, Run: func([]byte) error {
			if atomic.AddInt32(&attempts, 1) == 1 {
				return errors.New("boom")
			}
			return nil
		}},
		{ID: "too-big", Chunk: maskqc.ChunkCoord{X: 2, Y: 0}, Pixels: 4097, Run: func([]byte) error { return nil }},
	}
	opts := maskqc.SchedulerOptions{MaxParallel: 1, MaxBufferPixels: 4096, QueueBudget: 1}
	results := maskqc.RunJobs(ctx, jobs, opts)
	if maskqc.AllJobsFailed(results) == nil {
		t.Fatal("expected failures")
	}
	if results[2].Err == nil || results[2].Err.Code != maskqc.CodePara {
		t.Fatalf("expected buffer E_PARA, got %+v", results[2].Err)
	}
	retryable := results[:2]
	retried := maskqc.RetryFailed(ctx, jobs[:2], retryable, opts)
	if maskqc.AllJobsFailed(retried) != nil {
		t.Fatalf("retry should succeed: %+v", retried)
	}
}
