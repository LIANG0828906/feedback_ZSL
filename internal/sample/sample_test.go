package sample_test

import (
	"os"
	"path/filepath"
	"testing"

	"maskpipe/internal/model"
	"maskpipe/internal/sample"
)

func TestGenerateLoadAndDecodeError(t *testing.T) {
	root := t.TempDir()
	manifest, err := sample.Generate(root, 424242)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Samples) != 3 {
		t.Fatalf("expected 3 scenes, got %d", len(manifest.Samples))
	}
	loaded, err := sample.LoadScene(root, "blocks")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Scene.ChunkCount != 6 || loaded.Scene.TruthRegionCount != 3 {
		t.Fatalf("unexpected blocks manifest: chunks=%d regions=%d", loaded.Scene.ChunkCount, loaded.Scene.TruthRegionCount)
	}

	corruptRoot := t.TempDir()
	if _, err := sample.Generate(corruptRoot, 424242); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corruptRoot, "blocks.png"), []byte("not a png"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = sample.LoadScene(corruptRoot, "blocks")
	pe, ok := model.AsError(err)
	if !ok || pe.Code != model.CodeDecode {
		t.Fatalf("expected E_DECODE, got %v", err)
	}
}

func TestLoadMetaMismatchNamesField(t *testing.T) {
	root := t.TempDir()
	if _, err := sample.Generate(root, 7); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	bad := string(data)
	bad = replaceFirst(t, bad, `"width": 192`, `"width": 256`)
	bad = replaceFirst(t, bad, `"cols": 3`, `"cols": 4`)
	bad = replaceFirst(t, bad, `"chunk_count": 6`, `"chunk_count": 8`)
	if err := os.WriteFile(manifestPath, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = sample.LoadScene(root, "blocks")
	pe, ok := model.AsError(err)
	if !ok || pe.Code != model.CodeMeta || pe.Field != "width" {
		t.Fatalf("expected E_META field=width, got %v", err)
	}
}

func replaceFirst(t *testing.T, s, old, new string) string {
	t.Helper()
	for i := 0; i+len(old) <= len(s); i++ {
		if s[i:i+len(old)] == old {
			return s[:i] + new + s[i+len(old):]
		}
	}
	t.Fatalf("pattern %q not found", old)
	return ""
}
