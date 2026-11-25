package terrain

import (
	"testing"
)

func TestTriWaveDeterministic(t *testing.T) {
	if got := TriWave(0, 16, 20_000); got != -20_000 {
		t.Fatalf("tri(0)=%d", got)
	}
	if got := TriWave(2, 16, 20_000); got != -10_000 {
		t.Fatalf("tri(eighth)=%d", got)
	}
	if got := TriWave(4, 16, 20_000); got != 0 {
		t.Fatalf("tri(quarter)=%d", got)
	}
	if got := TriWave(8, 16, 20_000); got != 20_000 {
		t.Fatalf("tri(half)=%d", got)
	}
	if got := TriWave(12, 16, 20_000); got != 0 {
		t.Fatalf("tri(three quarters)=%d", got)
	}
}

func TestHeightAndSlopeAreDeterministic(t *testing.T) {
	tests := []struct {
		x, y      int
		height    int64
		slopeURad int64
	}{
		{0, 0, 42147, 1510005},
		{3, 2, 50600, 1534348},
		{7, 7, 45827, 1510005},
		{4, 5, 54640, 1534779},
		{15, 15, 33573, 1527876},
	}
	for _, tc := range tests {
		if got := HeightMM(20261125, tc.x, tc.y); got != tc.height {
			t.Fatalf("height(%d,%d)=%d, want %d", tc.x, tc.y, got, tc.height)
		}
		if got := SlopeURad(20261125, tc.x, tc.y); got != tc.slopeURad {
			t.Fatalf("slope(%d,%d)=%d, want %d", tc.x, tc.y, got, tc.slopeURad)
		}
	}
}

func TestAtanURadApproximatesAtan(t *testing.T) {
	if got := AtanURad(0); got != 0 {
		t.Fatalf("atan(0)=%d", got)
	}
	previous := int64(-1)
	for ratio := int64(1_000_000); ratio <= 40_000_000; ratio += 1_000_000 {
		got := AtanURad(ratio)
		if got < previous || got > 1_570_796 {
			t.Fatalf("atan not monotonic at %d: %d", ratio, got)
		}
		previous = got
	}
}

func TestNoiseOffsetBoundedAndDeterministic(t *testing.T) {
	for _, pt := range [][2]int{{0, 0}, {3, 2}, {7, 7}, {4, 5}, {31, 19}} {
		a := NoiseOffset(20261136, 20_000, 4_000, pt[0], pt[1])
		b := NoiseOffset(20261136, 20_000, 4_000, pt[0], pt[1])
		if a != b {
			t.Fatalf("noise not deterministic at %v", pt)
		}
		if a < -20_000 || a > 20_000 {
			t.Fatalf("noise %d outside amplitude at %v", a, pt)
		}
	}
	if NoiseOffset(1, 20_000, 0, 0, 0) != 0 {
		t.Fatal("zero scale must disable noise")
	}
}
