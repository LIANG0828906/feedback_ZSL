package convert

import (
	"math"
	"testing"

	"battlekernel/internal/errs"
)

func TestAlignedRoundTripPreservesRoundAndTimestamp(t *testing.T) {
	const epoch = int64(1760000000000)
	ts, err := RoundIndexToMillis(4, 1, epoch, 250, "round")
	if err != nil {
		t.Fatal(err)
	}
	round, canonical, err := CanonicalRoundTrip(ts, 1, epoch, 250, RoundFloor, "ts")
	if err != nil {
		t.Fatal(err)
	}
	if round != 4 || canonical != ts {
		t.Fatalf("round=%d canonical=%d ts=%d", round, canonical, ts)
	}
}

func TestFloorCanonicalizesQueueBucket(t *testing.T) {
	const epoch = int64(1760000000000)
	round, canonical, err := CanonicalRoundTrip(epoch+2999, 1, epoch, 1000, RoundFloor, "ts")
	if err != nil {
		t.Fatal(err)
	}
	if round != 3 || canonical != epoch+2000 {
		t.Fatalf("round=%d canonical=%d", round, canonical)
	}
}

func TestOverflowReturnsEOVFWithValues(t *testing.T) {
	_, err := RoundIndexToMillis(math.MaxInt64, 1, 0, 1000, "probe.round")
	if !errs.IsCode(err, errs.EOVF) {
		t.Fatalf("expected E_OVF, got %v", err)
	}
	ke := err.(*errs.Error)
	if ke.Field != "probe.round" || len(ke.Values) != 2 {
		t.Fatalf("bad overflow metadata: %#v", ke)
	}
}
