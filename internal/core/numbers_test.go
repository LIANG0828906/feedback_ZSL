package core

import (
	"math"
	"testing"
)

func TestTimeConversionUsesDeclaredIntegerRounding(t *testing.T) {
	floor, err := UnixNanosToTicks(-1, 0, 3, RoundFloor)
	if err != nil {
		t.Fatal(err)
	}
	zero, err := UnixNanosToTicks(-1, 0, 3, RoundTowardZero)
	if err != nil {
		t.Fatal(err)
	}
	if floor != -1 || zero != 0 {
		t.Fatalf("rounding floor=%d toward-zero=%d", floor, zero)
	}
	nanos, err := TicksToUnixNanos(floor, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if nanos != -3 {
		t.Fatalf("ticks to nanos = %d, want -3", nanos)
	}
}

func TestTimeConversionOverflow(t *testing.T) {
	if _, err := TicksToUnixNanos(math.MaxInt64/2+1, 0, 2); err == nil {
		t.Fatalf("expected multiplication overflow")
	}
	if _, err := TicksToUnixNanos(math.MaxInt64, 1_700_000_000, 1); err == nil {
		t.Fatalf("expected addition overflow")
	}
	if _, err := UnixNanosToTicks(0, math.MaxInt64, 1, RoundFloor); err == nil {
		t.Fatalf("expected epoch overflow")
	}
}
