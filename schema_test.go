package macrokit

import (
	"encoding/json"
	"strings"
	"testing"
)

func exportDocMap(t *testing.T, p *Project, format int) map[string]any {
	t.Helper()
	data, err := ExportProject(p, format)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func loadMutatedDoc(t *testing.T, doc map[string]any) error {
	t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	_, err = LoadProjectBytes(data, "mutated.json")
	return err
}

func TestUnknownFormatReturnsSchemaPath(t *testing.T) {
	p := GenerateSamples(FixedSampleSeed).Projects[0].Project
	doc := exportDocMap(t, p, FormatFlatFrameV1)
	doc["format"] = 99
	err := loadMutatedDoc(t, doc)
	if !HasCode(err, ESchema) {
		t.Fatalf("expected E_SCHEMA, got %v", err)
	}
	domainErr, _ := AsError(err)
	if domainErr.Path != "/format" {
		t.Fatalf("path = %q, want /format", domainErr.Path)
	}
}

func TestMissingRequiredFieldsReturnSchemaPath(t *testing.T) {
	p := GenerateSamples(FixedSampleSeed).Projects[0].Project

	doc := exportDocMap(t, p, FormatFlatFrameV1)
	delete(doc, "project_id")
	err := loadMutatedDoc(t, doc)
	assertSchemaPath(t, err, "/project_id")

	doc = exportDocMap(t, p, FormatNestedMSV2)
	delete(doc, "projectId")
	err = loadMutatedDoc(t, doc)
	assertSchemaPath(t, err, "/projectId")

	doc = exportDocMap(t, p, FormatFlatFrameV1)
	branches := doc["branches"].([]any)
	first := branches[0].(map[string]any)
	delete(first, "id")
	err = loadMutatedDoc(t, doc)
	assertSchemaPath(t, err, "/branches/0/id")

	_, err = LoadProjectBytes([]byte("{not-json"), "bad.json")
	if !HasCode(err, ESchema) {
		t.Fatalf("invalid JSON should be E_SCHEMA, got %v", err)
	}
}

func TestFlatAndNestedReferenceBounds(t *testing.T) {
	p := GenerateSamples(FixedSampleSeed).Projects[0].Project

	doc := exportDocMap(t, p, FormatFlatFrameV1)
	branches := doc["branches"].([]any)
	first := branches[0].(map[string]any)
	first["when_ref"] = "missing-root"
	err := loadMutatedDoc(t, doc)
	if !HasCode(err, ERef) {
		t.Fatalf("expected E_REF for missing when_ref, got %v", err)
	}
	domainErr, _ := AsError(err)
	if domainErr.Ref != "missing-root" || !strings.Contains(domainErr.Path, "when_ref") {
		t.Fatalf("unexpected ref error: %+v", domainErr)
	}

	doc = exportDocMap(t, p, FormatFlatFrameV1)
	rows := doc["condition_table"].([]any)
	for _, rowAny := range rows {
		row := rowAny.(map[string]any)
		if refs, ok := row["refs"].([]any); ok && len(refs) > 0 {
			refs[0] = "missing-node"
			break
		}
	}
	err = loadMutatedDoc(t, doc)
	if !HasCode(err, ERef) {
		t.Fatalf("expected E_REF for missing table node, got %v", err)
	}
	domainErr, _ = AsError(err)
	if domainErr.Ref != "missing-node" {
		t.Fatalf("ref = %q, want missing-node", domainErr.Ref)
	}

	doc = exportDocMap(t, p, FormatNestedMSV2)
	branches = doc["branches"].([]any)
	first = branches[0].(map[string]any)
	first["when"] = map[string]any{"op": "branch", "branchId": "ghost_branch"}
	err = loadMutatedDoc(t, doc)
	if !HasCode(err, ERef) {
		t.Fatalf("expected nested branch E_REF, got %v", err)
	}
	domainErr, _ = AsError(err)
	if domainErr.Ref != "ghost_branch" || !strings.Contains(domainErr.Path, "branchId") && !strings.Contains(domainErr.Path, "branch_id") {
		t.Fatalf("unexpected nested ref error: %+v", domainErr)
	}
}

func assertSchemaPath(t *testing.T, err error, path string) {
	t.Helper()
	if !HasCode(err, ESchema) {
		t.Fatalf("expected E_SCHEMA, got %v", err)
	}
	domainErr, _ := AsError(err)
	if domainErr.Path != path {
		t.Fatalf("path = %q, want %q (err=%v)", domainErr.Path, path, err)
	}
}
