package loudness

import (
	"math"
	"testing"
)

func TestMulDivRoundingResidualAndOverflow(t *testing.T) {
	value, trace, err := MulDiv(5, 1, 2, RoundHalfUp)
	if err != nil {
		t.Fatal(err)
	}
	if value != 3 || trace.Remainder >= 0 || trace.Residual >= 0 {
		t.Fatalf("half-up: value=%d trace=%+v", value, trace)
	}
	value, trace, err = MulDiv(-5, 1, 2, RoundHalfUp)
	if err != nil {
		t.Fatal(err)
	}
	if value != -3 || trace.Remainder != 1 {
		t.Fatalf("negative half-up: value=%d trace=%+v", value, trace)
	}
	value, trace, err = MulDiv(-5, 1, 2, RoundDown)
	if err != nil {
		t.Fatal(err)
	}
	if value != -2 || trace.Remainder != -1 || trace.Residual != -1 {
		t.Fatalf("negative toward-zero: value=%d trace=%+v", value, trace)
	}
	_, _, err = MulDiv(math.MaxInt64, 2, 1, RoundDown)
	if !HasCode(err, EOvF) {
		t.Fatalf("expected E_OVF, got %v", err)
	}
	domainErr, ok := AsDomainError(err)
	if !ok || len(domainErr.Operands) != 2 || domainErr.Operands[0] != math.MaxInt64 || domainErr.Operands[1] != 2 {
		t.Fatalf("overflow diagnostics: %+v", domainErr)
	}
}

func TestTimeAndScaleRoundTrips(t *testing.T) {
	samples := int64(1234)
	millis, trace, err := SamplesToMillis(samples, 8000, RoundHalfUp)
	if err != nil {
		t.Fatal(err)
	}
	if trace.Quotient != millis {
		t.Fatalf("trace quotient %d != %d", trace.Quotient, millis)
	}
	roundTrip, _, err := MillisToSamples(millis, 8000, RoundHalfUp)
	if err != nil {
		t.Fatal(err)
	}
	if absInt64(roundTrip-samples) > 4 {
		t.Fatalf("time round-trip %d -> %d -> %d", samples, millis, roundTrip)
	}
	if trace.Remainder == 0 {
		t.Fatal("expected a declared non-zero time remainder")
	}

	gainBasisPoints, _, err := ConvertScale(InternalGainScale, InternalGainScale, 10000, "$.gain", RoundHalfUp)
	if err != nil || gainBasisPoints != 10000 {
		t.Fatalf("gain to bp: %d %v", gainBasisPoints, err)
	}
	gainBack, _, err := ConvertScale(gainBasisPoints, 10000, InternalGainScale, "$.gain", RoundHalfUp)
	if err != nil || gainBack != InternalGainScale {
		t.Fatalf("gain round-trip: %d %v", gainBack, err)
	}
	loudnessCenti, _, err := ConvertScale(-30*InternalLoudnessScale, InternalLoudnessScale, 100, "$.loudness", RoundHalfUp)
	if err != nil || loudnessCenti != -3000 {
		t.Fatalf("loudness to centi-dB: %d %v", loudnessCenti, err)
	}
	loudnessBack, _, err := ConvertScale(loudnessCenti, 100, InternalLoudnessScale, "$.loudness", RoundHalfUp)
	if err != nil || loudnessBack != -30*InternalLoudnessScale {
		t.Fatalf("loudness round-trip: %d %v", loudnessBack, err)
	}
}

func TestLinearDBRoundTrip(t *testing.T) {
	for _, db := range []int64{-12000, -6000, -3072, -1541, -512, 0, 512, 1541, 3072, 6000} {
		linear, trace, err := DBQ8ToLinearQ16(db, "$.db")
		if err != nil {
			t.Fatalf("DBQ8ToLinearQ16(%d): %v", db, err)
		}
		if linear <= 0 || trace.Quotient != linear {
			t.Fatalf("db %d linear %d trace %+v", db, linear, trace)
		}
		back, _, err := LinearToDBQ16(linear, "$.linear")
		if err != nil {
			t.Fatalf("LinearToDBQ16(%d): %v", linear, err)
		}
		if absInt64(back-db) > DBRoundTripToleranceQ8 {
			t.Fatalf("dB round-trip %d -> linear %d -> %d", db, linear, back)
		}
	}
	fullScaleDB, _, err := LinearToDBQ16(InternalLinearScale, "$.full")
	if err != nil {
		t.Fatal(err)
	}
	if absInt64(fullScaleDB) > 1 {
		t.Fatalf("full-scale dB = %d, want 0", fullScaleDB)
	}
	halfScaleDB, _, err := LinearToDBQ16(InternalLinearScale/2, "$.half")
	if err != nil {
		t.Fatal(err)
	}
	if absInt64(halfScaleDB+1541) > 2 {
		t.Fatalf("half-scale dB = %d, want about -1541", halfScaleDB)
	}
}
