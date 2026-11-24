package pipeline_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"virtualstage/pipeline"
	"virtualstage/sample"
)

func TestLoadProjectVersionsAndTruth(t *testing.T) {
	work := t.TempDir()
	result, err := sample.Generate(sample.Options{OutDir: work, Seed: 99})
	if err != nil {
		t.Fatal(err)
	}
	model, manifest, err := pipeline.LoadProject(result.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.WindowSize != 6 {
		t.Fatalf("window size = %d", manifest.WindowSize)
	}
	if len(model.Layers) != 3 {
		t.Fatalf("layers = %d", len(model.Layers))
	}
	if len(model.SourceVersions) != 3 || model.SourceVersions[0] != 1 || model.SourceVersions[2] != 3 {
		t.Fatalf("source versions = %v", model.SourceVersions)
	}
	versions := map[int]bool{}
	for _, layer := range model.Layers {
		versions[layer.SourceVersion] = true
	}
	for _, version := range []int{1, 2, 3} {
		if !versions[version] {
			t.Fatalf("missing layer from source version %d", version)
		}
	}
	if got := model.TimeBase.Timestamp(1); got != pipeline.NewRational(1, 24) {
		t.Fatalf("timestamp = %v", got)
	}
	for _, layer := range model.Layers {
		if layer.InFrame < 0 || layer.VisibleUntilFrame > model.FrameCount {
			t.Fatalf("invalid layer endpoint: %+v", layer)
		}
	}
}

func TestStandaloneTimeBaseUnits(t *testing.T) {
	work := t.TempDir()
	result, err := sample.Generate(sample.Options{OutDir: work, Seed: 11})
	if err != nil {
		t.Fatal(err)
	}
	v2, err := pipeline.LoadFile(result.Sources[1])
	if err != nil {
		t.Fatal(err)
	}
	v3, err := pipeline.LoadFile(result.Sources[2])
	if err != nil {
		t.Fatal(err)
	}
	v2Frame, err := pipeline.CompositeFrame(v2, 1, pipeline.CompositeConfig{MaxFrameBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if v2Frame.TimeUnit != "second" || v2Frame.Timestamp != pipeline.NewRational(1, 24) {
		t.Fatalf("v2 timestamp = %v %s", v2Frame.Timestamp, v2Frame.TimeUnit)
	}
	v3Frame, err := pipeline.CompositeFrame(v3, 1, pipeline.CompositeConfig{MaxFrameBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if v3Frame.TimeUnit != "tick" || v3Frame.Timestamp != pipeline.NewRational(1000, 1) {
		t.Fatalf("v3 timestamp = %v %s", v3Frame.Timestamp, v3Frame.TimeUnit)
	}
}

func TestLoaderErrors(t *testing.T) {
	work := t.TempDir()
	result, err := sample.Generate(sample.Options{OutDir: work, Seed: 3})
	if err != nil {
		t.Fatal(err)
	}
	v1Path := result.Sources[0]
	v3Path := result.Sources[2]

	t.Run("E_VER unknown format", func(t *testing.T) {
		path := mutateJSON(t, v1Path, func(root map[string]any) {
			root["format"] = "foreground-composite/v9"
		})
		_, err := pipeline.LoadFile(path)
		assertCode(t, err, pipeline.CodeVersion)
	})

	t.Run("E_VER missing required", func(t *testing.T) {
		path := mutateJSON(t, v1Path, func(root map[string]any) {
			scene := root["scene"].(map[string]any)
			delete(scene, "width")
		})
		_, err := pipeline.LoadFile(path)
		assertCode(t, err, pipeline.CodeVersion)
	})

	t.Run("E_SCHEMA path and field", func(t *testing.T) {
		path := mutateJSON(t, v1Path, func(root map[string]any) {
			scene := root["scene"].(map[string]any)
			scene["width"] = "wide"
		})
		_, err := pipeline.LoadFile(path)
		var pipelineErr *pipeline.Error
		if !errors.As(err, &pipelineErr) || pipelineErr.Code != pipeline.CodeSchema {
			t.Fatalf("error = %v", err)
		}
		if pipelineErr.Path != "$.scene.width" || pipelineErr.Field != "width" {
			t.Fatalf("location = %q %q", pipelineErr.Path, pipelineErr.Field)
		}
	})

	t.Run("E_DECODE malformed JSON", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "broken.json")
		if err := os.WriteFile(path, []byte(`{"format":`), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := pipeline.LoadFile(path)
		assertCode(t, err, pipeline.CodeDecode)
	})

	t.Run("E_LAYER missing stack target", func(t *testing.T) {
		path := mutateJSON(t, v1Path, func(root map[string]any) {
			root["layer_order"] = []any{"missing-layer"}
		})
		_, err := pipeline.LoadFile(path)
		var pipelineErr *pipeline.Error
		if !errors.As(err, &pipelineErr) || pipelineErr.Code != pipeline.CodeLayer {
			t.Fatalf("error = %v", err)
		}
		if pipelineErr.LayerID != "missing-layer" {
			t.Fatalf("layer id = %q", pipelineErr.LayerID)
		}
	})

	t.Run("E_LAYER hierarchy cycle", func(t *testing.T) {
		path := mutateJSON(t, v3Path, func(root map[string]any) {
			layers := root["layers"].([]any)
			layer := layers[0].(map[string]any)
			layer["parent"] = "character"
		})
		_, err := pipeline.LoadFile(path)
		var pipelineErr *pipeline.Error
		if !errors.As(err, &pipelineErr) || pipelineErr.Code != pipeline.CodeLayer {
			t.Fatalf("error = %v", err)
		}
		if pipelineErr.LayerID != "character" {
			t.Fatalf("layer id = %q", pipelineErr.LayerID)
		}
	})
}

func mutateJSON(t *testing.T, source string, mutate func(map[string]any)) string {
	t.Helper()
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	mutate(root)
	raw, err = json.MarshalIndent(root, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "mutated.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertCode(t *testing.T, err error, code string) {
	t.Helper()
	var pipelineErr *pipeline.Error
	if !errors.As(err, &pipelineErr) {
		t.Fatalf("expected pipeline error, got %v", err)
	}
	if pipelineErr.Code != code {
		t.Fatalf("code = %s, want %s (%v)", pipelineErr.Code, code, err)
	}
}
