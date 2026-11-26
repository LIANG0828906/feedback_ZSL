package pipeline_test

import (
	"context"
	"path/filepath"
	"testing"

	"wardrobekernel/internal/cache"
	"wardrobekernel/internal/checkpoint"
	"wardrobekernel/internal/compare"
	"wardrobekernel/internal/model"
	"wardrobekernel/internal/pipeline"
	"wardrobekernel/internal/samplegen"
	"wardrobekernel/internal/scheduler"
	"wardrobekernel/internal/xerr"
)

func TestThreePathsTruthIncrementalAndCheckpoint(t *testing.T) {
	project, err := samplegen.Generate(model.FixedSeed)
	if err != nil {
		t.Fatal(err)
	}
	truth, _, err := samplegen.BuildTruth(project)
	if err != nil {
		t.Fatal(err)
	}
	full, err := pipeline.VerifyTruth(project, truth)
	if err != nil {
		t.Fatalf("truth mismatch: %v", err)
	}

	dbPath := filepath.Join(t.TempDir(), "checkpoint.sqlite")
	store, err := checkpoint.Open(dbPath, project.AlgorithmVersion, project.ConstraintVersion)
	if err != nil {
		t.Fatal(err)
	}
	realtime, err := pipeline.RunRealtime(context.Background(), project, pipeline.RealtimeOptions{
		Scheduler:  scheduler.Options{FailFirstAttempt: map[string]bool{"waist_belt": true}},
		Checkpoint: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := compare.Plans(full, realtime.Plan); err != nil {
		t.Fatalf("realtime mismatch: %v", err)
	}

	resume, err := pipeline.ResumeAndContinue(context.Background(), project, store, scheduler.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := compare.Plans(full, resume.Plan); err != nil {
		t.Fatalf("resume mismatch: %v", err)
	}

	reopened, err := checkpoint.Open(dbPath, project.AlgorithmVersion, project.ConstraintVersion)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.LastSequence() != len(full.Batches) {
		t.Fatalf("durable sequence=%d want %d", reopened.LastSequence(), len(full.Batches))
	}

	cachePath := filepath.Join(t.TempDir(), "cache.json")
	persistent, err := cache.Load(cachePath, project.AlgorithmVersion, project.ConstraintVersion)
	if err != nil {
		t.Fatal(err)
	}
	for _, batch := range full.Batches {
		if err := persistent.Put(batch); err != nil {
			t.Fatal(err)
		}
	}

	edited, err := pipeline.CloneProject(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := pipeline.EditMaterialVersion(edited, "waist_belt", "main", "v-test-edit"); err != nil {
		t.Fatal(err)
	}
	inc, err := pipeline.IncrementalReplan(project, full, persistent, edited)
	if err != nil {
		t.Fatalf("incremental material edit mismatch: %v", err)
	}
	if len(inc.Invalidation.InvalidBatches) != 1 || inc.Invalidation.InvalidBatches[0] != 3 {
		t.Fatalf("unexpected invalid batches: %#v", inc.Invalidation)
	}
	if len(inc.Invalidation.ReusedBatches) != 3 {
		t.Fatalf("expected three reused batches, got %#v", inc.Invalidation.ReusedBatches)
	}

	geometryEdited, err := pipeline.CloneProject(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := pipeline.EditGeometryScale(geometryEdited, "waist_belt", 24); err != nil {
		t.Fatal(err)
	}
	geoCache, err := cache.Load(filepath.Join(t.TempDir(), "geo-cache.json"), project.AlgorithmVersion, project.ConstraintVersion)
	if err != nil {
		t.Fatal(err)
	}
	for _, batch := range full.Batches {
		if err := geoCache.Put(batch); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pipeline.IncrementalReplan(project, full, geoCache, geometryEdited); err != nil {
		t.Fatalf("geometry incremental mismatch: %v", err)
	}
}

func TestVersionMismatchReturnsEVer(t *testing.T) {
	project, _ := samplegen.Generate(model.FixedSeed)
	dbPath := filepath.Join(t.TempDir(), "cp.sqlite")
	store, err := checkpoint.Open(dbPath, project.AlgorithmVersion, project.ConstraintVersion)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := pipeline.FullPlan(project)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitBatch(plan.Batches[0]); err != nil {
		t.Fatal(err)
	}
	_, err = checkpoint.Open(dbPath, "wrong-algo", project.ConstraintVersion)
	coded, ok := xerr.AsError(err)
	if !ok || coded.Code != xerr.EVer || len(coded.AffectedBatches) == 0 {
		t.Fatalf("expected E_VER with affected batches, got %#v", err)
	}
}

func TestCompareLocatesFirstMismatchAsEDiff(t *testing.T) {
	project, _ := samplegen.Generate(model.FixedSeed)
	full, err := pipeline.FullPlan(project)
	if err != nil {
		t.Fatal(err)
	}
	changed := clonePlan(full)
	changed.Batches[0].Placements[0].Submeshes[0].VertexStart += 7
	err = compare.Plans(full, changed)
	coded, ok := xerr.AsError(err)
	if !ok || coded.Code != xerr.EDiff || coded.FirstMismatch == nil {
		t.Fatalf("expected E_DIFF mismatch locator, got %#v", err)
	}
	if coded.FirstMismatch.BatchNumber != 1 || coded.FirstMismatch.PartID == "" || coded.FirstMismatch.FieldName == "" {
		t.Fatalf("mismatch locator incomplete: %#v", coded.FirstMismatch)
	}
}

func clonePlan(in *model.Plan) *model.Plan {
	cp := *in
	cp.Batches = append([]model.Batch(nil), in.Batches...)
	for i := range cp.Batches {
		cp.Batches[i].Placements = append([]model.PartPlacement(nil), in.Batches[i].Placements...)
		for j := range cp.Batches[i].Placements {
			cp.Batches[i].Placements[j].Submeshes = append([]model.SubmeshPlacement(nil), in.Batches[i].Placements[j].Submeshes...)
		}
	}
	return &cp
}
