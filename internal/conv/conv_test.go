package conv

import (
	"math"
	"reflect"
	"testing"

	"msworkbench/internal/e"
)

func TestScaleExposureGainIntegerRoundTrip(t *testing.T) {
	rec := &Recorder{}
	bp, err := PermilleToPermyriad(123, rec)
	if err != nil || bp != 1230 {
		t.Fatalf("permille -> bp = %d,%v", bp, err)
	}
	back, rem, err := PermyriadToPermille(1234, rec)
	if err != nil || back != 123 || rem != 4 {
		t.Fatalf("bp -> permille = %d rem %d err %v", back, rem, err)
	}

	ms, rem, err := MicroToMilli(8250, rec)
	if err != nil || ms != 8 || rem != 250 {
		t.Fatalf("us -> ms = %d rem %d err %v", ms, rem, err)
	}
	us, err := MilliToMicro(8)
	if err != nil || us != 8000 {
		t.Fatalf("ms -> us = %d err %v", us, err)
	}

	gp, rem, err := PPMGainToPermille(1_125_000, rec)
	if err != nil || gp != 1125 || rem != 0 {
		t.Fatalf("gain ppm -> permille = %d rem %d err %v", gp, rem, err)
	}
	ppm, err := PermilleGainToPPM(&gp)
	if err != nil || ppm != 1_125_000 {
		t.Fatalf("gain permille -> ppm = %d err %v", ppm, err)
	}

	first := append([]Truncation(nil), rec.Truncations...)
	rec2 := &Recorder{}
	_, _, _ = PermyriadToPermille(1234, rec2)
	_, _, _ = MicroToMilli(8250, rec2)
	_, _, _ = PPMGainToPermille(1_125_000, rec2)
	if len(first) != 2 || !reflect.DeepEqual(first, rec2.Truncations) {
		t.Fatalf("residue sequence not deterministic: %#v vs %#v", first, rec2.Truncations)
	}
}

func TestOverflowCodedError(t *testing.T) {
	_, err := MilliToMicro(math.MaxInt64)
	ce, ok := e.As(err)
	if !ok || ce.Code != e.Ovf {
		t.Fatalf("expected E_OVF, got %v", err)
	}
	if ce.Field != "exposure.stack[].value_ms" || len(ce.Operands) != 2 || ce.Operands[1] != 1000 {
		t.Fatalf("overflow diagnostics wrong: %#v", ce)
	}
}

func TestGainParameterRange(t *testing.T) {
	err := ValidateGainPPM("project.bands[B09].gain_ppm", 10)
	ce, ok := e.As(err)
	if !ok || ce.Code != e.Para || ce.Field == "" || ce.Detail == "" {
		t.Fatalf("expected E_PARA with field/range, got %#v", err)
	}
}
