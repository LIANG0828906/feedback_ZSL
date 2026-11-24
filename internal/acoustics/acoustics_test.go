package acoustics_test

import (
	"math"
	"testing"

	"github.com/gameaudio/workbench/internal/acoustics"
	"github.com/gameaudio/workbench/internal/model"
)

func sampleRegion() model.Region {
	return model.Region{ID: "R1", ReverbTime: 1.2, ERDensity: 24, Gain: 1.0}
}

func TestCurveDeterministicAndSampled(t *testing.T) {
	in := acoustics.Input{Region: sampleRegion(), EffectiveLen: 7.5, Occluded: false}
	a := acoustics.Synthesize(in)
	b := acoustics.Synthesize(in)
	if len(a.Curve) != acoustics.SampleCount {
		t.Fatalf("curve length=%d want %d", len(a.Curve), acoustics.SampleCount)
	}
	for i := range a.Curve {
		if a.Curve[i] != b.Curve[i] {
			t.Fatalf("curve must be deterministic, idx=%d", i)
		}
	}
	// 单调递减（RT60 指数衰减）。
	for i := 1; i < len(a.Curve); i++ {
		if a.Curve[i] >= a.Curve[i-1] {
			t.Fatalf("curve must decay monotonically at idx=%d", i)
		}
	}
}

func TestOcclusionLowersSPL(t *testing.T) {
	base := acoustics.Synthesize(acoustics.Input{
		Region: sampleRegion(), EffectiveLen: 7.5, Occluded: false})
	occluded := acoustics.Synthesize(acoustics.Input{
		Region: sampleRegion(), EffectiveLen: 7.5, Occluded: true})
	if math.Abs((base.SPL-occluded.SPL)-acoustics.OcclusionPenalty) > 1e-9 {
		t.Fatalf("occlusion penalty mismatch: %.4f", base.SPL-occluded.SPL)
	}
}

func TestGainShiftsCurve(t *testing.T) {
	r := sampleRegion()
	low := acoustics.Synthesize(acoustics.Input{Region: r, EffectiveLen: 5})
	r.Gain = 2.0
	high := acoustics.Synthesize(acoustics.Input{Region: r, EffectiveLen: 5})
	if high.Curve[0] <= low.Curve[0] {
		t.Fatal("higher gain must raise curve")
	}
}

func TestEqualCurveLocator(t *testing.T) {
	a := []float64{1, 2, 3, 4}
	b := []float64{1, 2, 9, 4}
	if idx := acoustics.EqualCurve(a, b, 0); idx != 2 {
		t.Fatalf("first mismatch idx=%d want 2", idx)
	}
	if idx := acoustics.EqualCurve(a, a, 0); idx != -1 {
		t.Fatalf("identical curves idx=%d want -1", idx)
	}
}
