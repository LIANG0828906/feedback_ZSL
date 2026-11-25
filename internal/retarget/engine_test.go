package retarget_test

import (
	"errors"
	"testing"

	"skeletonretarget/internal/codec"
	"skeletonretarget/internal/generator"
	"skeletonretarget/internal/model"
	"skeletonretarget/internal/pipeline"
	"skeletonretarget/internal/retarget"
)

func loadStudio(t *testing.T) *model.Document {
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

func TestGeneratedFormatsRetargetWithinTruthAndBound(t *testing.T) {
	sample, err := generator.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	hashes := make(map[string]bool)
	for name, data := range map[string][]byte{
		"studio": sample.Studio, "engine": sample.Engine, "legacy": sample.Legacy,
	} {
		doc, err := codec.Load(data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		hashes[doc.ContentHash()] = true
		result, err := retarget.Compute(doc, retarget.DefaultOptions())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(result.Frames) != generator.FrameCount {
			t.Fatalf("%s: got %d frames", name, len(result.Frames))
		}
		var maxRotation, maxPosition int64
		truthSamples := 0
		for _, frame := range result.Frames {
			if len(frame.Normalizations) != len(doc.Skeleton.Joints) {
				t.Fatalf("%s: missing normalization records", name)
			}
			for _, joint := range frame.Joints {
				if joint.Report.ActualDeviationPPM == nil {
					continue
				}
				truthSamples++
				maxRotation = max64(maxRotation, *joint.Report.ActualDeviationPPM)
				maxPosition = max64(maxPosition, *joint.Report.PositionDeviationPPM)
				if *joint.Report.ActualDeviationPPM > joint.Report.BoundPPM {
					t.Fatalf("%s: actual rotation deviation exceeds derived bound at frame %d joint %s", name, frame.Frame, joint.Joint)
				}
				if *joint.Report.ActualDeviationPPM > doc.Tolerance.RotationPPM {
					t.Fatalf("%s: actual rotation deviation exceeds declared tolerance", name)
				}
			}
		}
		if truthSamples == 0 {
			t.Fatalf("%s: no truth samples were evaluated", name)
		}
		t.Logf("%s hash=%s rotationDeviation=%dppm positionDeviation=%dppm", name, doc.ContentHash()[:12], maxRotation, maxPosition)
	}
	if len(hashes) != 1 {
		t.Fatalf("expected one semantic hash, got %d", len(hashes))
	}
}

func TestPrecisionMonotonicity(t *testing.T) {
	doc := loadStudio(t)
	scales := []int64{100_000, 500_000, 1_000_000, 10_000_000}
	var previousBound int64 = 1 << 60
	for _, scale := range scales {
		options := retarget.DefaultOptions()
		options.WorkingScale = scale
		result, err := retarget.Compute(doc, options)
		if err != nil {
			t.Fatalf("scale %d: %v", scale, err)
		}
		var maxActual, maxBound int64
		for _, frame := range result.Frames {
			for _, joint := range frame.Joints {
				maxBound = max64(maxBound, joint.Report.BoundPPM)
				if joint.Report.ActualDeviationPPM != nil {
					maxActual = max64(maxActual, *joint.Report.ActualDeviationPPM)
				}
			}
		}
		if maxActual > doc.Tolerance.RotationPPM {
			t.Fatalf("scale %d: actual %d exceeds tolerance", scale, maxActual)
		}
		if maxBound > previousBound {
			t.Fatalf("bound increased when precision tightened: scale=%d bound=%d previous=%d", scale, maxBound, previousBound)
		}
		t.Logf("scale=%d actual=%dppm bound=%dppm", scale, maxActual, maxBound)
		previousBound = maxBound
	}
}

func TestWorkerBudgetsQueueAndFailureIsolation(t *testing.T) {
	doc := loadStudio(t)
	options := retarget.DefaultOptions()
	options.Parallelism = 2
	options.QueueBudget = 1
	options.JointBufferLimit = 1
	options.FailFrames = map[int]string{7: "injected single-frame failure"}
	result, err := retarget.Compute(doc, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Failures) != 1 || result.Failures[0].Frame != 7 || result.Failures[0].Code != model.CodeFrame {
		t.Fatalf("unexpected failures: %+v", result.Failures)
	}
	if len(result.Frames) != generator.FrameCount-1 {
		t.Fatalf("failure polluted other frames: got %d successful frames", len(result.Frames))
	}
	if result.Queue.MaxJointState > options.JointBufferLimit {
		t.Fatalf("joint state buffer %d exceeded limit %d", result.Queue.MaxJointState, options.JointBufferLimit)
	}
	if result.Queue.FullWaits == 0 || len(result.Queue.Waits) == 0 {
		t.Fatalf("expected queue-budget wait accounting")
	}
	for _, wait := range result.Queue.Waits {
		if wait.Reason == "" || wait.Label == "" || wait.WaitNanos < 0 {
			t.Fatalf("queue wait record is incomplete: %+v", wait)
		}
	}
	options.FailFrames = nil
	retry, err := retarget.Compute(doc, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(retry.Frames) != generator.FrameCount || len(retry.Failures) != 0 {
		t.Fatalf("retry did not recover all frames")
	}
}

func TestErrorChannels(t *testing.T) {
	doc := loadStudio(t)
	mapBroken := cloneDoc(t, doc)
	mapBroken.Retarget.Mappings[0].Target = "missing_target"
	if _, err := retarget.Compute(mapBroken, retarget.DefaultOptions()); !model.IsCode(err, model.CodeMap) {
		t.Fatalf("expected E_MAP, got %v", err)
	}

	normBroken := cloneDoc(t, doc)
	normBroken.Tolerance.NormPPM = 1
	if _, err := retarget.Compute(normBroken, retarget.DefaultOptions()); !model.IsCode(err, model.CodeNorm) {
		t.Fatalf("expected E_NORM, got %v", err)
	} else {
		var appErr *model.Error
		if !errors.As(err, &appErr) || appErr.Joint == "" || appErr.Frame < 0 {
			t.Fatalf("E_NORM lacks joint/frame context: %+v", appErr)
		}
	}

	compBroken := cloneDoc(t, doc)
	compBroken.Clip.Frames[0].Rotations[0].W = compBroken.Clip.Frames[0].Rotations[0].Scale + 1
	if _, err := retarget.Compute(compBroken, retarget.DefaultOptions()); !model.IsCode(err, model.CodeComp) {
		t.Fatalf("expected E_COMP, got %v", err)
	}

	boundBroken := cloneDoc(t, doc)
	for i := range boundBroken.Truth {
		if boundBroken.Truth[i].Joint == "root_t" {
			boundBroken.Truth[i].Rotation.X += boundBroken.Truth[i].Rotation.Scale / 100
			break
		}
	}
	if _, err := retarget.Compute(boundBroken, retarget.DefaultOptions()); !model.IsCode(err, model.CodeBound) {
		t.Fatalf("expected E_BOUND, got %v", err)
	}
}

func cloneDoc(t *testing.T, doc *model.Document) *model.Document {
	t.Helper()
	return pipeline.CloneDocument(doc)
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
