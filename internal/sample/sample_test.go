package sample_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"maskpreview/internal/core"
	"maskpreview/internal/sample"
)

func TestGeneratorIsDeterministic(t *testing.T) {
	base := t.TempDir()
	dirA := filepath.Join(base, "a")
	dirB := filepath.Join(base, "b")
	cfgA := sample.GenerateConfig{Seed: 991122, OutDir: dirA, Sizes: []int{64, 96, 144}}
	cfgB := cfgA
	cfgB.OutDir = dirB
	ma, err := sample.Generate(cfgA)
	if err != nil {
		t.Fatalf("generate A: %v", err)
	}
	mb, err := sample.Generate(cfgB)
	if err != nil {
		t.Fatalf("generate B: %v", err)
	}
	if len(ma.Samples) != len(mb.Samples) {
		t.Fatal("sample count differs")
	}
	for i := range ma.Samples {
		a, b := ma.Samples[i], mb.Samples[i]
		if a.Width != b.Width || a.Height != b.Height || a.MaskHash != b.MaskHash || a.TruthFingerprint != b.TruthFingerprint {
			t.Fatalf("sample %d metadata differs", i)
		}
		for _, name := range []string{a.ImageFile, a.ReferenceMaskFile} {
			rawA, err := os.ReadFile(filepath.Join(dirA, name))
			if err != nil {
				t.Fatal(err)
			}
			rawB, err := os.ReadFile(filepath.Join(dirB, name))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(rawA, rawB) {
				t.Fatalf("%s is not deterministic", name)
			}
		}
	}
	rawA, _ := os.ReadFile(filepath.Join(dirA, "manifest.json"))
	rawB, _ := os.ReadFile(filepath.Join(dirB, "manifest.json"))
	if !bytes.Equal(rawA, rawB) {
		t.Fatal("manifest is not deterministic")
	}
}

func TestLoaderErrorChains(t *testing.T) {
	dir := t.TempDir()
	manifest, err := sample.Generate(sample.GenerateConfig{Seed: 1, OutDir: dir, Sizes: []int{64}})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	spec := manifest.Samples[0]
	if _, err := sample.LoadSample(dir, spec); err != nil {
		t.Fatalf("baseline load: %v", err)
	}

	corrupt := spec
	corrupt.ImageSHA256 = ""
	corrupt.ImageFile = "corrupt.png"
	if err := os.WriteFile(filepath.Join(dir, corrupt.ImageFile), []byte("not a png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := sample.LoadSample(dir, corrupt); core.CodeOf(err) != core.CodeDecode {
		t.Fatalf("corrupt image error = %v, want E_DECODE", err)
	}

	badWidth := spec
	badWidth.Width++
	_, err = sample.LoadSample(dir, badWidth)
	assertMeta(t, err, "width")

	badChannels := spec
	badChannels.Channels = 3
	_, err = sample.LoadSample(dir, badChannels)
	assertMeta(t, err, "channels")

	badFingerprint := spec
	badFingerprint.TruthFingerprint = "deadbeef"
	_, err = sample.LoadSample(dir, badFingerprint)
	assertMeta(t, err, "truth_fingerprint")
}

func assertMeta(t *testing.T, err error, field string) {
	t.Helper()
	if core.CodeOf(err) != core.CodeMeta {
		t.Fatalf("error = %v, want E_META", err)
	}
	appErr, ok := err.(*core.Error)
	if !ok || appErr.Field != field || appErr.Got == nil || appErr.Want == nil {
		t.Fatalf("meta error missing sides/field: %#v", err)
	}
}
