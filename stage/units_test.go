package stage

import (
	"errors"
	"testing"
)

func TestIntegerConversionsAndResiduals(t *testing.T) {
	for _, v := range []int64{45_000_000, -10_000_000, 1_234_567} {
		rad, rem1, err := MicroDegToMicroRad(v)
		if err != nil {
			t.Fatal(err)
		}
		back, rem2, err := MicroRadToMicroDeg(rad)
		if err != nil {
			t.Fatal(err)
		}
		// One micro-radian is about 57.3 micro-degrees, so a nearest-rounded
		// micro-radian representation has a declared worst-case residual below
		// 30 micro-degrees (0.00003 degrees).
		if abs64(back-v) > 30 {
			t.Fatalf("angle roundtrip %d -> %d -> %d (residuals %d,%d)", v, rad, back, rem1, rem2)
		}
	}
	for _, ms := range []int64{0, 100, 8, 17, 400} {
		tick, _, err := MSToTick(ms, 120)
		if err != nil {
			t.Fatal(err)
		}
		back, _, err := TickToMS(tick, 120)
		if err != nil {
			t.Fatal(err)
		}
		if abs64(back-ms) > 5 {
			t.Fatalf("time roundtrip %d -> %d -> %d", ms, tick, back)
		}
	}
	for _, pm := range []int64{0, 1, 999, 1000} {
		my, _, err := PermilleToPermyriad(pm)
		if err != nil {
			t.Fatal(err)
		}
		back, rem, err := PermyriadToPermille(my)
		if err != nil {
			t.Fatal(err)
		}
		if back != pm || rem != 0 {
			t.Fatalf("ratio roundtrip %d -> %d -> %d rem=%d", pm, my, back, rem)
		}
	}
	for _, kelvin := range []int64{CCTMinKelvin, 3840, 5880, CCTMaxKelvin} {
		pm, _, err := CCTKelvinToPermille(kelvin)
		if err != nil {
			t.Fatal(err)
		}
		back, _, err := CCTPermilleToKelvin(pm)
		if err != nil {
			t.Fatal(err)
		}
		if abs64(back-kelvin) > 6 {
			t.Fatalf("cct permille roundtrip %d -> %d -> %d", kelvin, pm, back)
		}
		my, _, err := CCTKelvinToPermyriad(kelvin)
		if err != nil {
			t.Fatal(err)
		}
		back, _, err = CCTPermyriadToKelvin(my)
		if err != nil {
			t.Fatal(err)
		}
		if abs64(back-kelvin) > 1 {
			t.Fatalf("cct permyriad roundtrip %d -> %d -> %d", kelvin, my, back)
		}
	}
}

func TestOverflowReportsFieldAndOperands(t *testing.T) {
	_, _, err := ScaleInt(1<<62, 8, 1, RoundExact, "test.overflow")
	if !errors.Is(err, ErrOverflow) {
		t.Fatalf("expected E_OVF, got %v", err)
	}
	de := AsError(err)
	if de.Field != "test.overflow" || len(de.Operands) != 2 {
		t.Fatalf("missing overflow context: %+v", de)
	}
}
