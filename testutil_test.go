package textureconv

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func testImage(t *testing.T, w, h int, depth BitDepth, layout Layout) *Image {
	t.Helper()
	limit := depth.Max()
	pix := make([]uint16, w*h*4)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r := uint16((x * int(limit)) / max(1, w-1))
			g := uint16((y * int(limit)) / max(1, h-1))
			b := uint16(((x*17 + y*31) * int(limit)) % (int(limit) + 1))
			a := uint16(((x/7 + y/5) % 5) * int(limit) / 4)
			v := [4]uint16{r, g, b, a}
			off := (y*w + x) * 4
			if layout == LayoutBGRA {
				pix[off], pix[off+1], pix[off+2], pix[off+3] = v[2], v[1], v[0], v[3]
			} else {
				copy(pix[off:off+4], v[:])
			}
		}
	}
	return &Image{Width: w, Height: h, BitDepth: depth, Layout: layout, Pixels: pix}
}

func testSpecs() []ImageSpec {
	return []ImageSpec{
		{ID: "rgba8", Width: 160, Height: 120, BitDepth: Depth8, Layout: LayoutRGBA},
		{ID: "bgra16", Width: 130, Height: 100, BitDepth: Depth16, Layout: LayoutBGRA},
	}
}

func testBundle(t *testing.T, plan *Plan) *SampleBundle {
	t.Helper()
	dir := t.TempDir()
	b, err := GenerateSample(dir, GenerateOptions{Seed: 99173, Images: testSpecs(), Plan: plan})
	if err != nil {
		t.Fatalf("GenerateSample: %v", err)
	}
	return b
}

func processAll(t *testing.T, e *Engine) RunReport {
	t.Helper()
	r, err := e.Process(context.Background())
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	return r
}

func openTestStore(t *testing.T, dir, name string) (*Store, string) {
	t.Helper()
	path := filepath.Join(dir, name)
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	return s, path
}

func mustCode(t *testing.T, err error, code Code) *Error {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil", code)
	}
	if got := CodeOf(err); got != code {
		t.Fatalf("expected code %s, got %s (%v)", code, got, err)
	}
	var e *Error
	if !asError(err, &e) {
		t.Fatalf("error is not *Error: %T", err)
	}
	return e
}

func fileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
