package anomaly

import (
	"path/filepath"
	"testing"

	"serviceregistry/checkpoint"
	"serviceregistry/registry"
	"serviceregistry/samplegen"
)

func TestAnomalySuite(t *testing.T) {
	bundle, err := samplegen.BuildBundle(samplegen.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	document, err := registry.LoadDocument(bundle.Files[registry.SchemaV1])
	if err != nil {
		t.Fatal(err)
	}
	run, snapshots, err := registry.ComputeProject(document.Project, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	store := &checkpoint.Store{Path: path}
	if err := store.Save(&checkpoint.Checkpoint{
		Snapshot: snapshots[2],
		Steps:    append([]registry.StepResult(nil), run.Steps[:3]...),
	}); err != nil {
		t.Fatal(err)
	}
	results, err := RunSuite(Input{
		Project:        document.Project,
		Documents:      bundle.Files,
		CheckpointPath: path,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 11 {
		t.Fatalf("got %d anomaly cases, want 11", len(results))
	}
	for _, result := range results {
		if !result.Passed {
			t.Fatalf("%s failed: %+v", result.Kind, result)
		}
		if result.ActualCode != result.ExpectedCode {
			t.Fatalf("%s code = %s, want %s", result.Kind, result.ActualCode, result.ExpectedCode)
		}
		if !result.SourceUnchanged || !result.CatalogUsable {
			t.Fatalf("%s modified source or damaged catalog: %+v", result.Kind, result)
		}
	}
	if _, err := store.Load(); err != nil {
		t.Fatalf("primary checkpoint became unreadable: %v", err)
	}
}
