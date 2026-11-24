package sample_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"hdrproof/internal/apperr"
	"hdrproof/internal/model"
	"hdrproof/internal/sample"
)

func TestGenerateIsDeterministic(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	repA, err := sample.Generate(sample.GenerateOptions{Dir: left, Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	repB, err := sample.Generate(sample.GenerateOptions{Dir: right, Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	if repA.TruthFingerprint != repB.TruthFingerprint {
		t.Fatalf("fingerprints differ: %s != %s", repA.TruthFingerprint, repB.TruthFingerprint)
	}
	for _, name := range []string{"manifest.json", "truth.json",
		"routes/arena-linear-bt2020/frames.hdrseq",
		"routes/racing-rgb10-p3/frames.hdrseq"} {
		a, err := os.ReadFile(filepath.Join(left, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(right, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("%s is not deterministic", name)
		}
	}
	var truth model.Truth
	b, _ := os.ReadFile(filepath.Join(left, "truth.json"))
	if err := json.Unmarshal(b, &truth); err != nil {
		t.Fatal(err)
	}
	if !sample.VerifyFingerprints(&truth) {
		t.Fatal("generated truth fingerprints do not verify")
	}
	ds, err := sample.Load(left)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds.Routes) != 4 || len(ds.Routes[0].DecodedFrames) != 24 {
		t.Fatalf("unexpected dataset: routes=%d frames=%d", len(ds.Routes), len(ds.Routes[0].DecodedFrames))
	}
}

func TestCorruptFrameReturnsDecode(t *testing.T) {
	dir := t.TempDir()
	if _, err := sample.Generate(sample.GenerateOptions{Dir: dir, Seed: 7}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "routes", "arena-linear-bt2020", "frames.hdrseq")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b[60] ^= 0xff
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := sample.Load(dir); err == nil || apperr.CodeOf(err) != apperr.CodeDecode {
		t.Fatalf("expected E_DECODE, got %v", err)
	}
}

func TestDeclarationConflictReturnsMeta(t *testing.T) {
	dir := t.TempDir()
	if _, err := sample.Generate(sample.GenerateOptions{Dir: dir, Seed: 7}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "manifest.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest model.Manifest
	if err := json.Unmarshal(b, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Routes[0].Width = 999
	b, _ = json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = sample.Load(dir)
	if err == nil || apperr.CodeOf(err) != apperr.CodeMeta {
		t.Fatalf("expected E_META, got %v", err)
	}
	info, _ := apperr.As(err)
	if info.Field != "width" || info.RouteID != manifest.Routes[0].ID {
		t.Fatalf("unexpected context: %+v", info)
	}
}

func TestMissingTruthRouteReturnsMeta(t *testing.T) {
	dir := t.TempDir()
	if _, err := sample.Generate(sample.GenerateOptions{Dir: dir, Seed: 7}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "truth.json")
	b, _ := os.ReadFile(path)
	var truth model.Truth
	if err := json.Unmarshal(b, &truth); err != nil {
		t.Fatal(err)
	}
	truth.Routes = truth.Routes[1:]
	b, _ = json.MarshalIndent(truth, "", "  ")
	_ = os.WriteFile(path, b, 0o644)
	if _, err := sample.Load(dir); err == nil || apperr.CodeOf(err) != apperr.CodeMeta {
		t.Fatalf("expected E_META, got %v", err)
	}
}
