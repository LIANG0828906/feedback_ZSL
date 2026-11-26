package dialoguecal

import (
	"math"
	"testing"
)

func TestSampleMillisecondConversionAndResidual(t *testing.T) {
	ms, tr, err := SamplesToMillis(960, 48000, "seg.start", RoundHalfAway)
	if err != nil {
		t.Fatal(err)
	}
	if ms != 20 || tr.Remainder != 0 || tr.Residual != 0 {
		t.Fatalf("exact conversion mismatch: ms=%d trace=%+v", ms, tr)
	}
	back, _, err := MillisToSamples(ms, 48000, "seg.start", RoundHalfAway)
	if err != nil || back != 960 {
		t.Fatalf("exact round trip: back=%d err=%v", back, err)
	}

	ms, tr, err = SamplesToMillis(961, 48000, "seg.start", RoundHalfAway)
	if err != nil {
		t.Fatal(err)
	}
	if ms != 20 || tr.Remainder == 0 || tr.TruncationSite == "" {
		t.Fatalf("expected recorded remainder, got ms=%d trace=%+v", ms, tr)
	}
}

func TestIntegerRoundingDirections(t *testing.T) {
	floor, _, err := SamplesToMillis(1, 3, "x", RoundFloor)
	if err != nil {
		t.Fatal(err)
	}
	half, _, err := SamplesToMillis(1, 3, "x", RoundHalfAway)
	if err != nil {
		t.Fatal(err)
	}
	if floor != 333 || half != 333 {
		t.Fatalf("unexpected positive rounding: floor=%d half=%d", floor, half)
	}
	floor, _, _ = SamplesToMillis(-1, 3, "x", RoundFloor)
	half, _, _ = SamplesToMillis(-1, 3, "x", RoundHalfAway)
	if floor != -334 || half != -333 {
		t.Fatalf("unexpected negative rounding: floor=%d half=%d", floor, half)
	}
	if _, _, err := SamplesToMillis(math.MaxInt64, 1, "x", RoundHalfAway); err == nil {
		t.Fatal("expected multiplication overflow")
	} else {
		requireCode(t, err, CodeOverflow)
	}
}

func TestScaleConversionsAreExactOnProjectGrid(t *testing.T) {
	internal, tr, err := ScaleToPerMyriad(1234, ScalePermille, "gain", RoundHalfAway)
	if err != nil {
		t.Fatal(err)
	}
	if internal != 12340 || tr.Remainder != 0 {
		t.Fatalf("permille conversion: %d %+v", internal, tr)
	}
	back, tr, err := PerMyriadToScale(internal, ScalePermille, "gain", RoundHalfAway)
	if err != nil {
		t.Fatal(err)
	}
	if back != 1234 || tr.Residual != 0 {
		t.Fatalf("exact project-grid round trip: %d %+v", back, tr)
	}
	q, tr, err := PerMyriadToScale(12345, ScalePermille, "target", RoundHalfAway)
	if err != nil {
		t.Fatal(err)
	}
	if q != 1235 || tr.Remainder == 0 {
		t.Fatalf("expected quantized remainder, got %d %+v", q, tr)
	}
}

func TestLinearDBRoundTripCanonicalValues(t *testing.T) {
	for _, linear := range []int64{100, 1000, 1 << 15, 1 << 16, 1 << 17, 1 << 20} {
		rt, err := LinearDBRoundTrip(linear, "amplitude", RoundHalfAway)
		if err != nil {
			t.Fatalf("linear %d: %v", linear, err)
		}
		if rt.Residual != 0 || rt.Linear != linear {
			t.Fatalf("linear %d was not reversible: %+v", linear, rt)
		}
		if rt.Up.TruncationSite == "" || rt.Down.TruncationSite == "" {
			t.Fatalf("linear conversion did not record truncation sites: %+v", rt)
		}
	}
	rt, err := LinearDBRoundTrip(123456789, "amplitude", RoundHalfAway)
	if err != nil {
		t.Fatal(err)
	}
	if rt.Residual == 0 {
		t.Fatal("expected the finite-precision linear/dB path to expose a truncation residual")
	}
	if rt.Up.TruncationSite == "" || rt.Down.TruncationSite == "" {
		t.Fatalf("truncation locations missing: %+v", rt)
	}
}

func TestScaleOverflowReportsOperands(t *testing.T) {
	_, _, err := ScaleToPerMyriad(math.MaxInt64, ScalePermille, "segments.gain", RoundHalfAway)
	ke := requireCode(t, err, CodeOverflow)
	if ke.Field != "segments.gain" || ke.OperandA != math.MaxInt64 || ke.OperandB != ScalePerMyriad {
		t.Fatalf("overflow locators missing: %+v", ke)
	}
}
