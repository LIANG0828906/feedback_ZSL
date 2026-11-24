package model

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"volumediag/internal/diag"
)

func sampleBytes(t *testing.T) (map[string]any, map[string]any) {
	t.Helper()
	sceneRaw, err := os.ReadFile(filepath.Join("..", "..", "artifacts", "sample", "scene.json"))
	if err != nil {
		t.Fatal(err)
	}
	truthRaw, err := os.ReadFile(filepath.Join("..", "..", "artifacts", "sample", "truth.json"))
	if err != nil {
		t.Fatal(err)
	}
	var sceneObj map[string]any
	if err := json.Unmarshal(sceneRaw, &sceneObj); err != nil {
		t.Fatal(err)
	}
	var truthObj map[string]any
	if err := json.Unmarshal(truthRaw, &truthObj); err != nil {
		t.Fatal(err)
	}
	return sceneObj, truthObj
}

func decodeSceneObj(t *testing.T, obj map[string]any) error {
	t.Helper()
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	_, err = DecodeScene(raw)
	return err
}

func decodeTruthObj(t *testing.T, sceneObj map[string]any, truthObj map[string]any) error {
	t.Helper()
	sceneRaw, _ := json.Marshal(sceneObj)
	scene, err := DecodeScene(sceneRaw)
	if err != nil {
		t.Fatal(err)
	}
	truthRaw, _ := json.Marshal(truthObj)
	_, err = DecodeTruth(truthRaw, scene)
	return err
}

func TestLoaderErrorCodes(t *testing.T) {
	sceneObj, truthObj := sampleBytes(t)

	t.Run("schema missing light field", func(t *testing.T) {
		obj := cloneMap(t, sceneObj)
		light := obj["lights"].([]any)[0].(map[string]any)
		delete(light, "intensity")
		err := decodeSceneObj(t, obj)
		if diag.CodeOf(err) != diag.ESchema {
			t.Fatalf("code=%s err=%v", diag.CodeOf(err), err)
		}
		de := diag.As(err)
		if de == nil || de.LightID != "point_key" || !strings.Contains(de.Field, "intensity") {
			t.Fatalf("missing location: %#v", de)
		}
	})

	t.Run("reference missing occluder", func(t *testing.T) {
		obj := cloneMap(t, sceneObj)
		light := obj["lights"].([]any)[0].(map[string]any)
		light["occluder_ids"] = []any{"missing_occluder"}
		err := decodeSceneObj(t, obj)
		if diag.CodeOf(err) != diag.ERef {
			t.Fatalf("code=%s err=%v", diag.CodeOf(err), err)
		}
		de := diag.As(err)
		if de == nil || de.LightID != "point_key" {
			t.Fatalf("missing light id: %#v", de)
		}
	})

	t.Run("negative extinction", func(t *testing.T) {
		obj := cloneMap(t, sceneObj)
		medium := obj["medium"].(map[string]any)
		medium["extinction"] = []any{-0.1, 0.1, 0.1}
		err := decodeSceneObj(t, obj)
		if diag.CodeOf(err) != diag.EPara {
			t.Fatalf("code=%s err=%v", diag.CodeOf(err), err)
		}
		if de := diag.As(err); de == nil || de.Field != "scene.medium.extinction" {
			t.Fatalf("missing field: %#v", de)
		}
	})

	t.Run("corrupt depth grid", func(t *testing.T) {
		obj := cloneMap(t, sceneObj)
		mesh := obj["geometry"].([]any)[0].(map[string]any)
		depths := mesh["depths"].([]any)
		mesh["depths"] = depths[:len(depths)-1]
		err := decodeSceneObj(t, obj)
		if diag.CodeOf(err) != diag.EDecode {
			t.Fatalf("code=%s err=%v", diag.CodeOf(err), err)
		}
	})

	t.Run("truth coverage missing pixel", func(t *testing.T) {
		sceneCopy := cloneMap(t, sceneObj)
		truthCopy := cloneMap(t, truthObj)
		truthCopy["pixels"] = truthCopy["pixels"].([]any)[1:]
		err := decodeTruthObj(t, sceneCopy, truthCopy)
		if diag.CodeOf(err) != diag.ESchema {
			t.Fatalf("code=%s err=%v", diag.CodeOf(err), err)
		}
		de := diag.As(err)
		if de == nil || de.PixelX != 0 || de.PixelY != 0 {
			t.Fatalf("missing first pixel: %#v", de)
		}
	})
}

func cloneMap(t *testing.T, in map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
