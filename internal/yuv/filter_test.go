package yuv

import (
	"context"
	"testing"
)

func TestChromaGainOffsetUsesCodeUnits(t *testing.T) {
	p := &Plane{
		Name: "U", Width: 2, Height: 1, BitDepth: 8, QuantMin: 0, QuantMax: 255,
		Data: []uint16{100, 105},
	}
	got, err := ChromaGain(context.Background(), p, 1.0, 0.01)
	if err != nil {
		t.Fatal(err)
	}
	if got.Data[0] != 103 || got.Data[1] != 108 {
		t.Fatalf("offset must add 0.01*255 rounded to 3 code units, got %v", got.Data)
	}
}
