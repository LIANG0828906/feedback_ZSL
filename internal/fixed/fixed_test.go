package fixed_test

import (
	"testing"

	"skeletonretarget/internal/fixed"
	"skeletonretarget/internal/model"
)

func TestNormalizeAndRoundTripStayWithinDeclaredTolerance(t *testing.T) {
	original := model.Quat{W: 9238795, X: 3826834, Y: 0, Z: 0, Scale: 10_000_000}
	normalized, err := fixed.NormalizeChecked(original, 10, model.RoundHalfUp)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.NormResidualPPM > 10 {
		t.Fatalf("residual %dppm exceeds tolerance", normalized.NormResidualPPM)
	}
	if len(normalized.Truncations) != 4 {
		t.Fatalf("expected four component truncation records, got %d", len(normalized.Truncations))
	}
	roundTrip, truncs := fixed.QuantizeRoundTrip(original, 100_000, model.RoundHalfUp)
	if got := fixed.QuatDeltaPPM(original, roundTrip); got > 20 {
		t.Fatalf("round-trip component drift %dppm exceeds 20ppm", got)
	}
	if got := fixed.NormPPM(roundTrip); got > 10 {
		t.Fatalf("round-trip norm drift %dppm exceeds 10ppm", got)
	}
	if len(truncs) != 8 {
		t.Fatalf("expected eight truncation records, got %d", len(truncs))
	}
}

func TestNormalizeRejectsOutOfRangeComponent(t *testing.T) {
	_, err := fixed.Normalize(model.Quat{W: 1_000_001, Scale: 1_000_000}, model.RoundHalfUp)
	if err == nil || err.Code != model.CodeComp {
		t.Fatalf("expected E_COMP, got %v", err)
	}
}

func TestTimeConversionRecordsRemainderAndDirection(t *testing.T) {
	ms, trunc := fixed.FrameToMS(1, 24, model.RoundHalfUp)
	if ms != 42 || trunc.Remainder != 16 || trunc.Divisor != 24 {
		t.Fatalf("unexpected half-up conversion: ms=%d trunc=%+v", ms, trunc)
	}
	floor, floorTrunc := fixed.FrameToMS(1, 24, model.RoundFloor)
	if floor != 41 || floorTrunc.Remainder != 16 {
		t.Fatalf("unexpected floor conversion: ms=%d trunc=%+v", floor, floorTrunc)
	}
	frame, back := fixed.MSToFrame(ms, 24, model.RoundHalfUp)
	if frame != 1 || back.Stage != "ms_to_frame" {
		t.Fatalf("unexpected reverse conversion: frame=%d trunc=%+v", frame, back)
	}
}

func TestQuaternionMultiplicationIdentity(t *testing.T) {
	q := model.Quat{W: 9238795, X: 3826834, Scale: 10_000_000}
	product, _, err := fixed.Mul(fixed.Identity(q.Scale), q, model.RoundHalfUp)
	if err != nil {
		t.Fatal(err)
	}
	if drift := fixed.QuatDeltaPPM(q, product); drift > 1 {
		t.Fatalf("identity multiplication drifted by %dppm", drift)
	}
}

func TestSignedDivisionModes(t *testing.T) {
	tests := []struct {
		numerator int64
		mode      model.RoundMode
		want      int64
	}{
		{-6, model.RoundFloor, -2},
		{-7, model.RoundFloor, -3},
		{-7, model.RoundCeil, -2},
		{-6, model.RoundCeil, -2},
		{-7, model.RoundToward, -2},
		{-5, model.RoundHalfUp, -2},
		{-4, model.RoundHalfUp, -1},
	}
	for _, test := range tests {
		got, _ := fixed.DivRound(test.numerator, 3, test.mode)
		if got != test.want {
			t.Fatalf("DivRound(%d,3,%s)=%d want %d", test.numerator, test.mode, got, test.want)
		}
	}
}
