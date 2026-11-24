package format_test

import (
	"encoding/json"
	"strings"
	"testing"

	"economycore/format"
	"economycore/model"
	"economycore/sample"
)

func TestUnknownVersionAndMissingFieldPaths(t *testing.T) {
	_, err := format.Load([]byte(`{"schema_version":"economy-v99"}`))
	if !model.IsCode(err, model.EVer) {
		t.Fatalf("expected E_VER, got %v", err)
	}
	typed, _ := model.AsError(err)
	if typed.Path != "/schema_version" {
		t.Fatalf("E_VER path=%q", typed.Path)
	}

	doc := map[string]any{
		"schema_version":     "economy-v1",
		"project_id":         "p",
		"project_name":       "P",
		"conversion_version": "conv-1.0.0",
		"timing": map[string]any{
			"unit":                  "tick",
			"ticks_per_cycle":       4,
			"milliseconds_per_tick": 250,
		},
		"window_cycles": 4,
		"workforce":     1,
		"resources":     []any{},
		"initial_stock": map[string]any{},
		"capacity":      map[string]any{},
		"nodes":         []any{},
		"intents":       []any{},
	}
	data, _ := json.Marshal(doc)
	_, err = format.Load(data)
	if !model.IsCode(err, model.ESchema) {
		t.Fatalf("expected E_SCHEMA, got %v", err)
	}
	typed, _ = model.AsError(err)
	if typed.Path != "/window_count" {
		t.Fatalf("E_SCHEMA path=%q", typed.Path)
	}
}

func TestFixedConversionOverflowCarriesNodeField(t *testing.T) {
	m := sample.Scenario(1)
	result, err := format.Export(m, model.SchemaV1, format.ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	doc := string(result.Data)
	doc = strings.Replace(doc, `"rate_permille": 1000`, `"rate_permille": 9223372036854775807`, 1)
	_, err = format.Load([]byte(doc))
	if !model.IsCode(err, model.EOvf) {
		t.Fatalf("expected E_OVF, got %v", err)
	}
	typed, _ := model.AsError(err)
	if typed.NodeID != "forest" || typed.Field != "rate" {
		t.Fatalf("missing node/field context: %+v", typed)
	}
}

func TestRoundingLossIsExplicit(t *testing.T) {
	m := sample.Scenario(2)
	m.Nodes[0].RateExpr = model.ConstExpr(10001)
	_, err := format.Export(m, model.SchemaV1, format.ExportOptions{})
	if !model.IsCode(err, model.ELossy) {
		t.Fatalf("expected E_LOSSY, got %v", err)
	}
	result, err := format.Export(m, model.SchemaV1, format.ExportOptions{AllowLossy: true, RoundMode: "floor"})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := format.Load(result.Data)
	if err != nil {
		t.Fatal(err)
	}
	rate, _, _ := loaded.Nodes[0].ConstantRate()
	if rate != 10000 {
		t.Fatalf("floor conversion rate=%d, want 10000", rate)
	}
}
