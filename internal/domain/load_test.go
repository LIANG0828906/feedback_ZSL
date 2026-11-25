package domain_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"spectralforge/internal/domain"
	"spectralforge/internal/errs"
	"spectralforge/internal/sample"
)

func generated(t *testing.T) (string, *domain.SampleBundle) {
	t.Helper()
	dir := t.TempDir()
	if _, err := sample.Generate(dir, 20261125); err != nil {
		t.Fatalf("generate: %v", err)
	}
	b, err := domain.LoadBundle(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return dir, b
}

func readObject(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return obj
}

func writeObject(t *testing.T, path string, obj any) {
	t.Helper()
	data, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func wantCode(t *testing.T, err error, code errs.Code) *errs.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil", code)
	}
	e, ok := errs.As(err)
	if !ok {
		t.Fatalf("expected structured %s, got %T: %v", code, err, err)
	}
	if e.Code != code {
		t.Fatalf("expected %s, got %s: %v", code, e.Code, e)
	}
	return e
}

func TestMissingFieldReturnsSchema(t *testing.T) {
	dir, _ := generated(t)
	path := filepath.Join(dir, domain.SceneFileName)
	scene := readObject(t, path)
	vertices := scene["meshes"].([]any)[0].(map[string]any)["vertices"].([]any)
	delete(vertices[0].(map[string]any), "roughness")
	writeObject(t, path, scene)
	_, err := domain.LoadBundle(dir)
	e := wantCode(t, err, errs.CodeSchema)
	if e.Field != "scene.meshes[0].vertices[0].roughness" {
		t.Fatalf("unexpected field %q", e.Field)
	}
}

func TestOutOfRangeReturnsParaWithFieldAndRange(t *testing.T) {
	dir, _ := generated(t)
	path := filepath.Join(dir, domain.SceneFileName)
	scene := readObject(t, path)
	vertices := scene["meshes"].([]any)[0].(map[string]any)["vertices"].([]any)
	vertices[0].(map[string]any)["roughness"] = 1.25
	writeObject(t, path, scene)
	_, err := domain.LoadBundle(dir)
	e := wantCode(t, err, errs.CodePara)
	if e.Field != "scene.meshes[0].vertices[0].roughness" || e.Range != "[0,1]" {
		t.Fatalf("unexpected error metadata: %+v", e)
	}
}

func TestBasisCoefficientRangeIsEnforced(t *testing.T) {
	dir, _ := generated(t)
	path := filepath.Join(dir, domain.SceneFileName)
	scene := readObject(t, path)
	objects := scene["objects"].([]any)
	objects[0].(map[string]any)["coefficients"].([]any)[0] = float64(999999)
	writeObject(t, path, scene)
	_, err := domain.LoadBundle(dir)
	e := wantCode(t, err, errs.CodePara)
	if e.Field != "scene.objects[0].coefficients[0]" {
		t.Fatalf("unexpected field %q", e.Field)
	}
}

func TestWavelengthAndCMFAlignment(t *testing.T) {
	dir, _ := generated(t)
	path := filepath.Join(dir, domain.SpectralFileName)
	spectral := readObject(t, path)
	cmf := spectral["cmf"].(map[string]any)
	cmf["x"] = cmf["x"].([]any)[:len(cmf["x"].([]any))-1]
	writeObject(t, path, spectral)
	_, err := domain.LoadBundle(dir)
	e := wantCode(t, err, errs.CodePara)
	if e.Field != "spectral.cmf" {
		t.Fatalf("unexpected field %q", e.Field)
	}
}

func TestWavelengthsMustStrictlyIncrease(t *testing.T) {
	dir, _ := generated(t)
	path := filepath.Join(dir, domain.SpectralFileName)
	spectral := readObject(t, path)
	wavelengths := spectral["wavelengths"].([]any)
	wavelengths[4] = wavelengths[3]
	writeObject(t, path, spectral)
	_, err := domain.LoadBundle(dir)
	e := wantCode(t, err, errs.CodePara)
	if e.Field != "spectral.wavelengths[4]" {
		t.Fatalf("unexpected field %q", e.Field)
	}
}

func TestVertexMaterialCountMustMatchDeclaration(t *testing.T) {
	dir, _ := generated(t)
	path := filepath.Join(dir, domain.SceneFileName)
	scene := readObject(t, path)
	scene["meshes"].([]any)[0].(map[string]any)["vertex_count"] = 99
	writeObject(t, path, scene)
	_, err := domain.LoadBundle(dir)
	e := wantCode(t, err, errs.CodePara)
	if e.Field != "scene.meshes[0].vertex_count" {
		t.Fatalf("unexpected field %q", e.Field)
	}
}

func TestMalformedJSONReturnsDecode(t *testing.T) {
	dir, _ := generated(t)
	if err := os.WriteFile(filepath.Join(dir, domain.SpectralFileName), []byte(`{"bad"`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := domain.LoadBundle(dir)
	wantCode(t, err, errs.CodeDecode)
}

func TestTruthHashAndEntryPoint(t *testing.T) {
	_, b := generated(t)
	if b.Truth.SceneHash != b.ContentHash {
		t.Fatalf("truth hash mismatch")
	}
	if b.Truth.Source != "analytic_generator_v1" || len(b.Truth.Points) < 4 {
		t.Fatalf("truth provenance/points missing")
	}
	for _, p := range b.Truth.Points {
		if p.ExpectedPeakRadiance < 0 || p.ExpectedChromaticity[0] <= 0 || p.ExpectedChromaticity[1] <= 0 {
			t.Fatalf("invalid truth point: %+v", p)
		}
	}
}
