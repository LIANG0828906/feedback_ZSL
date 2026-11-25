package loader

import (
	"encoding/json"
	"strings"
	"testing"

	"example.com/columnar/domain"
	"example.com/columnar/generator"
)

func fixture(t *testing.T) *generator.Sample {
	t.Helper()
	s, err := generator.Generate(generator.Seed)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestThreeVersionsLoadEquivalent(t *testing.T) {
	s := fixture(t)
	var projects []domain.Project
	for _, name := range s.VersionOrder {
		p, err := Load(s.VersionJSON[name])
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(p.Families) != 3 || len(p.Batches) != 8 || len(p.Operations) != 11 || len(p.Scans) != 8 {
			t.Fatalf("%s counts: families=%d batches=%d ops=%d scans=%d", name, len(p.Families), len(p.Batches), len(p.Operations), len(p.Scans))
		}
		if p.TimeUnit != "ms" || p.SizeUnit != "bytes" {
			t.Fatalf("%s units not normalized: %s/%s", name, p.TimeUnit, p.SizeUnit)
		}
		projects = append(projects, p)
	}
	if err := CompareCanonical(projects); err != nil {
		t.Fatal(err)
	}
	if projects[0].Families[0].ID != "core" || projects[0].Families[1].ID != "attrs" || projects[0].Families[2].ID != "metrics" {
		t.Fatalf("family order changed: %+v", projects[0].Families)
	}
}

func TestStrictUnknownFieldAndMissingField(t *testing.T) {
	s := fixture(t)
	obj := decodeObject(t, s.VersionJSON["v1"])
	obj["unexpected_property"] = true
	_, err := Load(mustJSON(t, obj))
	if !domain.HasCode(err, domain.CodeSchemaUnknown) {
		t.Fatalf("expected E_SCHEMA_UNKNOWN_FIELD, got %v", err)
	}
	if !strings.Contains(err.Error(), "unexpected_property") {
		t.Fatalf("context lost: %v", err)
	}

	obj = decodeObject(t, s.VersionJSON["v1"])
	delete(obj, "table")
	_, err = Load(mustJSON(t, obj))
	if !domain.HasCode(err, domain.CodeSchemaMissing) {
		t.Fatalf("expected E_SCHEMA_MISSING_FIELD, got %v", err)
	}
}

func TestUnitConflictAndOverflow(t *testing.T) {
	s := fixture(t)
	obj := decodeObject(t, s.VersionJSON["v2"])
	obj["units"] = map[string]any{"time": "ms", "size": "byte"}
	_, err := Load(mustJSON(t, obj))
	if !domain.HasCode(err, domain.CodeUnitConflict) {
		t.Fatalf("expected unit conflict, got %v", err)
	}

	obj = decodeObject(t, s.VersionJSON["v2"])
	limits := obj["limits"].(map[string]any)
	limits["decode_buffer_kib"] = float64(1 << 62)
	_, err = Load(mustJSON(t, obj))
	if !domain.HasCode(err, domain.CodeOverflow) {
		t.Fatalf("expected overflow, got %v", err)
	}
}

func TestDuplicateBatchAndUnknownVersion(t *testing.T) {
	s := fixture(t)
	obj := decodeObject(t, s.VersionJSON["v3"])
	table := obj["batch_table"].([]any)
	table[1].(map[string]any)["batch_no"] = table[0].(map[string]any)["batch_no"]
	_, err := Load(mustJSON(t, obj))
	if !domain.HasCode(err, domain.CodeBatchDuplicate) {
		t.Fatalf("expected duplicate batch, got %v", err)
	}
	_, err = Load([]byte(`{"schema_version":"99.0"}`))
	if !domain.HasCode(err, domain.CodeSchemaVersion) {
		t.Fatalf("expected schema version error, got %v", err)
	}
}

func TestCrossVersionDependencyConflict(t *testing.T) {
	s := fixture(t)
	obj := decodeObject(t, s.VersionJSON["v2"])
	families := obj["families"].([]any)
	core := families[0].(map[string]any)
	segments := core["segments"].([]any)
	segments[2].(map[string]any)["deps"] = []any{100}
	blobs := [][]byte{s.VersionJSON["v1"], mustJSON(t, obj), s.VersionJSON["v3"]}
	_, err := LoadAll(blobs...)
	if !domain.HasCode(err, domain.CodeCrossVersion) {
		t.Fatalf("expected cross-version conflict, got %v", err)
	}
}

func TestOptionalInvalidationMarkerAndSignedKiB(t *testing.T) {
	s := fixture(t)
	v1 := decodeObject(t, s.VersionJSON["v1"])
	fam := v1["families"].([]any)[0].(map[string]any)
	fam["batches"].([]any)[0].(map[string]any)["invalidated"] = true
	p, err := Load(mustJSON(t, v1))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Batches[0].Invalidated {
		t.Fatal("v1 invalidated marker was not mapped")
	}

	v3 := decodeObject(t, s.VersionJSON["v3"])
	table := v3["batch_table"].([]any)
	table[0].(map[string]any)["stale"] = true
	p3, err := Load(mustJSON(t, v3))
	if err != nil {
		t.Fatal(err)
	}
	if !p3.Batches[0].Invalidated {
		t.Fatal("v3 stale marker was not mapped")
	}

	v2 := decodeObject(t, s.VersionJSON["v2"])
	mutations := v2["mutations"].([]any)
	mutations[0].(map[string]any)["stored_delta_kib"] = -1
	p2, err := Load(mustJSON(t, v2))
	if err != nil {
		t.Fatal(err)
	}
	if p2.Operations[0].BytesDelta != -1024 {
		t.Fatalf("signed KiB delta not normalized: %d", p2.Operations[0].BytesDelta)
	}
}

func TestVersionSpecificFieldCombinationRejected(t *testing.T) {
	s := fixture(t)
	obj := decodeObject(t, s.VersionJSON["v3"])
	scan := obj["scan_plan"].([]any)[0].(map[string]any)
	scan["window"] = map[string]any{"from": 0, "to": 10}
	_, err := Load(mustJSON(t, obj))
	if !domain.HasCode(err, domain.CodeSchemaUnknown) {
		t.Fatalf("expected version-specific field rejection, got %v", err)
	}
}

func decodeObject(t *testing.T, blob []byte) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(blob, &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
