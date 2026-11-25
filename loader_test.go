package vpcompose_test

import (
	"encoding/json"
	"testing"

	"vpcompose"
	"vpcompose/sample"
)

func TestUnknownVersionAndMissingFieldAreSchemaErrors(t *testing.T) {
	generated, err := sample.Generate(7)
	if err != nil {
		t.Fatal(err)
	}
	var v3 map[string]any
	if err := json.Unmarshal(generated.Files["project-v3.json"], &v3); err != nil {
		t.Fatal(err)
	}
	v3["schema-version"] = 99
	data, _ := json.Marshal(v3)
	_, err = vpcompose.LoadProject(data)
	if !vpcompose.IsCode(err, vpcompose.CodeSchema) {
		t.Fatalf("unknown version error = %v", err)
	}
	ee := err.(*vpcompose.EngineError)
	if ee.Path != "schema-version" {
		t.Fatalf("unknown version path = %q", ee.Path)
	}

	var v2 map[string]any
	if err := json.Unmarshal(generated.Files["project-v2.json"], &v2); err != nil {
		t.Fatal(err)
	}
	nodes := v2["nodes"].([]any)
	backdrop := nodes[1].(map[string]any)
	delete(backdrop, "opacity")
	data, _ = json.Marshal(v2)
	_, err = vpcompose.LoadProject(data)
	if !vpcompose.IsCode(err, vpcompose.CodeSchema) {
		t.Fatalf("missing field error = %v", err)
	}
	ee = err.(*vpcompose.EngineError)
	if ee.Path != "nodes[1].opacity" {
		t.Fatalf("missing field path = %q", ee.Path)
	}
}

func TestHierarchyCycleReportsParticipatingIDs(t *testing.T) {
	generated, err := sample.Generate(8)
	if err != nil {
		t.Fatal(err)
	}
	var v2 map[string]any
	if err := json.Unmarshal(generated.Files["project-v2.json"], &v2); err != nil {
		t.Fatal(err)
	}
	nodes := v2["nodes"].([]any)
	nodes[0].(map[string]any)["parentId"] = "grp-talent"
	nodes[2].(map[string]any)["parentId"] = "grp-plate"
	data, _ := json.Marshal(v2)
	_, err = vpcompose.LoadProject(data)
	if !vpcompose.IsCode(err, vpcompose.CodeCycle) {
		t.Fatalf("cycle error = %v", err)
	}
	ee := err.(*vpcompose.EngineError)
	if len(ee.LayerIDs) < 2 {
		t.Fatalf("cycle ids = %v", ee.LayerIDs)
	}
}

func TestCorruptSampleIsDecodeError(t *testing.T) {
	_, err := vpcompose.LoadProject([]byte(`{"schema_version":1,`))
	if !vpcompose.IsCode(err, vpcompose.CodeDecode) {
		t.Fatalf("decode error = %v", err)
	}
}
