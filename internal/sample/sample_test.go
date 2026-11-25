package sample

import (
	"os"
	"path/filepath"
	"testing"

	"edgerefine/internal/errs"
	"edgerefine/internal/model"
)

func TestGenerateDeterministicAndLoad(t *testing.T) {
	root := t.TempDir()
	ms, err := GenerateAll(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 3 {
		t.Fatalf("expected 3 samples, got %d", len(ms))
	}
	for _, m := range ms {
		b, err := LoadBundle(filepath.Join(root, m.SampleID))
		if err != nil {
			t.Fatalf("load %s: %v", m.SampleID, err)
		}
		if len(b.Image.Pix) != m.Width*m.Height {
			t.Fatalf("bad pixel length for %s", m.SampleID)
		}
		if len(m.Edges) == 0 || m.TruthFingerprint == "" {
			t.Fatalf("truth not populated")
		}
	}

	root2 := t.TempDir()
	ms2, err := GenerateAll(root2)
	if err != nil {
		t.Fatal(err)
	}
	for i := range ms {
		a, err := os.ReadFile(filepath.Join(root, ms[i].SampleID, "image.png"))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(root2, ms2[i].SampleID, "image.png"))
		if err != nil {
			t.Fatal(err)
		}
		if string(a) != string(b) || ms[i].TruthFingerprint != ms2[i].TruthFingerprint {
			t.Fatalf("sample %s is not deterministic", ms[i].SampleID)
		}
	}
}

func TestLoadCorruptImageReturnsEDecode(t *testing.T) {
	root := t.TempDir()
	m, err := Generate(root, DefaultSpecs()[0])
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, m.SampleID)
	if err := os.WriteFile(filepath.Join(dir, m.ImageFile), []byte("not a png"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = LoadBundle(dir)
	ee, ok := errs.As(err)
	if !ok || ee.Code != errs.EDecode {
		t.Fatalf("expected E_DECODE, got %v", err)
	}
}

func TestParameterValidationPointsAtField(t *testing.T) {
	p := model.DefaultParams()
	p.TileSize = 30
	err := model.ValidateParams(p, 128, 96)
	ee, ok := errs.As(err)
	if !ok || ee.Code != errs.EPara || ee.Violation.Field != "width" {
		t.Fatalf("expected E_PARA width, got %#v", err)
	}

	p = model.DefaultParams()
	p.IterBudget = 0
	err = model.ValidateParams(p, 128, 96)
	ee, ok = errs.As(err)
	if !ok || ee.Violation.Field != "iter_budget" {
		t.Fatalf("expected iter_budget field, got %v", err)
	}
}
