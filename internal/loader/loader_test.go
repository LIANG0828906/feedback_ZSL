package loader_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"integritykernel/internal/core"
	"integritykernel/internal/loader"
	"integritykernel/internal/samplegen"
)

func TestLoadValidSample(t *testing.T) {
	dir := t.TempDir()
	if _, err := samplegen.Generate(samplegen.Options{OutputDir: dir, Frames: 12, Force: true}); err != nil {
		t.Fatal(err)
	}
	seq, err := loader.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(seq.Frames) != 12 || seq.Manifest.Width <= 0 {
		t.Fatalf("unexpected sequence: %+v", seq.Manifest)
	}
}

func TestMissingManifestIsDecodeError(t *testing.T) {
	_, err := loader.Load(t.TempDir())
	requireCode(t, err, core.CodeDecode, "")
}

func TestTruncatedFrameIsDecodeError(t *testing.T) {
	dir := t.TempDir()
	manifest, err := samplegen.Generate(samplegen.Options{OutputDir: dir, Frames: 6, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	framePath := filepath.Join(dir, manifest.Frames[2].File)
	if err := os.Truncate(framePath, 80); err != nil {
		t.Fatal(err)
	}
	_, err = loader.Load(dir)
	requireCode(t, err, core.CodeDecode, "file")
	if ke, ok := core.AsError(err); !ok || ke.Frame == nil || *ke.Frame != 2 {
		t.Fatalf("expected frame 2 decode error, got %#v", err)
	}
}

func TestMetadataErrorsPointToFrameAndField(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
		code   string
		frame  int
		field  string
	}{
		{"width", func(m map[string]any) { m["width"] = m["width"].(float64) + 1 }, core.CodeMeta, 0, "width"},
		{"channels", func(m map[string]any) { m["channels"] = float64(3) }, core.CodeMeta, 0, "channels"},
		{"timestamp", func(m map[string]any) {
			frames := m["frames"].([]any)
			first := frames[0].(map[string]any)
			second := frames[1].(map[string]any)
			second["timestamp_millis"] = first["timestamp_millis"].(float64) - 1
		}, core.CodeMeta, 1, "timestamp_millis"},
		{"frame_count", func(m map[string]any) { m["frame_count"] = float64(99) }, core.CodeMeta, 6, "frame_count"},
		{"watermark_region", func(m map[string]any) {
			region := m["watermark_region"].(map[string]any)
			region["x"] = float64(-10)
		}, core.CodeMark, 0, "watermark_region"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if _, err := samplegen.Generate(samplegen.Options{OutputDir: dir, Frames: 6, Force: true}); err != nil {
				t.Fatal(err)
			}
			manifestPath := filepath.Join(dir, "truth.json")
			raw, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			tc.mutate(m)
			out, err := json.MarshalIndent(m, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manifestPath, out, 0o644); err != nil {
				t.Fatal(err)
			}
			_, err = loader.Load(dir)
			requireCode(t, err, tc.code, tc.field)
			ke, _ := core.AsError(err)
			if ke.Frame == nil || *ke.Frame != tc.frame {
				t.Fatalf("expected frame %d, got %#v", tc.frame, ke)
			}
		})
	}
}

func requireCode(t *testing.T, err error, code, field string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s error", code)
	}
	ke, ok := core.AsError(err)
	if !ok {
		t.Fatalf("expected core.Error, got %T %v", err, err)
	}
	if ke.Code != code {
		t.Fatalf("expected code %s, got %s (%v)", code, ke.Code, ke)
	}
	if field != "" && ke.Field != field {
		t.Fatalf("expected field %s, got %s", field, ke.Field)
	}
}
