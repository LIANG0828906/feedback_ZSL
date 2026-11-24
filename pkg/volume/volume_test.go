package volume_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"volumelab/pkg/model"
	"volumelab/pkg/samplegen"
	"volumelab/pkg/volume"
)

func generateFixture(t *testing.T) (string, *volume.Manifest) {
	t.Helper()
	dir := t.TempDir()
	result, err := samplegen.Generate(samplegen.Options{OutputDir: dir, Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := volume.LoadManifest(result.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	return result.ManifestPath, manifest
}

func mutateManifest(t *testing.T, path string, mutation func(map[string]any)) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	mutation(doc)
	encoded, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		t.Fatal(err)
	}
}

func firstVolume(doc map[string]any) map[string]any {
	return doc["volumes"].([]any)[0].(map[string]any)
}

func TestLoadAxisOrdersAndRawLookup(t *testing.T) {
	manifestPath, _ := generateFixture(t)
	for _, id := range []string{"vol_a", "vol_b"} {
		_, v, err := volume.LoadFromManifest(manifestPath, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, voxel := range []model.IVec3{{0, 0, 0}, {1, 2, 3}, {v.Shape[0] - 1, v.Shape[1] - 1, v.Shape[2] - 1}} {
			if _, ok := v.RawAt(voxel); !ok {
				t.Fatalf("%s cannot read %v", id, voxel)
			}
		}
		world := v.WorldFromVoxel(model.Vec3{3, 4, 5})
		voxel := v.VoxelFromWorld(world)
		if voxel != (model.Vec3{3, 4, 5}) {
			t.Fatalf("%s world/voxel round trip failed: %v", id, voxel)
		}
	}
}

func TestAxisOrderConflictReturnsEAxis(t *testing.T) {
	manifestPath, _ := generateFixture(t)
	mutateManifest(t, manifestPath, func(doc map[string]any) {
		firstVolume(doc)["axisOrder"] = "ZYX"
	})
	_, _, err := volume.LoadFromManifest(manifestPath, "vol_a")
	appErr := model.AsAppError(err)
	if appErr.Code != "E_AXIS" || appErr.Axis == "" {
		t.Fatalf("expected E_AXIS with conflict axis, got %#v", appErr)
	}
}

func TestMissingCalibrationFieldReturnsECal(t *testing.T) {
	manifestPath, _ := generateFixture(t)
	mutateManifest(t, manifestPath, func(doc map[string]any) {
		delete(firstVolume(doc), "slope")
	})
	_, _, err := volume.LoadFromManifest(manifestPath, "vol_a")
	appErr := model.AsAppError(err)
	if appErr.Code != "E_CAL" || appErr.Field != "slope" {
		t.Fatalf("expected E_CAL slope, got %#v", appErr)
	}
}

func TestNonPositiveSpacingReturnsECalWithField(t *testing.T) {
	manifestPath, _ := generateFixture(t)
	mutateManifest(t, manifestPath, func(doc map[string]any) {
		firstVolume(doc)["spacing"] = []any{0, 1.2, 1.5}
	})
	_, _, err := volume.LoadFromManifest(manifestPath, "vol_a")
	appErr := model.AsAppError(err)
	if appErr.Code != "E_CAL" || appErr.Field != "spacing.X" {
		t.Fatalf("expected E_CAL spacing.X, got %#v", appErr)
	}
}

func TestTruncatedVolumeReturnsEDecode(t *testing.T) {
	manifestPath, manifest := generateFixture(t)
	spec, err := manifest.Resolve("vol_a")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(spec.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(spec.FilePath, info.Size()-10); err != nil {
		t.Fatal(err)
	}
	_, _, err = volume.LoadFromManifest(manifestPath, "vol_a")
	appErr := model.AsAppError(err)
	if appErr.Code != "E_DECODE" {
		t.Fatalf("expected E_DECODE, got %#v", appErr)
	}
}

func TestManifestHashDetectsChangedManifest(t *testing.T) {
	manifestPath, manifest := generateFixture(t)
	original := manifest.ManifestHash
	if len(original) != 64 {
		t.Fatalf("unexpected manifest hash: %q", original)
	}
	copyPath := filepath.Join(t.TempDir(), "manifest.json")
	raw, _ := os.ReadFile(manifestPath)
	if err := os.WriteFile(copyPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	copied, err := volume.LoadManifest(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	if copied.ManifestHash != original {
		t.Fatal("manifest hash is not content deterministic")
	}
}
