package yuv

import (
	"context"
	"testing"
)

func TestUpsampleCenterCoordinates(t *testing.T) {
	cases := []struct {
		x    int
		want int
	}{
		{0, -32},
		{1, 32},
		{2, 96},
		{4, 224},
	}
	for _, c := range cases {
		if got := sourceCoordQ8(c.x, AlignCenter); got != c.want {
			t.Fatalf("x=%d got %d want %d", c.x, got, c.want)
		}
	}
}

func TestUpsampleBilinearIsDeterministic(t *testing.T) {
	p := &Plane{
		Name: "U", Width: 2, Height: 2, BitDepth: 8, QuantMin: 0, QuantMax: 255,
		Data: []uint16{0, 32, 96, 128},
	}
	a, err := UpsampleChroma(context.Background(), p, 4, 4, AlignTopLeft, UpsampleBilin)
	if err != nil {
		t.Fatal(err)
	}
	b, err := UpsampleChroma(context.Background(), p, 4, 4, AlignTopLeft, UpsampleBilin)
	if err != nil {
		t.Fatal(err)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("nondeterministic at %d", i)
		}
	}
}

func TestCenterUpsampleClampsLeftEdgeBeforeFraction(t *testing.T) {
	p := &Plane{
		Name: "U", Width: 2, Height: 1, BitDepth: 8, QuantMin: 0, QuantMax: 255,
		Data: []uint16{0, 100},
	}
	got, err := UpsampleChroma(context.Background(), p, 4, 2, AlignCenter, UpsampleBilin)
	if err != nil {
		t.Fatal(err)
	}
	wantRow := []uint16{0, 25, 75, 100}
	for x, want := range wantRow {
		if got[x] != want {
			t.Fatalf("x=%d got %d want %d (row=%v)", x, got[x], want, got[:4])
		}
	}
}
