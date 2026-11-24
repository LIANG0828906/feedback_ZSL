package model

import "testing"

func TestPlaneGeometry(t *testing.T) {
	shape := [3]int{64, 56, 40}
	spacing := [3]float64{1, 1.2, 1.5}
	tests := []struct {
		plane        Plane
		normal, u, v int
		w, h         int
	}{
		{PlaneAxial, AxisZ, AxisX, AxisY, 64, 56},
		{PlaneCoronal, AxisY, AxisX, AxisZ, 64, 40},
		{PlaneSagittal, AxisX, AxisY, AxisZ, 56, 40},
	}
	for _, tt := range tests {
		got, ok := PlaneGeometry(tt.plane, shape, spacing)
		if !ok {
			t.Fatalf("%s not recognized", tt.plane)
		}
		if got.Normal != tt.normal || got.U != tt.u || got.V != tt.v || got.Width != tt.w || got.Height != tt.h {
			t.Fatalf("%s geometry mismatch: %+v", tt.plane, got)
		}
	}
}
