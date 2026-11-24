package geometry_test

import (
	"testing"

	"occlusionlab/internal/generator"
	"occlusionlab/internal/geometry"
	"occlusionlab/internal/model"
)

func testParams(project *model.Project) model.Params {
	return model.Params{
		Quantization:      project.Meta.Precision,
		RasterResolution:  128,
		MaxWorkers:        2,
		RasterBufferCells: 128 * 128,
		MaxResident:       15,
		LoadCostBudget:    1000,
		TolerancePxQ:      project.Meta.TolerancePxQ,
		Version:           1,
	}
}

func TestProjectionTruthAndVisibility(t *testing.T) {
	project, appErr := generator.Generate(generator.DefaultConfig(1042))
	if appErr != nil {
		t.Fatal(appErr)
	}
	params := testParams(project)
	var errors []int64
	for window, truthSample := range project.Truth.Samples {
		result, appErr := geometry.EvaluateSample(project, params, window)
		if appErr != nil {
			t.Fatal(appErr)
		}
		for _, kp := range project.Truth.KnownProjections {
			if kp.Window != window {
				continue
			}
			verdict := result.Chunks[kp.Chunk]
			errQ := maxAbs(verdict.Anchor.XQ-kp.ExpectedX, verdict.Anchor.YQ-kp.ExpectedY)
			errors = append(errors, errQ)
			if errQ > project.Truth.TolerancePxQ {
				t.Fatalf("window %d chunk %d projection error %d exceeds tolerance %d",
					window, kp.Chunk, errQ, project.Truth.TolerancePxQ)
			}
			if errQ > verdict.Error.ProjectionErrorQ {
				t.Fatalf("window %d chunk %d observed error %d exceeds bound %d",
					window, kp.Chunk, errQ, verdict.Error.ProjectionErrorQ)
			}
		}
		for _, truthChunk := range truthSample.Chunks {
			verdict := result.Chunks[truthChunk.Chunk]
			if truthChunk.Visible && verdict.Status == geometry.StatusOccluded {
				t.Fatalf("window %d chunk %d truth-visible but occluded (%s)",
					window, truthChunk.Chunk, verdict.Reason)
			}
			if !truthChunk.Visible && verdict.Status == geometry.StatusVisible {
				t.Fatalf("window %d chunk %d truth-hidden but visible (%s)",
					window, truthChunk.Chunk, verdict.Reason)
			}
			if verdict.Status == geometry.StatusUncertain {
				if verdict.VisibilityLowerQ != 0 || verdict.VisibilityUpperQ != 65536 {
					t.Fatalf("window %d chunk %d uncertain bounds invalid", window, truthChunk.Chunk)
				}
				if verdict.Error.VisibilityUpperBoundQ <= 0 {
					t.Fatalf("window %d chunk %d cumulative bound must be positive", window, truthChunk.Chunk)
				}
			}
			if verdict.Status == geometry.StatusOccluded && verdict.ReferenceOccluder == "" {
				t.Fatalf("window %d chunk %d occluded without reference", window, truthChunk.Chunk)
			}
		}
	}
	counts := toleranceCounts(errors, project.Meta.TolerancePxQ)
	for i := 1; i < len(counts); i++ {
		if counts[i] > counts[i-1] {
			t.Fatalf("tolerance tightening is not monotonic: %v", counts)
		}
	}
}

func toleranceCounts(errors []int64, declared int64) []int64 {
	thresholds := []int64{declared, declared / 2, declared / 4, declared / 8}
	out := make([]int64, 0, len(thresholds))
	for _, threshold := range thresholds {
		var count int64
		for _, err := range errors {
			if err <= threshold {
				count++
			}
		}
		out = append(out, count)
	}
	return out
}

func maxAbs(a, b int64) int64 {
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	if a > b {
		return a
	}
	return b
}
