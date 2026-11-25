package schema_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"mvccworkbench/internal/domain"
	"mvccworkbench/internal/generator"
	"mvccworkbench/internal/schema"
)

func TestThreeSchemaEditionsRoundTripToSameCanonicalProject(t *testing.T) {
	suite, err := generator.DefaultSuite(424242)
	if err != nil {
		t.Fatal(err)
	}
	for _, batchID := range suite.Order {
		original := suite.Batches[batchID].Project
		for _, version := range []string{domain.SchemaV1, domain.SchemaV2, domain.SchemaV3} {
			t.Run(batchID+"/"+version, func(t *testing.T) {
				data, err := schema.Encode(version, original)
				if err != nil {
					t.Fatal(err)
				}
				loaded, detected, err := schema.LoadDocument(data)
				if err != nil {
					t.Fatalf("load: %v", err)
				}
				if detected != version {
					t.Fatalf("detected=%s want %s", detected, version)
				}
				loaded.SourceSchema = ""
				if !reflect.DeepEqual(loaded, original) {
					t.Fatalf("canonical project mismatch\nloaded=%s\noriginal=%s", mustJSON(loaded), mustJSON(original))
				}
			})
		}
	}
}

func TestUnknownSchemaReturnsStableCode(t *testing.T) {
	_, _, err := schema.LoadDocument([]byte(`{"schema_version":"mvcc.project/v9"}`))
	if domain.CodeOf(err) != domain.ESchemaUnknown {
		t.Fatalf("code=%s err=%v", domain.CodeOf(err), err)
	}
}

func mustJSON(v any) string {
	data, _ := json.MarshalIndent(v, "", "  ")
	return string(data)
}
