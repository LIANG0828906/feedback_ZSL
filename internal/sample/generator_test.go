package sample

import (
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"iconseg/internal/core"
)

func TestGenerateIsDeterministic(t *testing.T) {
	first := Generate(20261125)
	second := Generate(20261125)
	if len(first) != len(second) || len(first) != 3 {
		t.Fatalf("unexpected icon count: %d, %d", len(first), len(second))
	}
	for i := range first {
		if !reflect.DeepEqual(first[i].Manifest, second[i].Manifest) {
			t.Fatalf("manifest %d differs", i)
		}
		if !reflect.DeepEqual(first[i].Image.Pix, second[i].Image.Pix) {
			t.Fatalf("image %d pixels differ", i)
		}
		if first[i].Manifest.ReferenceFingerprint == "" {
			t.Fatal("truth fingerprint is empty")
		}
	}
}

func TestWriteLoadAndValidationCodes(t *testing.T) {
	icons := Generate(20261125)
	dir := t.TempDir()
	if err := WriteDataset(dir, icons); err != nil {
		t.Fatal(err)
	}
	for _, icon := range icons {
		loaded, err := LoadDataset(filepath.Join(dir, icon.Manifest.ImageID))
		if err != nil {
			t.Fatalf("load %s: %+v", icon.Manifest.ImageID, err)
		}
		if loaded.Manifest.ReferenceMaskHash != icon.Manifest.ReferenceMaskHash {
			t.Fatalf("%s mask hash changed", icon.Manifest.ImageID)
		}
		if got := loaded.Manifest.ReferenceFingerprint; got != icon.Manifest.ReferenceFingerprint {
			t.Fatalf("%s fingerprint mismatch", icon.Manifest.ImageID)
		}
	}

	icon := icons[0]
	iconDir := filepath.Join(dir, icon.Manifest.ImageID)
	iconPath := filepath.Join(iconDir, icon.Manifest.File)
	if err := writePNGFile(iconPath, image.NewGray(image.Rect(0, 0, icon.Manifest.Width, icon.Manifest.Height))); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDataset(iconDir); err == nil {
		t.Fatal("channel mismatch was accepted")
	} else {
		app := core.AsAppError(err)
		if app.Code != core.CodePara || app.Field != "channels" {
			t.Fatalf("channel mismatch error = %+v", app)
		}
	}

	small := image.NewNRGBA(image.Rect(0, 0, 12, 12))
	for i := range small.Pix {
		small.Pix[i] = 255
	}
	small.SetNRGBA(0, 0, color.NRGBA{R: 0, G: 0, B: 0, A: 200})
	if err := writePNGFile(iconPath, small); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDataset(iconDir); err == nil {
		t.Fatal("dimension mismatch was accepted")
	} else {
		app := core.AsAppError(err)
		if app.Code != core.CodePara || app.Field != "width" {
			t.Fatalf("dimension mismatch error = %+v", app)
		}
	}

	if err := os.WriteFile(iconPath, []byte("not a png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDataset(iconDir); err == nil {
		t.Fatal("corrupt image was accepted")
	} else if core.AsAppError(err).Code != core.CodeDecode {
		t.Fatalf("corrupt image error = %+v", core.AsAppError(err))
	}
}

func TestManifestJSONContainsReferenceMask(t *testing.T) {
	icons := Generate(1234)
	body, err := json.Marshal(icons[0].Manifest)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["reference_mask"] == nil || decoded["reference_mask"] == "" {
		t.Fatal("manifest JSON has no pixel-level reference mask")
	}
}

func writePNGFile(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
