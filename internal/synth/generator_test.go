package synth_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"ocrworkbench/internal/domain"
	"ocrworkbench/internal/synth"
)

func TestGenerationIsByteDeterministic(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	manifestA, appErr := synth.Generate(dirA, domain.DefaultBaseSeed)
	if appErr != nil {
		t.Fatalf("generate A: %v", appErr)
	}
	manifestB, appErr := synth.Generate(dirB, domain.DefaultBaseSeed)
	if appErr != nil {
		t.Fatalf("generate B: %v", appErr)
	}
	if len(manifestA.Samples) != 5 || len(manifestB.Samples) != len(manifestA.Samples) {
		t.Fatalf("unexpected sample counts: %d / %d", len(manifestA.Samples), len(manifestB.Samples))
	}
	if manifestA.Seed != manifestB.Seed || manifestA.FontVersion != manifestB.FontVersion {
		t.Fatalf("manifest identity differs")
	}
	for i, sample := range manifestA.Samples {
		if sample.ImageHash != manifestB.Samples[i].ImageHash {
			t.Fatalf("%s image hashes differ", sample.ID)
		}
		for _, name := range []string{sample.Image, sample.Truth} {
			a, err := os.ReadFile(filepath.Join(dirA, name))
			if err != nil {
				t.Fatalf("read A %s: %v", name, err)
			}
			b, err := os.ReadFile(filepath.Join(dirB, name))
			if err != nil {
				t.Fatalf("read B %s: %v", name, err)
			}
			if !bytes.Equal(a, b) {
				t.Fatalf("%s is not byte deterministic", name)
			}
		}
	}
}

func TestTruthComesFromConstructionRecords(t *testing.T) {
	dir := t.TempDir()
	manifest, appErr := synth.Generate(dir, domain.DefaultBaseSeed)
	if appErr != nil {
		t.Fatalf("generate: %v", appErr)
	}
	for _, meta := range manifest.Samples {
		if meta.RegionCount != meta.LineCount || meta.RegionCount == 0 {
			t.Fatalf("%s region/line count invalid", meta.ID)
		}
		if meta.ImageHash == "" || meta.Seed == 0 {
			t.Fatalf("%s missing construction metadata", meta.ID)
		}
	}
}
