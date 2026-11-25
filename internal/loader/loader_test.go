package loader_test

import (
	"encoding/json"
	"testing"

	"battlekernel/internal/engine"
	"battlekernel/internal/errs"
	"battlekernel/internal/loader"
	"battlekernel/internal/sample"
)

func TestThreeFormatsMapToSameSemanticProject(t *testing.T) {
	suite, err := sample.GenerateSuite(sample.DefaultSeed, 1)
	if err != nil {
		t.Fatal(err)
	}
	s := suite.Samples[0]
	set, err := loader.LoadSet(s.Documents[1], s.Documents[2], s.Documents[3], s.TruthJSON)
	if err != nil {
		t.Fatal(err)
	}
	if set.V1.SourceFormat != loader.FormatTickV1 || set.V2.SourceFormat != loader.FormatChronoV2 || set.V3.SourceFormat != loader.FormatCompactV3 {
		t.Fatalf("source identifiers not retained: %q %q %q", set.V1.SourceFormat, set.V2.SourceFormat, set.V3.SourceFormat)
	}
	if set.V2.Units[0].BaseWeight != 8 || set.V2.Units[2].InitialCooldown != 1 {
		t.Fatalf("swapped integer fields were not mapped: %+v", set.V2.Units)
	}
	if len(set.V1.SkillSequence) != 24 || len(set.V1.ActorSequence) != 24 {
		t.Fatalf("sequence lengths=%d/%d", len(set.V1.SkillSequence), len(set.V1.ActorSequence))
	}
}

func TestRawSwappedFieldSemanticsAreExplicit(t *testing.T) {
	suite, _ := sample.GenerateSuite(sample.DefaultSeed, 1)
	s := suite.Samples[0]

	var w1 map[string]any
	if err := json.Unmarshal(s.Documents[1], &w1); err != nil {
		t.Fatal(err)
	}
	u1 := w1["units"].([]any)[0].(map[string]any)
	if u1["priority"].(float64) != 8 || u1["recast"].(float64) != 0 {
		t.Fatalf("format 1 integer semantics changed: %v/%v", u1["priority"], u1["recast"])
	}
	if alpha := w1["lists"].(map[string]any)["alpha"].([]any)[0].(string); alpha[0] != 'S' {
		t.Fatalf("format 1 alpha should be skill sequence: %q", alpha)
	}

	var w2 map[string]any
	if err := json.Unmarshal(s.Documents[2], &w2); err != nil {
		t.Fatal(err)
	}
	unit := w2["unitRecords"].([]any)[0].(map[string]any)
	if unit["priority"].(float64) != 0 || unit["recast"].(float64) != 8 {
		t.Fatalf("format 2 swap not present in raw document: priority=%v recast=%v", unit["priority"], unit["recast"])
	}
	alpha := w2["sequenceBox"].(map[string]any)["alpha"].([]any)[0].(string)
	if alpha[0] != 'A' {
		t.Fatalf("format 2 alpha should be actor queue sequence: %q", alpha)
	}
}

func TestUnknownFormatReturnsESchema(t *testing.T) {
	_, err := loader.LoadDocument([]byte(`{"format":404}`))
	if !errs.IsCode(err, errs.ESchema) {
		t.Fatalf("expected E_SCHEMA, got %v", err)
	}
}

func TestMissingRequiredFieldReturnsFieldPath(t *testing.T) {
	suite, _ := sample.GenerateSuite(sample.DefaultSeed, 1)
	var raw map[string]any
	if err := json.Unmarshal(suite.Samples[0].Documents[1], &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "tick_ms")
	data, _ := json.Marshal(raw)
	_, err := loader.LoadDocument(data)
	if !errs.IsCode(err, errs.ESchema) {
		t.Fatalf("expected E_SCHEMA, got %v", err)
	}
	if fe, ok := err.(*errs.Error); !ok || fe.Field != "tick_ms" {
		t.Fatalf("expected tick_ms field path, got %#v", err)
	}
}

func TestChronoTickChronoRoundTripPreservesOrder(t *testing.T) {
	suite, _ := sample.GenerateSuite(sample.DefaultSeed, 1)
	original := suite.Samples[0]

	high, err := loader.LoadDocument(original.Documents[2])
	if err != nil {
		t.Fatal(err)
	}
	lowData, err := loader.Encode(high, 1)
	if err != nil {
		t.Fatal(err)
	}
	low, err := loader.LoadDocument(lowData)
	if err != nil {
		t.Fatal(err)
	}
	highAgainData, err := loader.Encode(low, 2)
	if err != nil {
		t.Fatal(err)
	}
	highAgain, err := loader.LoadDocument(highAgainData)
	if err != nil {
		t.Fatal(err)
	}

	before, _, err := engine.SimulateAll(high)
	if err != nil {
		t.Fatal(err)
	}
	after, _, err := engine.SimulateAll(highAgain)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("round count %d != %d", len(before), len(after))
	}
	for i := range before {
		if before[i].ContentHash != after[i].ContentHash {
			t.Fatalf("round %d changed after high-low-high round trip: %s != %s", before[i].Round, before[i].ContentHash, after[i].ContentHash)
		}
	}
}
