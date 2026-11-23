package sample

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"maskpipeline/internal/apperr"
)

func TestGenerateDeterministicAndLoad(t *testing.T) {
	root := t.TempDir()
	opts := GenerateOptions{
		Seed:   73,
		OutDir: filepath.Join(root, "a"),
		Resolutions: []Resolution{
			{Width: 320, Height: 192, TileSize: 64, DeadlineMS: 500},
		},
	}
	first, err := Generate(opts)
	if err != nil {
		t.Fatal(err)
	}
	opts.OutDir = filepath.Join(root, "b")
	second, err := Generate(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("unexpected generated count: %d %d", len(first), len(second))
	}
	a := filepath.Join(root, "a", first[0].SampleID)
	b := filepath.Join(root, "b", second[0].SampleID)
	assertSameTree(t, a, b)

	s, err := Load(a)
	if err != nil {
		t.Fatal(err)
	}
	if s.TruthStats.RegionCount != 3 || s.Manifest.Truth.RegionCount != 3 {
		t.Fatalf("region count = %d / %d", s.TruthStats.RegionCount, s.Manifest.Truth.RegionCount)
	}
	if s.TruthFingerprint == "" || s.TruthFingerprint != s.Manifest.Truth.Fingerprint {
		t.Fatalf("bad truth fingerprint %q", s.TruthFingerprint)
	}
	for _, r := range s.Manifest.Regions {
		if r.Area <= 0 || r.BBox.Empty() {
			t.Fatalf("invalid truth region: %+v", r)
		}
	}
}

func TestLoadFailureCodes(t *testing.T) {
	newSample := func(t *testing.T) string {
		t.Helper()
		root := t.TempDir()
		got, err := Generate(GenerateOptions{
			Seed:   11,
			OutDir: root,
			Resolutions: []Resolution{
				{Width: 192, Height: 128, TileSize: 32, DeadlineMS: 500},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return filepath.Join(root, got[0].SampleID)
	}

	t.Run("missing_manifest", func(t *testing.T) {
		_, err := Load(t.TempDir())
		assertCode(t, err, apperr.CodeDecode, "manifest")
	})

	t.Run("corrupt_image", func(t *testing.T) {
		dir := newSample(t)
		path := filepath.Join(dir, ImageName)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		raw[len(raw)-1] ^= 0x7f
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err = Load(dir)
		assertCode(t, err, apperr.CodeDecode, "image.sha256")
	})

	t.Run("width_metadata_mismatch", func(t *testing.T) {
		dir := newSample(t)
		modifyManifest(t, dir, func(m *Manifest) { m.Width++ })
		_, err := Load(dir)
		assertCode(t, err, apperr.CodeMeta, "width")
	})

	t.Run("channel_metadata_mismatch", func(t *testing.T) {
		dir := newSample(t)
		path := filepath.Join(dir, ImageName)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		raw[16] = 4
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err = Load(dir)
		assertCode(t, err, apperr.CodeMeta, "channels")
	})

	t.Run("missing_truth_mask", func(t *testing.T) {
		dir := newSample(t)
		m, err := LoadManifest(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(dir, filepath.FromSlash(m.Regions[0].MaskPath))); err != nil {
			t.Fatal(err)
		}
		_, err = Load(dir)
		assertCode(t, err, apperr.CodeDecode, "regions[0].mask")
	})
}

func modifyManifest(t *testing.T, dir string, fn func(*Manifest)) {
	t.Helper()
	path := filepath.Join(dir, ManifestName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	fn(&m)
	data, err = json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertSameTree(t *testing.T, a, b string) {
	t.Helper()
	filesA := treeHashes(t, a)
	filesB := treeHashes(t, b)
	if len(filesA) != len(filesB) {
		t.Fatalf("tree sizes differ: %d vs %d", len(filesA), len(filesB))
	}
	for name, hash := range filesA {
		if filesB[name] != hash {
			t.Fatalf("file differs: %s", name)
		}
	}
}

func treeHashes(t *testing.T, root string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = hashBytes(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return string(sum[:])
}

func assertCode(t *testing.T, err error, code, field string) {
	t.Helper()
	if err == nil {
		t.Fatalf("wanted %s, got nil", code)
	}
	if got := apperr.Code(err); got != code {
		t.Fatalf("code = %s, want %s: %v", got, code, err)
	}
	if field != "" && apperr.Field(err) != field {
		t.Fatalf("field = %q, want %q: %v", apperr.Field(err), field, err)
	}
}
