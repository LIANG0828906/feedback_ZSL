package spectralrender

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type loadedSamples struct {
	dir   string
	scene *Scene
	hash  string
	truth *TruthManifest
}

func loadSamples(t *testing.T) loadedSamples {
	t.Helper()
	dir := t.TempDir()
	if _, err := GenerateSamples(dir, 42); err != nil {
		t.Fatalf("generate samples: %v", err)
	}
	scene, hash, err := LoadScene(filepath.Join(dir, "scene.json"))
	if err != nil {
		t.Fatalf("load scene: %v", err)
	}
	truth, _, err := LoadTruth(filepath.Join(dir, "truth.json"))
	if err != nil {
		t.Fatalf("load truth: %v", err)
	}
	if terr := ValidateTruthForScene(truth, scene, hash); terr != nil {
		t.Fatalf("validate truth: %v", terr)
	}
	return loadedSamples{dir: dir, scene: scene, hash: hash, truth: truth}
}

func TestSampleGeneratorIsDeterministic(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	ma, err := GenerateSamples(a, 1234)
	if err != nil {
		t.Fatal(err)
	}
	mb, err := GenerateSamples(b, 1234)
	if err != nil {
		t.Fatal(err)
	}
	if ma.SceneHash != mb.SceneHash || ma.TruthHash != mb.TruthHash {
		t.Fatalf("manifest hashes differ: %s/%s vs %s/%s", ma.SceneHash, ma.TruthHash, mb.SceneHash, mb.TruthHash)
	}
	for _, name := range []string{"scene.json", "cmf.json", "truth.json", "manifest.json"} {
		ba, err := os.ReadFile(filepath.Join(a, name))
		if err != nil {
			t.Fatal(err)
		}
		bb, err := os.ReadFile(filepath.Join(b, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(ba, bb) {
			t.Fatalf("%s is not byte-deterministic", name)
		}
	}
}

func readSceneObject(t *testing.T, path string) map[string]any {
	t.Helper()
	v, err := DecodeJSONFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatal("scene root is not an object")
	}
	return m
}

func parseMutatedScene(t *testing.T, root map[string]any) *Error {
	t.Helper()
	b, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	_, perr := ParseSceneJSON(b)
	return perr
}

func TestSceneSchemaValidationPaths(t *testing.T) {
	s := loadSamples(t)
	scenePath := filepath.Join(s.dir, "scene.json")
	cases := []struct {
		name   string
		path   string
		mutate func(map[string]any)
	}{
		{
			name: "missing width", path: "scene.width",
			mutate: func(m map[string]any) { delete(m, "width") },
		},
		{
			name: "non integer coefficient", path: "scene.materials[0].coefficients[0]",
			mutate: func(m map[string]any) {
				mat := m["materials"].([]any)[0].(map[string]any)
				mat["coefficients"].([]any)[0] = json.Number("1.5")
			},
		},
		{
			name: "non monotonic wavelength", path: "scene.spectrum.wavelengths[1]",
			mutate: func(m map[string]any) {
				sp := m["spectrum"].(map[string]any)
				w := sp["wavelengths"].([]any)
				w[1] = w[0]
			},
		},
		{
			name: "cmf length mismatch", path: "scene.spectrum.cmf.x",
			mutate: func(m map[string]any) {
				sp := m["spectrum"].(map[string]any)
				cmf := sp["cmf"].(map[string]any)
				x := cmf["x"].([]any)
				cmf["x"] = x[:len(x)-1]
			},
		},
		{
			name: "light intensity length mismatch", path: "scene.lights[0].intensity",
			mutate: func(m map[string]any) {
				l := m["lights"].([]any)[0].(map[string]any)
				a := l["intensity"].([]any)
				l["intensity"] = a[:len(a)-1]
			},
		},
		{
			name: "vertex outside quantization unit", path: "scene.meshes[0].vertices[0].albedo[0]",
			mutate: func(m map[string]any) {
				mesh := m["meshes"].([]any)[0].(map[string]any)
				v := mesh["vertices"].([]any)[0].(map[string]any)
				v["albedo"].([]any)[0] = json.Number("99999")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := readSceneObject(t, scenePath)
			tc.mutate(root)
			perr := parseMutatedScene(t, root)
			if perr == nil {
				t.Fatal("expected E_SCHEMA")
			}
			if perr.Code != ErrSchema {
				t.Fatalf("code=%s, want %s (%s)", perr.Code, ErrSchema, perr)
			}
			if perr.Path != tc.path {
				t.Fatalf("path=%q, want %q", perr.Path, tc.path)
			}
		})
	}
}

func TestMalformedSceneReturnsDecode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scene.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":1,`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := LoadScene(path)
	if CodeOf(err) != ErrDecode {
		t.Fatalf("code=%s, want %s: %v", CodeOf(err), ErrDecode, err)
	}
}

func TestTruthValidation(t *testing.T) {
	s := loadSamples(t)
	if len(s.truth.Pixels) < 3 {
		t.Fatalf("expected several truth pixels, got %d", len(s.truth.Pixels))
	}
	badHash := *s.truth
	badHash.SceneHash = "not-the-scene"
	if err := ValidateTruthForScene(&badHash, s.scene, s.hash); err == nil || err.Code != ErrSchema {
		t.Fatalf("expected E_SCHEMA for wrong truth scene hash, got %v", err)
	}
}

func TestLoadSampleBundle(t *testing.T) {
	dir := t.TempDir()
	if _, err := GenerateSamples(dir, 42); err != nil {
		t.Fatal(err)
	}
	bundle, err := LoadSampleBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Scene == nil || bundle.Truth == nil || bundle.SceneHash != bundle.Truth.SceneHash {
		t.Fatal("bundle did not load and cross-check scene/truth")
	}
	cmfPath := filepath.Join(dir, "cmf.json")
	cmfBytes, err := os.ReadFile(cmfPath)
	if err != nil {
		t.Fatal(err)
	}
	cmfBytes[len(cmfBytes)/2] ^= 1
	if err := os.WriteFile(cmfPath, cmfBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSampleBundle(dir); err == nil || CodeOf(err) != ErrDecode {
		t.Fatalf("expected E_DECODE for damaged CMF table, got %v", err)
	}
}

func TestErrorHelpers(t *testing.T) {
	err := &Error{Code: ErrGamut, Pixel: &Pixel{1, 2}, Channel: "G"}
	if CodeOf(err) != ErrGamut {
		t.Fatal("CodeOf failed")
	}
	if !errors.As(error(err), new(*Error)) {
		t.Fatal("errors.As should find structured error")
	}
}
