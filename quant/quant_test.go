package quant_test

import (
	"math"
	"testing"

	"lipsync/domain"
	"lipsync/quant"
)

func TestIntegerConversionsAndRemainders(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func() (quant.Conversion, error)
		want int64
		rem  int64
	}{
		{"sample-floor", func() (quant.Conversion, error) {
			return quant.SampleToFrame(1440, 48000, 50, domain.RoundFloor, "start")
		}, 1, 24000},
		{"sample-ceil", func() (quant.Conversion, error) {
			return quant.SampleToFrame(1440, 48000, 50, domain.RoundCeil, "start")
		}, 2, 24000},
		{"sample-nearest", func() (quant.Conversion, error) {
			return quant.SampleToFrame(1440, 48000, 50, domain.RoundNearest, "start")
		}, 2, 24000},
		{"millis-floor", func() (quant.Conversion, error) {
			return quant.MillisToFrame(25, 50, domain.RoundFloor, "start_ms")
		}, 1, 250},
		{"millis-ceil", func() (quant.Conversion, error) {
			return quant.MillisToFrame(25, 50, domain.RoundCeil, "start_ms")
		}, 2, 250},
		{"frame-to-ms", func() (quant.Conversion, error) {
			return quant.FrameToMillis(1, 50, domain.RoundFloor, "start_ms")
		}, 20, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.fn()
			if err != nil {
				t.Fatal(err)
			}
			if got.Value != tc.want || got.Remainder != tc.rem {
				t.Fatalf("value/remainder = %d/%d, want %d/%d", got.Value, got.Remainder, tc.want, tc.rem)
			}
			if got.Truncated != (tc.rem != 0) {
				t.Fatalf("truncated = %v", got.Truncated)
			}
		})
	}
}

func TestWeightRoundTripExactForRepresentableValues(t *testing.T) {
	high, err := quant.WeightToPermyriad(1800, 10000, domain.RoundFloor, "weight")
	if err != nil {
		t.Fatal(err)
	}
	low, err := quant.WeightFromPermyriad(high.Value, 1000, domain.RoundFloor, "weight")
	if err != nil {
		t.Fatal(err)
	}
	back, err := quant.WeightToPermyriad(low.Value, 1000, domain.RoundFloor, "weight")
	if err != nil {
		t.Fatal(err)
	}
	if low.Value != 180 || low.Truncated || back.Value != 1800 {
		t.Fatalf("roundtrip low=%d truncated=%v back=%d", low.Value, low.Truncated, back.Value)
	}
	lossy, err := quant.WeightFromPermyriad(1805, 1000, domain.RoundFloor, "weight")
	if err != nil {
		t.Fatal(err)
	}
	if !lossy.Truncated || lossy.Value != 180 || lossy.Remainder != 5000 {
		t.Fatalf("expected recorded truncation, got %+v", lossy)
	}
}

func TestOverflowIncludesFieldAndOperands(t *testing.T) {
	_, err := quant.SampleToFrame(math.MaxInt64, 48000, 50, domain.RoundFloor, "phoneme.start_sample")
	if domain.ErrorCode(err) != domain.CodeOverflow {
		t.Fatalf("code = %s, want E_OVF: %v", domain.ErrorCode(err), err)
	}
	de, _ := domain.AsError(err)
	if de.Field != "phoneme.start_sample" || de.OperandA != math.MaxInt64 || de.OperandB != 50 {
		t.Fatalf("overflow context = %+v", de)
	}
}

func TestFrameAndMillisRoundTripAtProjectRate(t *testing.T) {
	for frame := int64(0); frame < 1000; frame++ {
		ms, err := quant.FrameToMillis(frame, 50, domain.RoundFloor, "f")
		if err != nil {
			t.Fatal(err)
		}
		back, err := quant.MillisToFrame(ms.Value, 50, domain.RoundFloor, "m")
		if err != nil {
			t.Fatal(err)
		}
		if back.Value != frame || ms.Truncated || back.Truncated {
			t.Fatalf("frame %d -> %dms -> %d", frame, ms.Value, back.Value)
		}
	}
}
