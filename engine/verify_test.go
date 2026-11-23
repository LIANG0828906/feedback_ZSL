package engine

import (
	"testing"

	"loudnessworkbench/dsp"
	"loudnessworkbench/project"
)

func TestToleranceSweepIsMonotonic(t *testing.T) {
	result := &Result{Segments: []*SegmentResult{{
		SourceMeasurement: dsp.Measurement{IntegratedLUFS: -23.01, TruePeakDBTP: -3.01},
	}}}
	manifest := &project.Manifest{Segments: []project.ManifestSegment{{
		Truth: project.SegmentTruth{IntegratedLUFS: -23, TruePeakDBTP: -3},
	}}}
	points := ToleranceSweep(result, manifest, []float64{0.02, 0.011, 0.005})
	if points[0].SegmentsWithin != 1 || points[1].SegmentsWithin != 1 || points[2].SegmentsWithin != 0 {
		t.Fatalf("unexpected sweep: %+v", points)
	}
	for i := 1; i < len(points); i++ {
		if points[i].SegmentsWithin > points[i-1].SegmentsWithin {
			t.Fatalf("tolerance sweep increased at %d: %+v", i, points)
		}
	}
}
