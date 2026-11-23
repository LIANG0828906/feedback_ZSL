package imaging_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"stitchworkbench/internal/errs"
	"stitchworkbench/internal/imaging"
	"stitchworkbench/internal/model"
	"stitchworkbench/internal/samplegen"
)

func TestLoadValidationCodes(t *testing.T) {
	t.Run("missing manifest", func(t *testing.T) {
		_, err := imaging.LoadSet(t.TempDir())
		assertCode(t, err, errs.EDecode)
	})
	t.Run("corrupt image", func(t *testing.T) {
		dir := makeSet(t)
		if err := os.WriteFile(filepath.Join(dir, "images", "img_01.png"), []byte("not a png"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := imaging.LoadSet(dir)
		assertCode(t, err, errs.EDecode)
	})
	t.Run("dimension field", func(t *testing.T) {
		dir := makeSet(t)
		m := readManifest(t, dir)
		m.Images[1].Width++
		writeManifest(t, dir, m)
		_, err := imaging.LoadSet(dir)
		e := assertCode(t, err, errs.EMeta)
		if e.Field != "images[1].width" {
			t.Fatalf("field=%s", e.Field)
		}
	})
	t.Run("channels field", func(t *testing.T) {
		dir := makeSet(t)
		m := readManifest(t, dir)
		m.Images[2].Channels = 9
		writeManifest(t, dir, m)
		_, err := imaging.LoadSet(dir)
		e := assertCode(t, err, errs.EMeta)
		if e.Field != "images[2].channels" {
			t.Fatalf("field=%s", e.Field)
		}
	})
	t.Run("non-finite homography", func(t *testing.T) {
		dir := makeSet(t)
		m := readManifest(t, dir)
		m.Images[0].HomographyToBase[0] = 0
		m.Images[0].HomographyToBase[4] = 0
		writeManifest(t, dir, m)
		_, err := imaging.LoadSet(dir)
		e := assertCode(t, err, errs.EMeta)
		if e.Field != "images[0].homography_to_base" {
			t.Fatalf("field=%s", e.Field)
		}
	})
	t.Run("canvas", func(t *testing.T) {
		dir := makeSet(t)
		m := readManifest(t, dir)
		m.Canvas.Width = 0
		writeManifest(t, dir, m)
		_, err := imaging.LoadSet(dir)
		e := assertCode(t, err, errs.EMeta)
		if e.Field != "canvas" {
			t.Fatalf("field=%s", e.Field)
		}
	})
}

func makeSet(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := samplegen.Generate(samplegen.Config{OutDir: root, Seed: 42, Sets: 1}); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, "harbor-06")
}

func readManifest(t *testing.T, dir string) model.Manifest {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m model.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func writeManifest(t *testing.T, dir string, m model.Manifest) {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertCode(t *testing.T, err error, code string) *errs.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil", code)
	}
	e := errs.As(err)
	if e.Code != code {
		t.Fatalf("expected %s, got %s (%v)", code, e.Code, err)
	}
	return e
}
