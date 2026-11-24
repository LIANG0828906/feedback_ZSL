package compose

import (
	"bytes"
	"testing"

	"virtualstudio/model"
)

func TestPremultiplyNormalizeRoundTrip(t *testing.T) {
	straight := []byte{
		0, 0, 0, 0,
		255, 255, 255, 255,
		128, 64, 32, 128,
		200, 100, 50, 192,
		10, 240, 90, 64,
	}
	premul := PremultiplyPixels(straight)
	back := NormalizePixels(premul, model.AlphaPremul)
	for i := 0; i+3 < len(straight); i += 4 {
		a := int(straight[i+3])
		for c := 0; c < 3; c++ {
			want := int(straight[i+c])
			if a > 0 {
				p := (want*a + 127) / 255
				want = (p*255 + a/2) / a
			} else {
				want = 0
			}
			if int(back[i+c]) != want {
				t.Fatalf("pixel %d channel %d round-trip=%d want=%d", i/4, c, back[i+c], want)
			}
		}
	}
}

func TestSRGBTransfer(t *testing.T) {
	cases := []struct {
		in   uint8
		want uint8
	}{
		{0, 0}, {255, 255}, {1, 1}, {128, 128},
	}
	for _, tc := range cases {
		got := EncodeSRGB(DecodeSRGB(tc.in))
		if got != tc.want {
			t.Fatalf("srgb %d -> %.12f -> %d, want %d", tc.in, DecodeSRGB(tc.in), got, tc.want)
		}
	}
	if DecodeSRGB(128) <= DecodeSRGB(64) {
		t.Fatal("linearization is not monotonic")
	}
}

func TestPremultiply16IsLosslessForEveryStraightValue(t *testing.T) {
	straight := make([]byte, 256*256*4)
	idx := 0
	for a := 0; a < 256; a++ {
		for c := 0; c < 256; c++ {
			straight[idx] = byte(c)
			straight[idx+1] = byte(255 - c)
			straight[idx+2] = byte((c * 37) % 256)
			straight[idx+3] = byte(a)
			if a == 0 {
				straight[idx], straight[idx+1], straight[idx+2] = 0, 0, 0
			}
			idx += 4
		}
	}
	premul := PremultiplyPixels16(straight)
	back := NormalizePixels16(premul, model.AlphaPremul)
	if !bytes.Equal(straight, back) {
		for i := range straight {
			if straight[i] != back[i] {
				t.Fatalf("byte %d: got %d want %d", i, back[i], straight[i])
			}
		}
	}
}

func TestRenderDeterministicAndOpaque(t *testing.T) {
	scene := testScene()
	commands := testCommands()
	a, err := RenderAll(scene, commands, RenderOptions{Samples: DefaultSamples(scene.Width, scene.Height)})
	if err != nil {
		t.Fatal(err)
	}
	b, err := RenderAll(scene, commands, RenderOptions{Samples: DefaultSamples(scene.Width, scene.Height)})
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Fatal("frame count differs")
	}
	for i := range a {
		if a[i].Hash != b[i].Hash || !bytes.Equal(a[i].RGBA, b[i].RGBA) {
			t.Fatalf("frame %d is not deterministic", i)
		}
		if a[i].Stats.ContentHash != a[i].Hash {
			t.Fatalf("frame %d stats hash mismatch", i)
		}
	}
}

func TestRenderRejectsBadSample(t *testing.T) {
	scene := testScene()
	_, err := RenderFrame(scene, testCommands(), 0, RenderOptions{Samples: []model.SamplePoint{{X: scene.Width, Y: 0}}})
	if !model.IsCode(err, model.CodeParam) {
		t.Fatalf("got %v, want E_PARA", err)
	}
	errObj, _ := model.AsError(err)
	if errObj.Path != "sample_points[0]" {
		t.Fatalf("path=%q", errObj.Path)
	}
}

func TestDiffHeatmapLocatesFirstChannelDifference(t *testing.T) {
	scene := testScene()
	frames, err := RenderAll(scene, testCommands(), RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	a := frames[0].Clone()
	b := frames[0].Clone()
	b.RGBA[4*1+2]++
	hm, err := DiffHeatmap(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if hm.Mismatches != 1 || hm.PixelDiff[1] != 1 || hm.MaxDiff != 1 {
		t.Fatalf("unexpected heatmap: %+v", hm)
	}
}

func testScene() *model.Scene {
	one := []byte{255, 0, 0, 255}
	two := []byte{0, 128, 255, 128}
	return &model.Scene{
		SourceVersion: "test", Width: 2, Height: 2, Channels: 4, FrameCount: 2, FrameDurationNS: 10,
		Layers: []model.Layer{
			{ID: "bg", Name: "bg", Visible: true, Opacity: 1, SourceAlphaMode: model.AlphaStraight,
				Pixels: append(append(append(append([]byte{}, one...), one...), one...), one...)},
			{ID: "fg", Name: "fg", Visible: true, Opacity: 1, SourceAlphaMode: model.AlphaStraight,
				Pixels: append(append(append(append([]byte{}, two...), two...), two...), two...)},
		},
	}
}

func testCommands() *model.CommandSet {
	return &model.CommandSet{Version: "test", WindowFrames: 1, Frames: []model.FrameCommand{
		{Frame: 0, Visible: map[string]bool{"bg": true, "fg": true}},
		{Frame: 1, Visible: map[string]bool{"bg": true, "fg": false}},
	}}
}
