package dialogue

import (
	"math"
	"testing"
)

const (
	truthEnergyTolerance = int64(2)
	truthZCRToleranceBP  = int64(1)
	truthPeakTolerance   = int64(0)
)

func TestFrameFeaturesMatchGeneratorTruth(t *testing.T) {
	p, truth, _ := sampleProject(t)
	rows, pErr := FullBuild(p)
	if pErr != nil {
		t.Fatal(pErr)
	}
	expected := truthFeatureMap(truth)
	if len(rows) != len(expected) {
		t.Fatalf("frame count %d != truth %d", len(rows), len(expected))
	}
	for _, row := range rows {
		want, ok := expected[row.RangeID]
		if !ok {
			t.Fatalf("missing truth for %s", row.RangeID)
		}
		if abs64(row.Feature.EnergyFixed-want.EnergyFixed) > truthEnergyTolerance {
			t.Fatalf("%s energy=%d truth=%d", row.RangeID, row.Feature.EnergyFixed, want.EnergyFixed)
		}
		if abs64(row.Feature.ZCRBP-want.ZCRBP) > truthZCRToleranceBP {
			t.Fatalf("%s zcr=%d truth=%d", row.RangeID, row.Feature.ZCRBP, want.ZCRBP)
		}
		if abs64(row.Feature.PeakSample-want.PeakSample) > truthPeakTolerance {
			t.Fatalf("%s peak sample=%d truth=%d", row.RangeID, row.Feature.PeakSample, want.PeakSample)
		}
		if row.Feature.Transient != want.Transient {
			t.Fatalf("%s transient flag=%v truth=%v", row.RangeID, row.Feature.Transient, want.Transient)
		}
		if len(row.Feature.Truncations) < 2 {
			t.Fatalf("%s expected integer truncation traces", row.RangeID)
		}
	}
}

func TestSilenceIsNotTransientAndSampleTruthMatches(t *testing.T) {
	p, truth, _ := sampleProject(t)
	rows, pErr := FullBuild(p)
	if pErr != nil {
		t.Fatal(pErr)
	}
	for _, clipTruth := range truth.Clips {
		silenceTruth := clipTruth.Features[2]
		var silenceRow *FrameResult
		for i := range rows {
			if rows[i].RangeID == silenceTruth.RangeID {
				silenceRow = &rows[i]
				break
			}
		}
		if silenceRow == nil {
			t.Fatalf("missing silence frame %s", silenceTruth.RangeID)
		}
		if silenceRow.Feature.Transient {
			t.Fatalf("silence frame %s falsely marked transient", silenceRow.RangeID)
		}
		if silenceRow.Feature.EnergyFixed != 0 || silenceRow.Feature.RMSMicro != 0 {
			t.Fatalf("silence frame %s energy/rms nonzero", silenceRow.RangeID)
		}
		for _, sample := range clipTruth.SampleChecks {
			frame := FrameOfSample(sample.SampleNo, p.SampleRate, p.FrameRate)
			if frame != sample.FrameNo {
				t.Fatalf("sample %d frame=%d truth=%d", sample.SampleNo, frame, sample.FrameNo)
			}
			clip, _ := findClip(p, clipTruth.ClipID)
			pcm := clip.Samples[sample.ClipSample]
			amp, _ := SampleToAmpMicro(pcm, "test")
			if amp != sample.AmpMicro {
				t.Fatalf("sample %d amp=%d truth=%d", sample.SampleNo, amp, sample.AmpMicro)
			}
		}
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func TestTighterTolerancePassRateNeverIncreases(t *testing.T) {
	p, truthManifest, _ := sampleProject(t)
	rows, pErr := FullBuild(p)
	if pErr != nil {
		t.Fatal(pErr)
	}
	truth := truthFeatureMap(truthManifest)
	tolerances := []int64{10_000, 1_000, 10, 0}
	previous := 2.0
	for _, tol := range tolerances {
		rate := energyPassRate(rows, truth, tol)
		if rate > previous+math.Pow10(-9) {
			t.Fatalf("pass rate increased while tolerance tightened: tol=%d rate=%f prev=%f", tol, rate, previous)
		}
		previous = rate
	}
}

func TestFrameAttributionIsTraceableAfterFPSChange(t *testing.T) {
	const sample = int64(7200)
	attr := AttributeSampleChange(sample, 25, 30, 24000)
	if attr.OldFrameNo != FrameOfSample(sample, 24000, 25) ||
		attr.NewFrameNo != FrameOfSample(sample, 24000, 30) {
		t.Fatalf("attribution mismatch: %+v", attr)
	}
	if !attr.Changed {
		t.Fatalf("expected 25->30 fps to move sample 1440 to a new frame: %+v", attr)
	}
}
