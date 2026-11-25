package dialogue

import (
	"errors"
	"math"
	"testing"
)

func TestSampleMillisIntegerRoundTrip(t *testing.T) {
	ms, tr, err := SamplesToMillis(48, 24000, RoundFloor, "$.start_sample")
	if err != nil || ms != 2 || tr.Remainder != 0 {
		t.Fatalf("48 samples = %d ms, tr=%+v, err=%v", ms, tr, err)
	}
	samples, _, err := MillisToSamples(2, 24000, RoundNearest, "$.start_ms")
	if err != nil || samples != 48 {
		t.Fatalf("2 ms = %d samples, err=%v", samples, err)
	}

	// 44.1 kHz deliberately has a remainder; the trace must retain it.
	ms44, tr44, err := SamplesToMillis(100, 44100, RoundFloor, "$.sample")
	if err != nil {
		t.Fatal(err)
	}
	if ms44 != 2 || tr44.Remainder != 11800 {
		t.Fatalf("unexpected 44.1k conversion: ms=%d trace=%+v", ms44, tr44)
	}
}

func TestPermilleBasisRoundTrip(t *testing.T) {
	bp, err := PermilleToBasis(123, "$.gain_pm")
	if err != nil || bp != 1230 {
		t.Fatalf("pm->bp = %d,%v", bp, err)
	}
	pm, tr, err := BasisToPermille(1234, RoundNearest, "$.gain_bp")
	if err != nil || pm != 123 || tr.Remainder != 4 {
		t.Fatalf("bp->pm = %d,%+v,%v", pm, tr, err)
	}
}

func TestDBIntegerRoundTrip(t *testing.T) {
	for _, db := range []int64{-40000, -24000, -21412, -600, 0, 600} {
		linear, _, err := DBMilliToLinear(db, "db")
		if err != nil {
			t.Fatal(err)
		}
		back, _, err := LinearToDBMilli(linear, "linear")
		if err != nil {
			t.Fatal(err)
		}
		if delta := abs64(back - db); delta > 2 {
			t.Fatalf("dB drift too large: in=%d linear=%d back=%d delta=%d", db, linear, back, delta)
		}
	}
}

func TestConversionOverflowCode(t *testing.T) {
	_, _, err := SamplesToMillis(math.MaxInt64, 24000, RoundFloor, "$.start_sample")
	var be *Error
	if !errors.As(err, &be) || be.Code != CodeOvf || len(be.Operands) != 2 {
		t.Fatalf("expected E_OVF with two operands, got %#v", err)
	}
	_, err = PermilleToBasis(math.MaxInt64, "$.gain_pm")
	if !errors.As(err, &be) || be.Code != CodeOvf {
		t.Fatalf("expected E_OVF from permille conversion, got %#v", err)
	}
}
