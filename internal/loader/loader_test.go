package loader_test

import (
	"errors"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"testing"

	"ocrworkbench/internal/core"
	"ocrworkbench/internal/loader"
	"ocrworkbench/internal/sample"
)

func TestRGBPNGChannelCount(t *testing.T) {
	dir := t.TempDir()
	img := image.NewRGBA(image.Rect(0, 0, 24, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 24; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 10, G: 20, B: 30, A: 255})
		}
	}
	path := filepath.Join(dir, "rgb.png")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(file, img); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	truth := core.ImageTruth{
		ID:                  "rgb",
		File:                "rgb.png",
		Width:               24,
		Height:              16,
		Channels:            3,
		DeclaredRegionCount: 0,
		SHA256:              core.HashBytes(data),
	}
	if _, err := loader.Load(dir, truth); err != nil {
		t.Fatalf("RGB PNG should report three channels: %v", err)
	}
}

func TestDecodeAndMetadataErrors(t *testing.T) {
	dir := t.TempDir()
	manifest, err := sample.Generate(sample.Options{OutputDir: dir, Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	source := manifest.Images[0]
	if _, err := loader.Load(dir, source); err != nil {
		t.Fatalf("baseline load: %v", err)
	}
	t.Run("decode", func(t *testing.T) {
		truth := cloneTruth(source)
		truth.File = "broken.png"
		if err := os.WriteFile(filepath.Join(dir, truth.File), []byte("not a png"), 0o644); err != nil {
			t.Fatal(err)
		}
		truth.SHA256 = ""
		_, err := loader.Load(dir, truth)
		assertCode(t, err, core.CodeDecode, "")
	})
	t.Run("width", func(t *testing.T) {
		truth := cloneTruth(source)
		truth.Width++
		_, err := loader.Load(dir, truth)
		assertCode(t, err, core.CodeMeta, "width")
	})
	t.Run("channels", func(t *testing.T) {
		truth := cloneTruth(source)
		truth.Channels = 3
		_, err := loader.Load(dir, truth)
		assertCode(t, err, core.CodeMeta, "channels")
	})
	t.Run("region_count", func(t *testing.T) {
		truth := cloneTruth(source)
		truth.DeclaredRegionCount++
		_, err := loader.Load(dir, truth)
		assertCode(t, err, core.CodeMeta, "declared_region_count")
	})
	t.Run("char_count", func(t *testing.T) {
		truth := cloneTruth(source)
		truth.Regions[0].CharCount++
		_, err := loader.Load(dir, truth)
		assertCode(t, err, core.CodeMeta, "regions[0].char_count")
	})
	t.Run("degenerate_box", func(t *testing.T) {
		truth := cloneTruth(source)
		truth.Regions[0].Box[1] = truth.Regions[0].Box[0]
		_, err := loader.Load(dir, truth)
		assertCode(t, err, core.CodeMeta, "regions[0].box")
	})
	t.Run("non_finite_box", func(t *testing.T) {
		truth := cloneTruth(source)
		truth.Regions[0].Box[0].X = math.NaN()
		_, err := loader.Load(dir, truth)
		assertCode(t, err, core.CodeMeta, "regions[0].box")
	})
}

func cloneTruth(source core.ImageTruth) core.ImageTruth {
	copyTruth := source
	copyTruth.Regions = append([]core.TruthRegion(nil), source.Regions...)
	return copyTruth
}

func assertCode(t *testing.T, err error, code, field string) {
	t.Helper()
	var typed *core.Error
	if !errors.As(err, &typed) {
		t.Fatalf("expected *core.Error, got %v", err)
	}
	if typed.Code != code || typed.Field != field {
		t.Fatalf("got code=%s field=%q, want %s %q", typed.Code, typed.Field, code, field)
	}
}
