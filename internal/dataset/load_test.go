package dataset_test

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"qualitybench/internal/core"
	"qualitybench/internal/dataset"
	"qualitybench/internal/generator"
	"qualitybench/internal/model"
)

func TestMissingFieldReturnsSchemaPath(t *testing.T) {
	path := writeMutatedProject(t, func(m map[string]any) {
		delete(m, "schema_version")
	})
	_, _, err := dataset.LoadProject(path)
	assertCodePath(t, err, core.CodeSchema, "schema_version")
}

func TestShapeOutsideFrameReturnsRange(t *testing.T) {
	path := writeMutatedProject(t, func(m map[string]any) {
		images := m["images"].([]any)
		shapes := images[0].(map[string]any)["shapes"].([]any)
		shape := shapes[0].(map[string]any)
		bbox := shape["bbox"].(map[string]any)
		bbox["x"] = -4.0
	})
	_, _, err := dataset.LoadProject(path)
	assertCodePath(t, err, core.CodeRange, "images[0].shapes[0].bbox")
}

func TestWatermarkShapeConflictReturnsRange(t *testing.T) {
	path := writeMutatedProject(t, func(m map[string]any) {
		images := m["images"].([]any)
		image := images[0].(map[string]any)
		shapes := image["shapes"].([]any)
		bbox := shapes[0].(map[string]any)["bbox"].(map[string]any)
		image["watermark"] = map[string]any{"x": bbox["x"], "y": bbox["y"], "w": 10.0, "h": 10.0}
	})
	_, _, err := dataset.LoadProject(path)
	if core.CodeOf(err) != core.CodeRange {
		t.Fatalf("expected E_RANGE, got %v", err)
	}
}

func TestBadBlockSizeReturnsCandidates(t *testing.T) {
	path := writeMutatedProject(t, func(m map[string]any) {
		m["block_size"] = 20.0
	})
	_, _, err := dataset.LoadProject(path)
	if core.CodeOf(err) != core.CodeBlock {
		t.Fatalf("expected E_BLOCK, got %v", err)
	}
	e, _ := core.As(err)
	candidates, ok := e.Details["candidates"].([]int)
	if !ok || len(candidates) == 0 {
		t.Fatalf("missing divisor candidates: %+v", e.Details)
	}
}

func TestInvalidStrengthReturnsStepAndField(t *testing.T) {
	path := writeMutatedProject(t, func(m map[string]any) {
		chains := m["chains"].([]any)
		steps := chains[0].(map[string]any)["steps"].([]any)
		steps[0].(map[string]any)["strength"] = 1.75
	})
	_, _, err := dataset.LoadProject(path)
	if core.CodeOf(err) != core.CodePara {
		t.Fatalf("expected E_PARA, got %v", err)
	}
	e, _ := core.As(err)
	if e.Step != 1 || e.Field != "strength" {
		t.Fatalf("expected step 1 strength, got step=%d field=%s", e.Step, e.Field)
	}
}

func TestNonFiniteSampleReturnsRange(t *testing.T) {
	ds, err := generator.Generate(generator.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	ds.Truth.Images[0].Samples[0].X = math.NaN()
	err = model.ValidateTruth(ds.Truth, ds.Project)
	if core.CodeOf(err) != core.CodeRange {
		t.Fatalf("expected E_RANGE, got %v", err)
	}
}

func writeMutatedProject(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	ds, err := generator.Generate(generator.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(ds.Project)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	mutate(m)
	data, err = json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "project.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertCodePath(t *testing.T, err error, code core.Code, path string) {
	t.Helper()
	if core.CodeOf(err) != code {
		t.Fatalf("expected %s, got %v", code, err)
	}
	e, _ := core.As(err)
	if e.Path != path {
		t.Fatalf("expected path %q, got %q (%v)", path, e.Path, err)
	}
}
