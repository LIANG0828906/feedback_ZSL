package fixed

import (
	"math"
	"testing"
)

func TestScaleConversionsAreExact(t *testing.T) {
	if got := PermilleToPPM(400).Value; got != 400_000 {
		t.Fatalf("permille PPM=%d", got)
	}
	if got := PerMyriadToPPM(1500).Value; got != 150_000 {
		t.Fatalf("per-myriad PPM=%d", got)
	}
	if got := KiBToBytes(100).Value; got != 102_400 {
		t.Fatalf("KiB bytes=%d", got)
	}
	rate := RateBytes(1_024_000_000, 400_000)
	if rate.Value != 409_600_000 || rate.Overflow {
		t.Fatalf("rate bytes=%+v", rate)
	}
}

func TestOverflowSaturatesAndIsObservable(t *testing.T) {
	overflowKiB := int64(math.MaxInt64/1024 + 1)
	result := KiBToBytes(overflowKiB)
	if !result.Overflow || !result.Saturated || result.Value != math.MaxInt64 {
		t.Fatalf("unexpected overflow result: %+v", result)
	}

	rate := PermilleToPPM(math.MaxInt64)
	if !rate.Overflow || rate.Value != MaxRatePPM {
		t.Fatalf("rate saturation wrong: %+v", rate)
	}
}

func TestToleranceBound(t *testing.T) {
	if !WithinTolerance(100, 101, 1) {
		t.Fatal("one-byte difference should be within tolerance")
	}
	if WithinTolerance(100, 102, 1) {
		t.Fatal("two-byte difference should exceed one-byte tolerance")
	}
}
