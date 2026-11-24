package app

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"cadlab/internal/engine"
	"cadlab/internal/model"
	"cadlab/internal/sample"
)

func TestAPIPathsCheckpointAndVersionMixing(t *testing.T) {
	dir := t.TempDir()
	opts := engine.DefaultOptions(dir)
	opts.DBPath = filepath.Join(dir, "api.sqlite")
	opts.Width, opts.Height = 180, 120
	opts.TileCols, opts.TileRows = 4, 3
	opts.ExactDensity = 10
	opts.PreviewDensity = 4
	a, err := New(opts, sample.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	boot := a.Call("bootstrap", "{}")
	if !boot.OK || len(boot.Data) == 0 {
		t.Fatalf("bootstrap failed: %+v", boot)
	}
	if res := a.Call("exact", "{}"); !res.OK {
		t.Fatalf("exact failed: %+v", res)
	}
	if res := a.Call("preview", "{}"); !res.OK {
		t.Fatalf("preview failed: %+v", res)
	}
	if res := a.Call("validatePaths", "{}"); !res.OK {
		t.Fatalf("path metadata should match at version 1: %+v", res)
	}
	mat := model.Material{
		BaseColor: model.Color{R: 210, G: 70, B: 45},
		Ambient:   0.2, Diffuse: 0.78, Specular: 0.8, Shininess: 24,
	}
	payload, _ := json.Marshal(map[string]any{"patchId": 2, "material": mat})
	if res := a.Call("applyMaterial", string(payload)); !res.OK {
		t.Fatalf("material edit failed: %+v", res)
	}
	res := a.Call("validatePaths", "{}")
	if res.OK || res.Error == nil || res.Error.Code != model.CodeVersion ||
		res.Error.VersionA != 1 || res.Error.VersionB != 2 {
		t.Fatalf("expected E_VER v1<->v2, got %+v", res)
	}
	cancel := a.Call("cancel", "{}")
	if !cancel.OK {
		t.Fatalf("cancel action failed: %+v", cancel)
	}
}
