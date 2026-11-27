package terrainbake

import (
	"math"
	"testing"
)

func TestCoordinateRoundTripHighLowHigh(t *testing.T) {
	p := testProject(t)
	v3 := encodeTest(t, p, SchemaV3)
	parsedV3 := loadTest(t, v3)

	v1 := encodeTest(t, parsedV3, SchemaV1)
	parsedV1 := loadTest(t, v1)

	v3Again := encodeTest(t, parsedV1, SchemaV3)
	final := loadTest(t, v3Again)

	if final.World.Rect != p.World.Rect {
		t.Fatalf("world roundtrip: got %+v want %+v", final.World.Rect, p.World.Rect)
	}
	for i := range p.Blocks {
		if final.Blocks[i].Rect != p.Blocks[i].Rect {
			t.Fatalf("block %s roundtrip: got %+v want %+v", p.Blocks[i].ID, final.Blocks[i].Rect, p.Blocks[i].Rect)
		}
	}
	for i := range p.Truth.Samples {
		a, b := p.Truth.Samples[i], final.Truth.Samples[i]
		if a.BlockID != b.BlockID || a.X != b.X || a.Y != b.Y || a.HeightMM != b.HeightMM {
			t.Fatalf("truth sample %d roundtrip: got %+v want %+v", i, b, a)
		}
	}
}

func TestIntegerToleranceConversionsAndResiduals(t *testing.T) {
	v, residual, err := ConvertTolerance(3, "permille", "basis_points", "floor")
	if err != nil {
		t.Fatal(err)
	}
	if v != 30 || residual != 0 {
		t.Fatalf("3 permille -> %d bp residual %d", v, residual)
	}
	v, residual, err = UMToTolerance(2750, "permille", "floor")
	if err != nil {
		t.Fatal(err)
	}
	if v != 2 || residual != 750 {
		t.Fatalf("2750um floor permille = %d residual %d", v, residual)
	}
	v, residual, err = UMToTolerance(2750, "permille", "ceil")
	if err != nil {
		t.Fatal(err)
	}
	if v != 3 || residual != -250 {
		t.Fatalf("2750um ceil permille = %d residual %d", v, residual)
	}
	v, _, err = ConvertTolerance(125, "basis_points", "permille", "half_up")
	if err != nil {
		t.Fatal(err)
	}
	if v != 13 { // 12.5 permille rounds half up
		t.Fatalf("125bp half_up permille = %d", v)
	}
}

func TestBoundaryConventionsNormalizeToHalfOpen(t *testing.T) {
	cases := []struct {
		rule       string
		min, max   int64
		wantMin    int64
		wantMax    int64
		truncation string
		residual   int64
	}{
		{"[]", 0, 127, 0, 128, "max+1", 1},
		{"[)", 0, 128, 0, 128, "none", 0},
		{"()", 0, 128, 1, 128, "min+1", 1},
		{"(]", -1, 127, 0, 128, "min+1,max+1", 2},
	}
	for _, tc := range cases {
		iv, err := BoundaryToHalfOpen("test", tc.min, tc.max, tc.rule)
		if err != nil {
			t.Fatalf("%s: %v", tc.rule, err)
		}
		if iv.Min != tc.wantMin || iv.Max != tc.wantMax || iv.Truncation != tc.truncation || iv.ResidualMin+iv.ResidualMax != tc.residual {
			t.Fatalf("%s: got %+v want min=%d max=%d trunc=%s residual=%d", tc.rule, iv, tc.wantMin, tc.wantMax, tc.truncation, tc.residual)
		}
		min, max, err := HalfOpenToBoundary("test", iv.Min, iv.Max, tc.rule)
		if err != nil {
			t.Fatal(err)
		}
		back, err := BoundaryToHalfOpen("test", min, max, tc.rule)
		if err != nil {
			t.Fatal(err)
		}
		if back.Min != iv.Min || back.Max != iv.Max {
			t.Fatalf("%s inverse got %+v want %+v", tc.rule, back, iv)
		}
	}
}

func TestOverflowReportsOperands(t *testing.T) {
	_, err := AddChecked("coord.x", math.MaxInt64, 1)
	e := mustCode(t, err, CodeOvf)
	if e.Field != "coord.x" || e.OperandA == "" || e.OperandB == "" {
		t.Fatalf("E_OVF context missing: %+v", e)
	}
	_, err = RelativeToAbsolute("coord.y", math.MaxInt64, 1)
	e = mustCode(t, err, CodeOvf)
	if e.OperandA != "9223372036854775807" || e.OperandB != "1" {
		t.Fatalf("E_OVF operands = %q,%q", e.OperandA, e.OperandB)
	}
	_, err = BoundaryToHalfOpen("rect.x", 0, math.MaxInt64, "[]")
	mustCode(t, err, CodeOvf)
	_, _, err = HalfOpenToBoundary("rect.x", math.MinInt64, 100, "(]")
	mustCode(t, err, CodeOvf)
}
