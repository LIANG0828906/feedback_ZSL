package quant

import (
	"math"
	"testing"

	"presentkernel/internal/domain"
)

func TestIntegerConversionsAndRoundTrip(t *testing.T) {
	frame, rem, err := MsToFrames("ms", 125, 24, RoundHalfUp)
	if err != nil || frame != 3 {
		t.Fatalf("125ms@24 = %d rem=%+v err=%v", frame, rem, err)
	}
	frame, _, err = SecondsToFrames("sec", "0.125", 24, RoundHalfUp)
	if err != nil || frame != 3 {
		t.Fatalf(".125s@24 = %d err=%v", frame, err)
	}
	frame, _, err = SamplesToFrames("samples", 6000, 48000, 24, RoundFloor)
	if err != nil || frame != 3 {
		t.Fatalf("6000 samples = %d err=%v", frame, err)
	}

	v, _, err := ScaleFixedPoint("weight", 123, ScalePermille, ScalePermyriad, RoundFloor)
	if err != nil || v != 1230 {
		t.Fatalf("permille->permyriad = %d err=%v", v, err)
	}
	back, r, err := ScaleFixedPoint("weight", v, ScalePermyriad, ScalePermille, RoundFloor)
	if err != nil || back != 123 || r.Remainder != 0 {
		t.Fatalf("round trip = %d rem=%+v err=%v", back, r, err)
	}

	_, rem, err = MsToFrames("ms.floor", 100, 24, RoundFloor)
	if err != nil {
		t.Fatal(err)
	}
	if rem.Result != 2 || rem.Remainder != 400 {
		t.Fatalf("unexpected truncation trace: %+v", rem)
	}
}

func TestOverflowReturnsStructuredEOVF(t *testing.T) {
	_, _, err := ScaleFixedPoint("project.weights[0]", math.MaxInt64, ScalePermille, ScalePermyriad, RoundFloor)
	de, ok := domain.AsError(err)
	if !ok || de.Code != domain.CodeOvf {
		t.Fatalf("expected E_OVF, got %T %v", err, err)
	}
	if de.Path != "project.weights[0]" || len(de.Operands) != 2 {
		t.Fatalf("overflow error missing field/operands: %+v", de)
	}
}
