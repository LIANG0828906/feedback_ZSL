package imaging_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"ocrworkbench/internal/domain"
	"ocrworkbench/internal/imaging"
	"ocrworkbench/internal/synth"
)

func TestLoadAndValidateDeterministicSample(t *testing.T) {
	dir := t.TempDir()
	manifest, appErr := synth.Generate(dir, domain.DefaultBaseSeed)
	if appErr != nil {
		t.Fatalf("generate: %v", appErr)
	}
	for _, meta := range manifest.Samples {
		sample, appErr := imaging.LoadSample(dir, meta.ID)
		if appErr != nil {
			t.Fatalf("%s: %v", meta.ID, appErr)
		}
		if sample.Width != meta.Width || sample.Height != meta.Height || sample.Channels != meta.Channels {
			t.Fatalf("%s metadata mismatch", meta.ID)
		}
		if len(sample.Truth.Regions) != meta.RegionCount {
			t.Fatalf("%s truth count mismatch", meta.ID)
		}
		for _, region := range sample.Truth.Regions {
			if region.CharCount != len([]rune(region.Text)) {
				t.Fatalf("%s char count mismatch", meta.ID)
			}
			if region.AvgStrokeWidth <= 0 {
				t.Fatalf("%s stroke width missing", meta.ID)
			}
			if region.Quad == ([4]domain.Point{}) {
				t.Fatalf("%s quad missing", meta.ID)
			}
		}
	}
}

func TestDecodeAndMetaErrors(t *testing.T) {
	t.Run("missing manifest", func(t *testing.T) {
		_, appErr := imaging.LoadSample(t.TempDir(), "missing")
		if appErr == nil || appErr.Code != domain.CodeDecode {
			t.Fatalf("expected E_DECODE, got %v", appErr)
		}
	})

	t.Run("corrupt image", func(t *testing.T) {
		dir := t.TempDir()
		manifest, appErr := synth.Generate(dir, domain.DefaultBaseSeed)
		if appErr != nil {
			t.Fatalf("generate: %v", appErr)
		}
		if err := os.WriteFile(filepath.Join(dir, manifest.Samples[0].Image), []byte("corrupt"), 0o644); err != nil {
			t.Fatalf("corrupt: %v", err)
		}
		_, appErr = imaging.LoadSample(dir, manifest.Samples[0].ID)
		if appErr == nil || appErr.Code != domain.CodeDecode {
			t.Fatalf("expected E_DECODE, got %v", appErr)
		}
	})

	t.Run("width mismatch", func(t *testing.T) {
		dir := t.TempDir()
		manifest, appErr := synth.Generate(dir, domain.DefaultBaseSeed)
		if appErr != nil {
			t.Fatalf("generate: %v", appErr)
		}
		manifest.Samples[0].Width++
		writeManifest(t, dir, manifest)
		_, appErr = imaging.LoadSample(dir, manifest.Samples[0].ID)
		if appErr == nil || appErr.Code != domain.CodeMeta || appErr.Field != "width" {
			t.Fatalf("expected E_META width, got %v", appErr)
		}
		if appErr.Expected == "" || appErr.Actual == "" {
			t.Fatalf("E_META must include expected and actual values")
		}
	})

	t.Run("region count mismatch", func(t *testing.T) {
		dir := t.TempDir()
		manifest, appErr := synth.Generate(dir, domain.DefaultBaseSeed)
		if appErr != nil {
			t.Fatalf("generate: %v", appErr)
		}
		meta := manifest.Samples[0]
		path := filepath.Join(dir, meta.Truth)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read truth: %v", err)
		}
		var truth domain.TruthFile
		if err := json.Unmarshal(data, &truth); err != nil {
			t.Fatalf("decode truth: %v", err)
		}
		truth.RegionCount++
		data, _ = json.MarshalIndent(truth, "", "  ")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("write truth: %v", err)
		}
		_, appErr = imaging.LoadSample(dir, meta.ID)
		if appErr == nil || appErr.Code != domain.CodeMeta || appErr.Field != "truth.region_count" {
			t.Fatalf("expected E_META region count, got %v", appErr)
		}
	})
}

func TestBandParameterValidation(t *testing.T) {
	if _, appErr := imaging.BuildTiles(800, 600, 256, 0); appErr == nil || appErr.Code != domain.CodePara || appErr.Field != "band_width" {
		t.Fatalf("expected zero-band E_PARA, got %v", appErr)
	}
	if _, appErr := imaging.BuildTiles(800, 600, 256, 128); appErr == nil || appErr.Code != domain.CodePara || appErr.Field != "band_width" {
		t.Fatalf("expected half-tile E_PARA, got %v", appErr)
	}
	params := domain.DefaultParams("bitmap5x7-v1")
	params.BandFactor = 0
	if _, appErr := params.EffectiveBand(5); appErr == nil || appErr.Code != domain.CodePara || appErr.Field != "band_factor" {
		t.Fatalf("expected factor E_PARA, got %v", appErr)
	}
}

func writeManifest(t *testing.T, dir string, manifest domain.Manifest) {
	t.Helper()
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}
