package yuv

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"yuvench/errs"
)

func TestGeneratedSamplesAreDeterministicAndTruthful(t *testing.T) {
	dir := t.TempDir()
	manifestPath, err := GenerateSamples(dir, 20261124)
	if err != nil {
		t.Fatal(err)
	}
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _, err := LoadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Images) < 3 {
		t.Fatalf("expected multiple samples, got %d", len(manifest.Images))
	}
	seenDepth := map[int]bool{}
	seenSize := map[[2]int]bool{}
	for _, meta := range manifest.Images {
		image, err := LoadImage(manifestPath, meta.Name)
		if err != nil {
			t.Fatalf("load %s: %v", meta.Name, err)
		}
		if err := VerifyTruth(image); err != nil {
			t.Fatalf("truth %s: %v", meta.Name, err)
		}
		if image.Y.Width != meta.Width || image.Y.Height != meta.Height {
			t.Fatalf("luma dimensions changed")
		}
		if image.U.Width != meta.Width/2 || image.U.Height != meta.Height/2 {
			t.Fatalf("chroma dimensions are not half resolution")
		}
		if image.V.Width != meta.Width/2 || image.V.Height != meta.Height/2 {
			t.Fatalf("chroma dimensions are not half resolution")
		}
		seenDepth[meta.BitDepth] = true
		seenSize[[2]int{meta.Width, meta.Height}] = true
	}
	if len(seenDepth) < 3 || len(seenSize) < 3 {
		t.Fatalf("samples do not vary resolution and depth: depths=%v sizes=%v", seenDepth, seenSize)
	}

	secondDir := t.TempDir()
	secondManifest, err := GenerateSamples(secondDir, 20261124)
	if err != nil {
		t.Fatal(err)
	}
	secondData, err := os.ReadFile(secondManifest)
	if err != nil {
		t.Fatal(err)
	}
	if string(manifestData) != string(secondData) {
		t.Fatal("manifest is not deterministic")
	}
	for _, meta := range manifest.Images {
		for _, spec := range meta.Planes {
			first, err := os.ReadFile(filepath.Join(dir, spec.File))
			if err != nil {
				t.Fatal(err)
			}
			second, err := os.ReadFile(filepath.Join(secondDir, spec.File))
			if err != nil {
				t.Fatal(err)
			}
			if string(first) != string(second) {
				t.Fatalf("plane %s is not deterministic", spec.File)
			}
		}
	}
}

func TestPlaneValidationErrors(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*Manifest, string)
		field    string
		plane    string
		truncate bool
	}{
		{
			name: "odd dimensions",
			mutate: func(m *Manifest, _ string) {
				m.Images[0].Width = 65
			},
			field: "width,height",
			plane: "Y",
		},
		{
			name: "chroma size mismatch",
			mutate: func(m *Manifest, _ string) {
				meta := &m.Images[0]
				spec := meta.Planes["U"]
				spec.Width = 31
				meta.Planes["U"] = spec
			},
			field: "planes.U",
			plane: "U",
		},
		{
			name: "bit depth mismatch",
			mutate: func(m *Manifest, _ string) {
				meta := &m.Images[0]
				spec := meta.Planes["V"]
				spec.BitDepth = 10
				meta.Planes["V"] = spec
			},
			field: "planes.V.bitDepth",
			plane: "V",
		},
		{
			name: "quantization mismatch",
			mutate: func(m *Manifest, _ string) {
				m.Images[0].Quantization.Max = 254
			},
			field: "quantization.max",
		},
		{
			name: "truncated plane",
			mutate: func(m *Manifest, _ string) {
				_ = m
			},
			field:    "planes.U",
			plane:    "U",
			truncate: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			manifestPath, err := GenerateSamples(dir, 20261124)
			if err != nil {
				t.Fatal(err)
			}
			manifest, _, err := LoadManifest(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(manifest, dir)
			if test.truncate {
				path := filepath.Join(dir, manifest.Images[0].Planes["U"].File)
				if err := os.Truncate(path, 10); err != nil {
					t.Fatal(err)
				}
			}
			data, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			badPath := filepath.Join(dir, "bad-manifest.json")
			if err := os.WriteFile(badPath, data, 0o644); err != nil {
				t.Fatal(err)
			}
			_, err = LoadImage(badPath, manifest.Images[0].Name)
			if err == nil {
				t.Fatal("expected validation error")
			}
			business, ok := errs.As(err)
			if !ok || business.Code != errs.CodePlane {
				t.Fatalf("expected E_PLANE, got %v", err)
			}
			if business.Field != test.field {
				t.Fatalf("field=%q, want %q", business.Field, test.field)
			}
			if test.plane != "" && business.Plane != test.plane {
				t.Fatalf("plane=%q, want %q", business.Plane, test.plane)
			}
		})
	}
}

func TestUpsamplePreservesDimensions(t *testing.T) {
	dir := t.TempDir()
	manifestPath, err := GenerateSamples(dir, 20261124)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _, err := LoadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, meta := range manifest.Images {
		image, err := LoadImage(manifestPath, meta.Name)
		if err != nil {
			t.Fatal(err)
		}
		composite, err := Upsample420(image.Meta, image.Y, image.U, image.V)
		if err != nil {
			t.Fatal(err)
		}
		if composite.Width != meta.Width || composite.Height != meta.Height {
			t.Fatalf("%s output dimensions changed", meta.Name)
		}
		if len(composite.Y) != meta.Width*meta.Height || len(composite.U) != meta.Width*meta.Height || len(composite.V) != meta.Width*meta.Height {
			t.Fatalf("%s output plane dimensions changed", meta.Name)
		}
	}
}
