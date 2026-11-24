package convert

import (
	"testing"

	"texlab/internal/apperr"
	"texlab/internal/model"
)

func TestChannelSwapAndUpQuantize(t *testing.T) {
	in := model.NewImage(2, 1, 8, 4, "RGBA", "straight")
	in.SetSemanticPixel(0, 0, [4]uint16{10, 20, 30, 255})
	in.SetSemanticPixel(1, 0, [4]uint16{1, 2, 3, 4})
	e := model.Entry{
		Index: 1, ID: "swap", TargetDepth: 16, TargetChannels: 4,
		Permutation: []int{2, 1, 0, 3}, Alpha: "straight", Quantize: "round",
	}
	got, err := Transform(in, e, []model.Rect{{ID: "full", X: 0, Y: 0, W: 2, H: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Image.Depth != 16 || got.Image.Layout != "BGRA" {
		t.Fatalf("output = depth %d layout %s", got.Image.Depth, got.Image.Layout)
	}
	if p := got.Image.SemanticPixel(0, 0); p != [4]uint16{30 * 257, 20 * 257, 10 * 257, 255 * 257} {
		t.Fatalf("pixel = %v", p)
	}
}

func TestQuantizeDirections(t *testing.T) {
	cases := []struct {
		mode string
		want uint16
	}{
		{"floor", 116}, {"ceil", 117}, {"round", 117},
	}
	for _, tc := range cases {
		if got := Quantize(30000, 16, 8, tc.mode); got != tc.want {
			t.Errorf("%s = %d, want %d", tc.mode, got, tc.want)
		}
	}
	if got := Quantize(127, 8, 16, "floor"); got != 127*257 {
		t.Errorf("8->16 = %d", got)
	}
}

func TestAlphaRoundTripWithinStep(t *testing.T) {
	for _, depth := range []int{8, 16} {
		limit := 255
		if depth == 16 {
			limit = 65535
		}
		for _, a := range []int{0, 1, 17, 64, 128, 255, 1024} {
			if a > limit {
				continue
			}
			for _, c := range []int{0, 1, 17, limit / 2, limit - 1, limit} {
				p := Premultiply(uint16(c), uint16(a), depth, "round")
				u := Unpremultiply(p, uint16(a), depth, "round")
				delta := int(u) - c
				if delta < 0 {
					delta = -delta
				}
				if delta > AlphaStepBound(uint16(a), depth) {
					t.Fatalf("depth %d a %d c %d: roundtrip %d delta %d > %d", depth, a, c, u, delta, AlphaStepBound(uint16(a), depth))
				}
			}
		}
	}
}

func TestAddDropChannels(t *testing.T) {
	in := model.NewImage(1, 1, 8, 4, "RGBA", "straight")
	in.SetSemanticPixel(0, 0, [4]uint16{9, 8, 7, 6})
	drop := model.Entry{
		Index: 1, ID: "drop", TargetDepth: 8, TargetChannels: 3,
		Permutation: []int{2, 1, 0}, Drop: []int{3}, Alpha: "straight", Quantize: "round",
	}
	got, err := Transform(in, drop, []model.Rect{{X: 0, Y: 0, W: 1, H: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if p := got.Image.SemanticPixel(0, 0); p != [4]uint16{7, 8, 9, 0} {
		t.Fatalf("drop pixel = %v", p)
	}
	add := model.Entry{
		Index: 2, ID: "add", TargetDepth: 8, TargetChannels: 4,
		Permutation: []int{0, 1, 2, -1}, Fill: []model.FillSpec{{TargetIndex: 3, Source: "constant", Channel: "A", Value: 255}},
		Alpha: "straight", Quantize: "round",
	}
	got2, err := Transform(got.Image, add, []model.Rect{{X: 0, Y: 0, W: 1, H: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if p := got2.Image.SemanticPixel(0, 0); p != [4]uint16{7, 8, 9, 255} {
		t.Fatalf("add pixel = %v", p)
	}
}

func TestParameterError(t *testing.T) {
	in := model.NewImage(1, 1, 8, 4, "RGBA", "straight")
	e := model.Entry{Index: 7, ID: "bad", TargetDepth: 12, TargetChannels: 4, Permutation: []int{0, 1, 2, 3}, Alpha: "straight", Quantize: "round"}
	_, err := Transform(in, e, []model.Rect{{X: 0, Y: 0, W: 1, H: 1}})
	if !apperr.IsCode(err, apperr.EPara) {
		t.Fatalf("want E_PARA, got %v", err)
	}
	ae := apperr.As(err)
	if ae.Field != "target_depth" || ae.EntryIndex != 7 {
		t.Fatalf("error context = %+v", ae)
	}
}

func TestLimitedScratchBandingIsPixelIdentical(t *testing.T) {
	in := model.NewImage(17, 13, 8, 4, "RGBA", "straight")
	for y := 0; y < 13; y++ {
		for x := 0; x < 17; x++ {
			in.SetSemanticPixel(x, y, [4]uint16{uint16(x * 7 % 256), uint16(y * 11 % 256), uint16((x + y) * 3 % 256), uint16((x * y) % 256)})
		}
	}
	e := model.Entry{
		Index: 1, ID: "band", TargetDepth: 16, TargetChannels: 4,
		Permutation: []int{2, 1, 0, 3}, Alpha: "premultiplied", Quantize: "ceil",
	}
	coverage := []model.Rect{{ID: "full", X: 0, Y: 0, W: 17, H: 13}}
	want, err := Transform(in, e, coverage)
	if err != nil {
		t.Fatal(err)
	}
	got, err := TransformLimited(in, e, coverage, 19)
	if err != nil {
		t.Fatal(err)
	}
	if d := model.FirstImageDiff(want.Image, got.Image, e, 0); d != nil {
		t.Fatalf("banded output differs at %+v", d)
	}
}
