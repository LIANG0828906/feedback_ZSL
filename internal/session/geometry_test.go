package session

import (
	"testing"

	"mirrorcast/internal/bizerror"
)

func TestMapRegionExactCanonical420(t *testing.T) {
	mapping, err := MapRegion(Rect{X: 16, Y: 9, Width: 1920, Height: 1080}, 320, 200, 10000, "PLANAR_420")
	if err != nil {
		t.Fatalf("MapRegion returned error: %v", err)
	}
	wantRect := Rect{X: 0, Y: 10, Width: 320, Height: 180}
	if mapping.Destination != wantRect {
		t.Fatalf("destination = %+v, want %+v", mapping.Destination, wantRect)
	}
	if mapping.ScaledWidth != 320 || mapping.ScaledHeight != 180 {
		t.Fatalf("scaled size = %dx%d", mapping.ScaledWidth, mapping.ScaledHeight)
	}
	if mapping.PlaneWidth != 160 || mapping.PlaneHeight != 90 {
		t.Fatalf("plane size = %dx%d, want 160x90", mapping.PlaneWidth, mapping.PlaneHeight)
	}
	if mapping.NominalBitsPerPixel != 12 || mapping.ChromaBitsPerPixel != 4 {
		t.Fatalf("bits = nominal %d chroma %d", mapping.NominalBitsPerPixel, mapping.ChromaBitsPerPixel)
	}
	if mapping.BudgetCostKbps != 41472 {
		t.Fatalf("budget = %d, want 41472", mapping.BudgetCostKbps)
	}
}

func TestMapRegionExact422QuarterScaleOffset(t *testing.T) {
	mapping, err := MapRegion(Rect{Width: 1920, Height: 1080}, 320, 200, 7500, "CHROMA_422")
	if err != nil {
		t.Fatal(err)
	}
	wantRect := Rect{X: 40, Y: 32, Width: 240, Height: 135}
	if mapping.Destination != wantRect {
		t.Fatalf("destination = %+v, want %+v", mapping.Destination, wantRect)
	}
	if mapping.PlaneWidth != 120 || mapping.PlaneHeight != 135 {
		t.Fatalf("422 plane = %dx%d, want 120x135", mapping.PlaneWidth, mapping.PlaneHeight)
	}
	if mapping.BudgetCostKbps != 31104 {
		t.Fatalf("budget = %d, want 31104", mapping.BudgetCostKbps)
	}
}

func TestMapRegionDistinguishesBoundaryErrors(t *testing.T) {
	master := Rect{Width: 1, Height: 1}
	tests := []struct {
		name   string
		w, h   int64
		scale  int64
		chroma string
		code   string
	}{
		{"zero scale", 100, 100, 0, "420", bizerror.EScaleZero},
		{"scale out of range", 100, 100, 10001, "420", bizerror.EScaleOutOfRange},
		{"negative width", -1, 100, 10000, "420", bizerror.EResolutionNegative},
		{"invalid chroma", 100, 100, 10000, "411", bizerror.EChromaEnumInvalid},
		{"below 420 unit", 100, 100, 100, "420", bizerror.EResolutionTooSmall},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := MapRegion(master, tt.w, tt.h, tt.scale, tt.chroma)
			if bizerror.CodeOf(err) != tt.code {
				t.Fatalf("code = %q, want %q (err=%v)", bizerror.CodeOf(err), tt.code, err)
			}
		})
	}
}

func TestExactChromaSamplingUnitCases(t *testing.T) {
	exact420, err := MapRegion(Rect{Width: 1, Height: 1}, 100, 100, 200, "420")
	if err != nil {
		t.Fatal(err)
	}
	if exact420.Destination.Width != 2 || exact420.Destination.Height != 2 || exact420.PlaneWidth != 1 || exact420.PlaneHeight != 1 {
		t.Fatalf("420 exact unit mismatch: %+v planes %dx%d", exact420.Destination, exact420.PlaneWidth, exact420.PlaneHeight)
	}
	exact422, err := MapRegion(Rect{Width: 2, Height: 1}, 100, 50, 200, "422")
	if err != nil {
		t.Fatal(err)
	}
	if exact422.Destination.Width != 2 || exact422.Destination.Height != 1 || exact422.PlaneWidth != 1 {
		t.Fatalf("422 exact unit mismatch: %+v plane %dx%d", exact422.Destination, exact422.PlaneWidth, exact422.PlaneHeight)
	}
}

func TestNextLowerRungIsAdjacent(t *testing.T) {
	next, ok := NextLowerRung(6250)
	if !ok || next != 5000 {
		t.Fatalf("NextLowerRung(6250)=%d,%v want 5000,true", next, ok)
	}
	next, ok = NextLowerRung(3750)
	if !ok || next != 2500 {
		t.Fatalf("NextLowerRung(3750)=%d,%v want 2500,true", next, ok)
	}
}
