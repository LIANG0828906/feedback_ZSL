package sample

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"iconbench/internal/model"
)

func TestLoaderErrors(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	m, err := Generate(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec := m.Images[0]

	if _, _, err := LoadImage(dir, rec); err != nil {
		t.Fatalf("valid image should load: %v", err)
	}

	badWidth := rec
	badWidth.Width++
	_, _, err = LoadImage(dir, badWidth)
	assertCode(t, err, model.CodeMeta)
	var me *model.Error
	if !errors.As(err, &me) || me.Field != "width" {
		t.Fatalf("expected E_META width, got %#v", err)
	}

	badChannels := rec
	badChannels.Channels = 3
	_, _, err = LoadImage(dir, badChannels)
	assertCode(t, err, model.CodeMeta)
	if !errors.As(err, &me) || me.Field != "channels" {
		t.Fatalf("expected E_META channels, got %#v", err)
	}

	corrupt := rec
	corrupt.ID = rec.ID + "_corrupt"
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rec.File)), []byte("not a png"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err = LoadImage(dir, rec)
	assertCode(t, err, model.CodeDecode)
}

func assertCode(t *testing.T, err error, code string) {
	t.Helper()
	var me *model.Error
	if !errors.As(err, &me) {
		t.Fatalf("expected *model.Error with %s, got %T %v", code, err, err)
	}
	if me.Code != code {
		t.Fatalf("expected %s, got %s (%v)", code, me.Code, me)
	}
}
