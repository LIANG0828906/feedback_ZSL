package units_test

import (
	"math"
	"testing"

	"lipsync/domain"
	"lipsync/units"
)

func TestFrameConversionsUseDeclaredRoundingAndRemainders(t *testing.T) {
	tests := []struct {
		samples int64
		round   units.Rounding
		want    int64
	}{
		{samples: 1000, round: units.RoundFloor, want: 1},
		{samples: 1000, round: units.RoundCeil, want: 2},
		{samples: 1000, round: units.RoundNearest, want: 1},
		{samples: 1500, round: units.RoundNearest, want: 2},
		{samples: 48000, round: units.RoundFloor, want: 50},
	}
	for _, test := range tests {
		conversion, err := units.SamplesToFrames(test.samples, 48000, 50, test.round, "phoneme.start_sample")
		if err != nil {
			t.Fatal(err)
		}
		if conversion.Output != test.want {
			t.Fatalf("%d samples %s: got %d want %d", test.samples, test.round, conversion.Output, test.want)
		}
		if test.samples%960 != 0 && !conversion.Truncated() {
			t.Fatalf("expected truncation metadata for %d samples", test.samples)
		}
		var truncations []domain.Truncation
		units.RecordTruncation(&truncations, "phoneme.start_sample", "samples", "frames", conversion, "ch1", "pkt1")
		if test.samples%960 != 0 {
			if len(truncations) != 1 || truncations[0].Remainder != conversion.Remainder {
				t.Fatalf("truncation position not recorded: %+v", truncations)
			}
		}
	}
}

func TestMillisecondAndBudgetConversions(t *testing.T) {
	ms, err := units.MillisToFrames(25, 50, units.RoundFloor, "dialogue.start_ms")
	if err != nil {
		t.Fatal(err)
	}
	if ms.Output != 1 || ms.Remainder != 250 {
		t.Fatalf("unexpected ms conversion: %+v", ms)
	}
	permilleToBP, err := units.PermilleToBasisPoints(322, units.RoundFloor, "budget")
	if err != nil {
		t.Fatal(err)
	}
	if permilleToBP.Output != 3220 || permilleToBP.Truncated() {
		t.Fatalf("unexpected permille conversion: %+v", permilleToBP)
	}
	bpToPermille, err := units.BasisPointsToPermille(4567, units.RoundFloor, "budget")
	if err != nil {
		t.Fatal(err)
	}
	if bpToPermille.Output != 456 || bpToPermille.Remainder != 7 {
		t.Fatalf("unexpected downgrade conversion: %+v", bpToPermille)
	}
	back, err := units.PermilleToBasisPoints(bpToPermille.Output, units.RoundFloor, "budget")
	if err != nil {
		t.Fatal(err)
	}
	if residual := int64(4567) - back.Output; residual < 0 || residual > 9 {
		t.Fatalf("round-trip residual %d exceeds one permille", residual)
	}
	for value := int64(0); value <= 10000; value += 37 {
		down, err := units.BasisPointsToPermille(value, units.RoundFloor, "budget")
		if err != nil {
			t.Fatal(err)
		}
		up, err := units.PermilleToBasisPoints(down.Output, units.RoundFloor, "budget")
		if err != nil {
			t.Fatal(err)
		}
		if residual := value - up.Output; residual < 0 || residual > 9 {
			t.Fatalf("budget %d residual %d out of range", value, residual)
		}
	}
}

func TestOverflowReturnsOperands(t *testing.T) {
	conversions := []func() error{
		func() error {
			_, err := units.SamplesToFrames(math.MaxInt64, 48000, 50, units.RoundFloor, "dialogue.start_sample")
			return err
		},
		func() error {
			_, err := units.MillisToFrames(math.MaxInt64, 50, units.RoundFloor, "dialogue.start_ms")
			return err
		},
		func() error {
			_, err := units.PermilleToBasisPoints(math.MaxInt64, units.RoundFloor, "channel.frame_budget")
			return err
		},
	}
	for i, run := range conversions {
		err := run()
		if domain.CodeOf(err) != domain.CodeOverflow {
			t.Fatalf("case %d: expected E_OVF, got %v", i, err)
		}
		var domainErr *domain.Error
		if !domainAs(err, &domainErr) {
			t.Fatalf("case %d: expected structured error", i)
		}
		if domainErr.Field == "" || domainErr.OperandA == 0 || domainErr.OperandB == 0 {
			t.Fatalf("case %d: overflow lacks field/operands: %+v", i, domainErr)
		}
	}
}

func domainAs(err error, target **domain.Error) bool {
	for err != nil {
		if typed, ok := err.(*domain.Error); ok {
			*target = typed
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}
