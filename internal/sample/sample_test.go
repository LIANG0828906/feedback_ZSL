package sample

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"gameintegrity/internal/model"
)

func TestWriteSampleIsDeterministic(t *testing.T) {
	spec := Spec{
		ID: "tiny", Width: 80, Height: 64, Channels: 3, FrameCount: 16,
		BlockSize: 8, Replacement: model.Interval{Start: 6, End: 8}, SegmentSeconds: 1, Tolerance: 1,
	}
	rootA, rootB := t.TempDir(), t.TempDir()
	if err := WriteSample(rootA, spec, 20261124); err != nil {
		t.Fatal(err)
	}
	if err := WriteSample(rootB, spec, 20261124); err != nil {
		t.Fatal(err)
	}
	paths := []string{
		filepath.Join("tiny", "manifest.json"),
		filepath.Join("tiny", "truth.json"),
		filepath.Join("tiny", "frames", "frame-000006.rgbx"),
		filepath.Join("tiny", "baseline", "frame-000006.rgb"),
	}
	for _, rel := range paths {
		a, _ := os.ReadFile(filepath.Join(rootA, rel))
		b, _ := os.ReadFile(filepath.Join(rootB, rel))
		if !bytes.Equal(a, b) {
			t.Fatalf("%s is not deterministic", rel)
		}
	}
}
