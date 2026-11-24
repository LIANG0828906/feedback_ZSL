package schema_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"occlusionlab/internal/generator"
	"occlusionlab/internal/model"
	"occlusionlab/internal/schema"
)

func setupProject(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "project")
	if _, err := generator.GenerateToDir(dir, generator.DefaultConfig(1042)); err != nil {
		t.Fatal(err)
	}
	return dir
}

func readMap(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func refreshWorldHash(t *testing.T, dir string) {
	t.Helper()
	world, err := os.ReadFile(filepath.Join(dir, "world.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest := readMap(t, filepath.Join(dir, "manifest.json"))
	sum := sha256.Sum256(world)
	manifest["world_sha256"] = hex.EncodeToString(sum[:])
	writeJSON(t, filepath.Join(dir, "manifest.json"), manifest)
}

func TestMissingChunkFieldIsLocated(t *testing.T) {
	dir := setupProject(t)
	world := readMap(t, filepath.Join(dir, "world.json"))
	chunks := world["chunks"].([]any)
	delete(chunks[0].(map[string]any), "bounds")
	writeJSON(t, filepath.Join(dir, "world.json"), world)
	refreshWorldHash(t, dir)

	_, err := schema.LoadProject(dir)
	if err == nil || err.Code != model.CodeSchema || err.Chunk != 0 || err.Field != "chunks[0].bounds" {
		t.Fatalf("expected E_SCHEMA at chunks[0].bounds, got %+v", err)
	}
}

func TestInvalidBoundingBoxIsLocated(t *testing.T) {
	dir := setupProject(t)
	world := readMap(t, filepath.Join(dir, "world.json"))
	chunks := world["chunks"].([]any)
	bounds := chunks[0].(map[string]any)["bounds"].(map[string]any)
	min := bounds["min"].(map[string]any)
	max := bounds["max"].(map[string]any)
	max["x"] = min["x"].(float64) - 1
	writeJSON(t, filepath.Join(dir, "world.json"), world)
	refreshWorldHash(t, dir)

	_, err := schema.LoadProject(dir)
	if err == nil || err.Code != model.CodeSchema || err.Chunk != 0 || err.Field != "chunks[0].bounds" {
		t.Fatalf("expected E_SCHEMA at chunks[0].bounds, got %+v", err)
	}
}

func TestInvalidOccluderReferenceIsLocated(t *testing.T) {
	dir := setupProject(t)
	world := readMap(t, filepath.Join(dir, "world.json"))
	chunks := world["chunks"].([]any)
	refs := chunks[0].(map[string]any)["occluder_refs"].([]any)
	refs[0] = "missing_occluder"
	writeJSON(t, filepath.Join(dir, "world.json"), world)
	refreshWorldHash(t, dir)

	_, err := schema.LoadProject(dir)
	if err == nil || err.Code != model.CodeSchema || err.Chunk != 0 ||
		err.Field != "chunks[0].occluder_refs[0]" {
		t.Fatalf("expected E_SCHEMA at chunks[0].occluder_refs[0], got %+v", err)
	}
}

func TestNonMonotonicCameraTimeIsLocated(t *testing.T) {
	dir := setupProject(t)
	world := readMap(t, filepath.Join(dir, "world.json"))
	cameras := world["cameras"].([]any)
	firstTime := cameras[0].(map[string]any)["time"]
	cameras[1].(map[string]any)["time"] = firstTime
	writeJSON(t, filepath.Join(dir, "world.json"), world)
	refreshWorldHash(t, dir)

	_, err := schema.LoadProject(dir)
	if err == nil || err.Code != model.CodeSchema || err.Window != 1 ||
		err.Field != "cameras[1].time" {
		t.Fatalf("expected E_SCHEMA at cameras[1].time, got %+v", err)
	}
}

func TestParameterValidationNamesField(t *testing.T) {
	params := model.Params{
		Quantization: model.Quantization{
			WorldFracBits: 8, BasisFracBits: 16, ScreenFracBits: 8, ParamFracBits: 16,
		},
		RasterResolution: 512, MaxWorkers: 2, RasterBufferCells: 128 * 128,
		MaxResident: 15, LoadCostBudget: 1000, TolerancePxQ: 512, Version: 1,
	}
	err := schema.ValidateParams(params)
	if err == nil || err.Code != model.CodeParameters || err.Field != "raster_resolution" {
		t.Fatalf("expected E_PARA at raster_resolution, got %+v", err)
	}
	params.RasterResolution = 256
	err = schema.ValidateParams(params)
	if err == nil || err.Code != model.CodeParameters || err.Field != "raster_resolution" {
		t.Fatalf("expected E_PARA for per-thread raster budget, got %+v", err)
	}
}
