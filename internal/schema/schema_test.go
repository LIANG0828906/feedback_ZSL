package schema_test

import (
	"encoding/json"
	"testing"

	"presentkernel/internal/domain"
	"presentkernel/internal/engine"
	"presentkernel/internal/generator"
	"presentkernel/internal/schema"
)

func TestThreeSchemaVersionsLoadAndMatchTruth(t *testing.T) {
	set, err := generator.GenerateSampleSet()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		version string
		data    []byte
	}{
		{domain.SchemaV1, set.V1},
		{domain.SchemaV2, set.V2},
		{domain.SchemaV3, set.V3},
	} {
		loaded, err := schema.LoadProject(tc.data)
		if err != nil {
			t.Fatalf("load %s: %v", tc.version, err)
		}
		if loaded.Project.SchemaVersion != tc.version {
			t.Fatalf("source version = %s want %s", loaded.Project.SchemaVersion, tc.version)
		}
		if len(loaded.Trace.Fields) == 0 && tc.version != domain.SchemaV3 {
			t.Fatalf("expected field conversion traces for %s", tc.version)
		}
		results, err := engine.BuildAll(loaded.Project, engine.Options{FPS: loaded.Project.TargetFPS})
		if err != nil {
			t.Fatalf("build %s: %v", tc.version, err)
		}
		if len(results) != len(set.TruthManifest.Partitions) {
			t.Fatalf("%s result count %d", tc.version, len(results))
		}
		for i := range results {
			if results[i].Digest != set.TruthManifest.Partitions[i].Digest {
				t.Fatalf("%s partition %s digest mismatch", tc.version, results[i].PartitionID)
			}
		}
	}
}

func TestSchemaErrorsAreStructured(t *testing.T) {
	_, err := schema.LoadProject([]byte(`{"schemaVersion":"9.9.9"}`))
	de, ok := domain.AsError(err)
	if !ok || de.Code != domain.CodeSchema || de.Path != "schemaVersion" {
		t.Fatalf("unknown version error = %+v", err)
	}

	set, err := generator.GenerateSampleSet()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(set.V1, &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc, "project_id")
	bad, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	_, err = schema.LoadProject(bad)
	de, ok = domain.AsError(err)
	if !ok || de.Code != domain.CodeSchema || de.Path != "project_id" {
		t.Fatalf("missing project_id error = %+v", err)
	}
}

func TestExportLossyListAndReload(t *testing.T) {
	p := setCanonical(t)
	for _, version := range []string{domain.SchemaV1, domain.SchemaV2, domain.SchemaV3} {
		res, err := schema.Export(version, p)
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := schema.LoadProject(res.Data)
		if err != nil {
			t.Fatalf("reload %s: %v", version, err)
		}
		results, err := engine.BuildAll(loaded.Project, engine.Options{FPS: loaded.Project.TargetFPS})
		if err != nil {
			t.Fatalf("rebuild %s: %v", version, err)
		}
		truth, err := engine.BuildAll(p, engine.Options{FPS: p.TargetFPS})
		if err != nil {
			t.Fatal(err)
		}
		for i := range results {
			if results[i].Digest != truth[i].Digest {
				t.Fatalf("export %s reload changed partition %s", version, results[i].PartitionID)
			}
		}
		if version == domain.SchemaV1 && len(res.Lossy) < 3 {
			t.Fatalf("expected v1 E_LOSSY records, got %d", len(res.Lossy))
		}
		for _, issue := range res.Lossy {
			if issue.Code != domain.CodeLossy || issue.Disposition == "" {
				t.Fatalf("lossy issue lacks disposition: %+v", issue)
			}
		}
	}
}

func setCanonical(t *testing.T) *domain.Project {
	t.Helper()
	set, err := generator.GenerateSampleSet()
	if err != nil {
		t.Fatal(err)
	}
	return set.Canonical
}
