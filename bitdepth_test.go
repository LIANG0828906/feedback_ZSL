package quantforensics

import "testing"

func TestBitDepthIntegerQuantizationAndTriage(t *testing.T) {
	spec := RGB565Spec()
	bound, err := MaxBitDepthDeviation(spec)
	if err != nil {
		t.Fatal(err)
	}
	if bound[0] == 0 || bound[1] == 0 || bound[2] == 0 {
		t.Fatalf("RGB565 bounds should be positive: %+v", bound)
	}
	c := RGB{R: 201, G: 127, B: 33}
	out, residual, err := QuantizePixelBitDepth(c, spec)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		d := residual[i]
		if d < 0 {
			d = -d
		}
		if d > bound[i] {
			t.Fatalf("channel %d residual %d exceeds bound %d (out=%+v)", i, d, bound[i], out)
		}
	}

	block := Block{ID: "b", X: 0, Y: 0, W: 8, H: 8}
	coord := ClassifyDiscrepancy(block, []int{1}, []int{1}, [3]int{1}, [2]int{2, 0}, nil)
	if coord.Level != LevelCoordinate {
		t.Fatalf("coord level=%s", coord.Level)
	}
	pal := ClassifyDiscrepancy(block, []int{1, 2}, []int{1, 3}, [3]int{9}, [2]int{}, nil)
	if pal.Level != LevelPalette || pal.IndexMismatches != 1 {
		t.Fatalf("palette classification=%+v", pal)
	}
	bit := ClassifyDiscrepancy(block, []int{1}, []int{1}, bound, [2]int{}, &spec)
	if bit.Level != LevelBitDepth {
		t.Fatalf("bit-depth classification=%+v bound=%+v", bit, bound)
	}
}
