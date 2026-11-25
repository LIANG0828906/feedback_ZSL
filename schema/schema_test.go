package schema_test

import (
	"reflect"
	"testing"

	"alertkernel/errs"
	"alertkernel/model"
	"alertkernel/sample"
	"alertkernel/schema"
)

func TestThreeSchemasMapToSameCore(t *testing.T) {
	bundle, err := sample.Generate()
	if err != nil {
		t.Fatal(err)
	}
	canonical := core(bundle.Project)
	for _, version := range []string{"v1", "v2", "v3"} {
		loaded, err := schema.Load(bundle.Docs[version])
		if err != nil {
			t.Fatalf("load %s: %v", version, err)
		}
		if !reflect.DeepEqual(canonical, core(loaded)) {
			t.Fatalf("%s canonical core differs", version)
		}
	}
	v1, err := schema.Load(bundle.Docs["v1"])
	if err != nil {
		t.Fatal(err)
	}
	v2, err := schema.Load(bundle.Docs["v2"])
	if err != nil {
		t.Fatal(err)
	}
	if len(v1.Silences) != 0 {
		t.Fatalf("v1 must treat missing silence field as no windows, got %d", len(v1.Silences))
	}
	if len(v2.Silences) != 1 {
		t.Fatalf("v2 should load optional silence window, got %d", len(v2.Silences))
	}
}

func TestUnknownSchemaReturnsLocatedBusinessCode(t *testing.T) {
	_, err := schema.Load([]byte(`{"schemaVersion":"v9","projectId":"x"}`))
	if !errs.IsCode(err, errs.ESchemaUnknown) {
		t.Fatalf("expected %s, got %v", errs.ESchemaUnknown, err)
	}
}

func core(p *model.Project) *model.Project {
	c := *p
	c.SourceSchema = ""
	c.Silences = nil
	return &c
}
