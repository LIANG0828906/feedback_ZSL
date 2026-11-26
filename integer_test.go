package strokelab

import (
	"math"
	"testing"
)

func TestIntegerWeightAndCoordinateConversions(t *testing.T) {
	if got, err := PermilleToWeight(800); err != nil || got != 8000 {
		t.Fatalf("permille->weight: %d %v", got, err)
	}
	out, residual, err := WeightToPermille(9995, RoundTowardZero)
	if err != nil || out != 999 || residual != 5 {
		t.Fatalf("toward-zero: out=%d residual=%d err=%v", out, residual, err)
	}
	out, residual, err = WeightToPermille(9995, RoundHalfAwayFromZero)
	if err != nil || out != 1000 || residual != -5 {
		t.Fatalf("half-away: out=%d residual=%d err=%v", out, residual, err)
	}
	encoded, recon, residual, err := AbsToStrokedOffset("x", 11, 0, 2, RoundHalfAwayFromZero)
	if err != nil || encoded != 6 || recon != 12 || residual != -1 {
		t.Fatalf("offset: enc=%d recon=%d residual=%d err=%v", encoded, recon, residual, err)
	}
	abs, err := StrokedOffsetToAbs("x", encoded, 0, 2)
	if err != nil || abs != 12 {
		t.Fatalf("decode: %d %v", abs, err)
	}
	encoded, recon, residual, err = AbsToStrokedOffset("x", 11, 0, 2, RoundTowardZero)
	if err != nil || encoded != 5 || recon != 10 || residual != 1 {
		t.Fatalf("toward-zero offset: enc=%d recon=%d residual=%d err=%v", encoded, recon, residual, err)
	}
}

func TestIntegerConversionOverflowDetails(t *testing.T) {
	_, err := PermilleToWeight(math.MaxInt64)
	if ErrorCode(err) != CodeOverflow {
		t.Fatalf("expected E_OVF, got %v", err)
	}
	ce, ok := err.(*CodedError)
	if !ok {
		t.Fatalf("not CodedError")
	}
	details, ok := ce.Details.(map[string]any)
	if !ok || details["field"] != "weight_permille" {
		t.Fatalf("overflow details missing field/operands: %#v", ce.Details)
	}
	_, err = StrokedOffsetToAbs("x", math.MaxInt64, 0, 2)
	if ErrorCode(err) != CodeOverflow {
		t.Fatalf("expected coordinate E_OVF, got %v", err)
	}
}

func TestAlphaAccumulationIntegerRule(t *testing.T) {
	if got := alphaOver(180, 90); got != 206 {
		t.Fatalf("alpha rule changed: got %d", got)
	}
	if alphaOver(0, 180) == alphaOver(0, 90) {
		t.Fatalf("first source-over intermediate unexpectedly equal")
	}
}
