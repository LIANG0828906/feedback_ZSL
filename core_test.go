package volumefog

import "testing"

func testScene(t *testing.T) *Scene {
	t.Helper()
	scene, err := GenerateScene(DefaultGenerateOptions())
	if err != nil {
		t.Fatalf("GenerateScene: %v", err)
	}
	return scene
}

func TestGeneratedTruthMatchesCore(t *testing.T) {
	scene := testScene(t)
	if err := ValidateTruth(scene); err != nil {
		t.Fatalf("ValidateTruth: %v", err)
	}
}

func TestGeneratedSceneJSONRoundTrip(t *testing.T) {
	scene := testScene(t)
	data, err := MarshalSceneJSON(scene)
	if err != nil {
		t.Fatalf("MarshalSceneJSON: %v", err)
	}
	loaded, err := LoadSceneJSON(data)
	if err != nil {
		t.Fatalf("LoadSceneJSON: %v", err)
	}
	if loaded.SnapshotVersion() != scene.SnapshotVersion() {
		t.Fatalf("snapshot version changed across round-trip")
	}
	if loaded.Truth == nil || len(loaded.Truth.Chunks) != len(scene.Truth.Chunks) {
		t.Fatalf("truth did not round-trip")
	}
}
