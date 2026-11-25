package pipeline_test

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"testing"

	"skeletonretarget/internal/checkpoint"
	"skeletonretarget/internal/codec"
	"skeletonretarget/internal/generator"
	"skeletonretarget/internal/model"
	"skeletonretarget/internal/pipeline"
	"skeletonretarget/internal/retarget"
)

func loadDoc(t *testing.T) *model.Document {
	t.Helper()
	sample, err := generator.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := codec.Load(sample.Studio)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestLoadIncrementalAndRecoveryConverge(t *testing.T) {
	ctx := context.Background()
	doc := loadDoc(t)
	path := filepath.Join(t.TempDir(), "three-paths.db")
	options := pipeline.DefaultOptions(path)
	full, err := pipeline.Run(ctx, doc, options, pipeline.Load, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !full.Complete || len(full.Frames) != generator.FrameCount {
		t.Fatalf("load path incomplete: %+v", full)
	}
	incremental, err := pipeline.Run(ctx, doc, options, pipeline.Incremental, nil)
	if err != nil {
		t.Fatal(err)
	}
	hits := 0
	for _, interval := range incremental.Intervals {
		if interval.CacheHit {
			hits++
		}
	}
	if hits != len(incremental.Intervals) {
		t.Fatalf("incremental path expected all hits, got %d/%d", hits, len(incremental.Intervals))
	}
	recovered, err := pipeline.Run(ctx, doc, options, pipeline.Recovery, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range []struct {
		name string
		got  []retarget.FrameResult
	}{
		{"incremental", incremental.Frames},
		{"recovery", recovered.Frames},
	} {
		if comparison := pipeline.CompareFrames(full.Frames, pair.got); !comparison.Equal {
			t.Fatalf("%s differs from load: %+v", pair.name, comparison)
		}
	}
	if pipeline.FramesDigest(full.Frames) != pipeline.FramesDigest(recovered.Frames) {
		t.Fatal("recovery digest differs")
	}
}

func TestInterruptedRunResumesOnlyCommittedIntervals(t *testing.T) {
	ctx := context.Background()
	doc := loadDoc(t)
	root := t.TempDir()
	resumePath := filepath.Join(root, "resume.db")
	referencePath := filepath.Join(root, "reference.db")
	resumeOptions := pipeline.DefaultOptions(resumePath)
	var partial *pipeline.Report
	var err error
	resumeOptions.StopAfter = 2
	partial, err = pipeline.Run(ctx, doc, resumeOptions, pipeline.Load, nil)
	if !errors.Is(err, pipeline.ErrInterrupted) {
		t.Fatalf("expected interruption, got %v", err)
	}
	if partial == nil || len(partial.Frames) != 8 {
		t.Fatalf("expected two committed intervals, got %+v", partial)
	}
	reference, err := pipeline.Run(ctx, doc, pipeline.DefaultOptions(referencePath), pipeline.Load, nil)
	if err != nil {
		t.Fatal(err)
	}
	resumeOptions.StopAfter = 0
	recovered, err := pipeline.Run(ctx, doc, resumeOptions, pipeline.Recovery, nil)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.RecoveredTo != 2 {
		t.Fatalf("expected two recovered intervals, got %d", recovered.RecoveredTo)
	}
	if comparison := pipeline.CompareFrames(reference.Frames, recovered.Frames); !comparison.Equal {
		t.Fatalf("recovered output differs: %+v", comparison)
	}
}

func TestConstraintEditInvalidatesOnlyAffectedAndDownstream(t *testing.T) {
	ctx := context.Background()
	doc := loadDoc(t)
	root := t.TempDir()
	cachePath := filepath.Join(root, "incremental.db")
	fullPath := filepath.Join(root, "full.db")
	baseOptions := pipeline.DefaultOptions(cachePath)
	if _, err := pipeline.Run(ctx, doc, baseOptions, pipeline.Load, nil); err != nil {
		t.Fatal(err)
	}

	edited := pipeline.CloneDocument(doc)
	for i := range edited.Skeleton.Joints {
		if edited.Skeleton.Joints[i].ID == "arm_l" {
			edited.Skeleton.Joints[i].Constraint.MaxRad = int64(math.Round(18.5 * math.Pi / 180 * float64(model.AngleScale)))
		}
	}
	editedOptions := pipeline.DefaultOptions(cachePath)
	editedOptions.Retarget.DisableTruthCheck = true
	edit := []pipeline.Edit{{
		Kind: pipeline.EditConstraint, Joint: "arm_l", Reason: "designer tightened left arm swing",
	}}
	incremental, err := pipeline.Run(ctx, edited, editedOptions, pipeline.Incremental, edit)
	if err != nil {
		t.Fatal(err)
	}
	if incremental.Invalidations == nil {
		t.Fatal("expected invalidation report")
	}
	if incremental.Invalidations[0].IntervalID != 4 {
		t.Fatalf("expected first invalidation at interval 4, got %+v", incremental.Invalidations[0])
	}
	hits := 0
	for _, interval := range incremental.Intervals {
		if interval.CacheHit {
			hits++
		}
	}
	if hits != 4 {
		t.Fatalf("expected unaffected prefix of four intervals to be reused, got %d hits", hits)
	}
	fullOptions := pipeline.DefaultOptions(fullPath)
	fullOptions.Retarget.DisableTruthCheck = true
	fullEdited, err := pipeline.Run(ctx, edited, fullOptions, pipeline.Load, nil)
	if err != nil {
		t.Fatal(err)
	}
	if comparison := pipeline.CompareFrames(fullEdited.Frames, incremental.Frames); !comparison.Equal {
		t.Fatalf("incremental output differs from full edited output: %+v", comparison)
	}
}

func TestThreeFormatPipelinesProduceIdenticalTargets(t *testing.T) {
	ctx := context.Background()
	sample, err := generator.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	var reference []retarget.FrameResult
	for i, data := range [][]byte{sample.Studio, sample.Engine, sample.Legacy} {
		doc, err := codec.Load(data)
		if err != nil {
			t.Fatal(err)
		}
		report, err := pipeline.Run(ctx, doc, pipeline.DefaultOptions(filepath.Join(root, "format.db")), pipeline.Load, nil)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			reference = report.Frames
		} else if comparison := pipeline.CompareFrames(reference, report.Frames); !comparison.Equal {
			t.Fatalf("format %d differs: %+v", i, comparison)
		}
	}
}

func TestCheckpointCorruptionRollsBackSuffix(t *testing.T) {
	ctx := context.Background()
	doc := loadDoc(t)
	path := filepath.Join(t.TempDir(), "corrupt.db")
	options := pipeline.DefaultOptions(path)
	options.StopAfter = 3
	if _, err := pipeline.Run(ctx, doc, options, pipeline.Load, nil); !errors.Is(err, pipeline.ErrInterrupted) {
		t.Fatalf("expected interruption, got %v", err)
	}
	store, err := checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CorruptPayload(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	options.StopAfter = 0
	recovered, err := pipeline.Run(ctx, doc, options, pipeline.Recovery, nil)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.RecoveredTo != 1 {
		t.Fatalf("expected only interval 0 to survive, got %d", recovered.RecoveredTo)
	}
	if len(recovered.Invalidations) == 0 || recovered.Invalidations[0].IntervalID != 1 {
		t.Fatalf("expected invalidation from interval 1, got %+v", recovered.Invalidations)
	}
	if !recovered.Complete {
		t.Fatal("recovery did not complete missing suffix")
	}
}

func TestPipelineFrameFailureKeepsCommittedPrefixAndRetries(t *testing.T) {
	ctx := context.Background()
	doc := loadDoc(t)
	root := t.TempDir()
	path := filepath.Join(root, "frame-failure.db")
	options := pipeline.DefaultOptions(path)
	options.Retarget.FailFrames = map[int]string{5: "injected frame failure"}
	report, err := pipeline.Run(ctx, doc, options, pipeline.Load, nil)
	if !model.IsCode(err, model.CodeFrame) {
		t.Fatalf("expected E_FRAME, got %v", err)
	}
	if len(report.Failures) != 1 || report.Failures[0].Frame != 5 {
		t.Fatalf("unexpected failure report: %+v", report.Failures)
	}
	options.Retarget.FailFrames = nil
	recovered, err := pipeline.Run(ctx, doc, options, pipeline.Recovery, nil)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.RecoveredTo != 1 {
		t.Fatalf("expected committed prefix of one interval, got %d", recovered.RecoveredTo)
	}
	reference, err := pipeline.Run(ctx, doc, pipeline.DefaultOptions(filepath.Join(root, "reference.db")), pipeline.Load, nil)
	if err != nil {
		t.Fatal(err)
	}
	if comparison := pipeline.CompareFrames(reference.Frames, recovered.Frames); !comparison.Equal {
		t.Fatalf("retry output differs: %+v", comparison)
	}
}

func TestPrecisionEditRecomputesCacheAndMatchesFull(t *testing.T) {
	ctx := context.Background()
	doc := loadDoc(t)
	root := t.TempDir()
	path := filepath.Join(root, "precision.db")
	base := pipeline.DefaultOptions(path)
	if _, err := pipeline.Run(ctx, doc, base, pipeline.Load, nil); err != nil {
		t.Fatal(err)
	}
	changed := pipeline.DefaultOptions(path)
	changed.Retarget.WorkingScale = 10_000_000
	incremental, err := pipeline.Run(ctx, doc, changed, pipeline.Incremental, []pipeline.Edit{{
		Kind: pipeline.EditPrecision, Reason: "working scale changed to 1e7",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if incremental.Invalidations == nil || incremental.Invalidations[0].IntervalID != 0 {
		t.Fatalf("precision edit must invalidate from interval zero: %+v", incremental.Invalidations)
	}
	fullOptions := pipeline.DefaultOptions(filepath.Join(root, "precision-full.db"))
	fullOptions.Retarget.WorkingScale = 10_000_000
	full, err := pipeline.Run(ctx, doc, fullOptions, pipeline.Load, nil)
	if err != nil {
		t.Fatal(err)
	}
	if comparison := pipeline.CompareFrames(full.Frames, incremental.Frames); !comparison.Equal {
		t.Fatalf("precision edit differs from full recomputation: %+v", comparison)
	}
}

func TestMappingEditInvalidatesMappedTargetSubtree(t *testing.T) {
	ctx := context.Background()
	doc := loadDoc(t)
	root := t.TempDir()
	path := filepath.Join(root, "mapping.db")
	if _, err := pipeline.Run(ctx, doc, pipeline.DefaultOptions(path), pipeline.Load, nil); err != nil {
		t.Fatal(err)
	}
	edited := pipeline.CloneDocument(doc)
	for i := range edited.Retarget.Mappings {
		if edited.Retarget.Mappings[i].Target == "chest_t" {
			edited.Retarget.Mappings[i].RotationScale = model.Ratio{Num: 1, Den: 2}
		}
	}
	options := pipeline.DefaultOptions(path)
	options.Retarget.DisableTruthCheck = true
	incremental, err := pipeline.Run(ctx, edited, options, pipeline.Incremental, []pipeline.Edit{{
		Kind: pipeline.EditMapping, Target: "chest_t", Reason: "chest retarget rotation coefficient changed",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(incremental.Invalidations) == 0 {
		t.Fatal("expected mapping invalidation report")
	}
	for _, joint := range incremental.Invalidations[0].Joints {
		if joint == "root_t" || joint == "spine_t" {
			t.Fatalf("mapping edit should not mark unrelated ancestor target %s", joint)
		}
	}
	fullOptions := pipeline.DefaultOptions(filepath.Join(root, "mapping-full.db"))
	fullOptions.Retarget.DisableTruthCheck = true
	full, err := pipeline.Run(ctx, edited, fullOptions, pipeline.Load, nil)
	if err != nil {
		t.Fatal(err)
	}
	if comparison := pipeline.CompareFrames(full.Frames, incremental.Frames); !comparison.Equal {
		t.Fatalf("mapping edit differs from full recomputation: %+v", comparison)
	}
}
