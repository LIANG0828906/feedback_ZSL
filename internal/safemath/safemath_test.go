package safemath

import (
	"math"
	"testing"

	"mirrorcast/internal/bizerror"
)

func TestCheckedMulOverflow(t *testing.T) {
	_, err := Mul(math.MaxInt64, 2)
	if bizerror.CodeOf(err) != bizerror.EOverflow {
		t.Fatalf("code=%q want E_OVERFLOW", bizerror.CodeOf(err))
	}
}

func TestMulDivRoundHalfUp(t *testing.T) {
	got, err := MulDivRound(3, 2500, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 { // 0.75 rounds up
		t.Fatalf("MulDivRound = %d, want 1", got)
	}
}

func TestSaturateInt32(t *testing.T) {
	got, saturated := SaturateInt32(math.MaxInt64)
	if !saturated || got != math.MaxInt32 {
		t.Fatalf("got %d saturated=%v", got, saturated)
	}
	got, saturated = SaturateInt32(180)
	if saturated || got != 180 {
		t.Fatalf("unexpected saturation: %d %v", got, saturated)
	}
}
