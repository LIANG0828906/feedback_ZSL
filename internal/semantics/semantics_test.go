package semantics_test

import (
	"encoding/json"
	"testing"

	"audioforge/internal/errs"
	"audioforge/internal/generator"
	"audioforge/internal/model"
	"audioforge/internal/semantics"
)

func TestThreeBranchesNormalizeEquivalentlyAndStayTraceable(t *testing.T) {
	bundle, err := generator.Generate(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	streams, err := bundle.LoadStreams()
	if err != nil {
		t.Fatal(err)
	}
	if len(streams) != 3 {
		t.Fatalf("got %d streams", len(streams))
	}
	if err := semantics.CompareStreams(streams); err != nil {
		t.Fatal(err)
	}
	for _, stream := range streams {
		if len(stream.Events) != len(bundle.CanonicalEvents) {
			t.Fatalf("%s has %d events", stream.Spec.Name, len(stream.Events))
		}
		for _, event := range stream.Events {
			if event.SourceBranch != stream.Spec.Name {
				t.Fatalf("event %s lost source branch", event.ID)
			}
			for _, field := range []string{"start", "intensity", "duration", "types", "routes", "gate"} {
				if event.SourcePaths[field] == "" {
					t.Fatalf("event %s has no source path for %s", event.ID, field)
				}
			}
		}
	}
}

func TestBranchRoundTripAcrossSemanticLayouts(t *testing.T) {
	bundle, err := generator.Generate(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	streams, err := bundle.LoadStreams()
	if err != nil {
		t.Fatal(err)
	}
	for _, targetSpec := range generator.DefaultSpecs() {
		encoded, err := semantics.EncodeStreamAtRate(targetSpec, streams[0].Events, generator.SampleRate)
		if err != nil {
			t.Fatal(err)
		}
		roundTripped, err := semantics.DecodeWithSampleRate(encoded, "roundtrip", generator.SampleRate, bundle.Registry)
		if err != nil {
			t.Fatal(err)
		}
		if err := semantics.CompareStreams([]model.Stream{streams[0], roundTripped}); err != nil {
			t.Fatalf("round trip through %s: %v", targetSpec.Name, err)
		}
	}
}

func TestUnknownBranchAndMissingFieldReturnVersionError(t *testing.T) {
	registry, err := semantics.NewRegistry(generator.DefaultSpecs()...)
	if err != nil {
		t.Fatal(err)
	}
	unknown := []byte(`{"branch":"unknown-console","events":[]}`)
	_, err = semantics.Decode(unknown, "unknown.json", registry)
	if !errs.IsCode(err, errs.EVersion) {
		t.Fatalf("unknown branch error = %v", err)
	}
	coded := err.(*errs.Error)
	if coded.Branch != "unknown-console" || coded.Field != "branch" {
		t.Fatalf("unexpected error context: %+v", coded)
	}

	var file map[string]any
	if err := json.Unmarshal([]byte(mustStream(t, "arena-tick")), &file); err != nil {
		t.Fatal(err)
	}
	events := file["events"].([]any)
	first := events[0].(map[string]any)
	delete(first, "value")
	missing, _ := json.Marshal(file)
	_, err = semantics.Decode(missing, "missing.json", registry)
	if !errs.IsCode(err, errs.EVersion) {
		t.Fatalf("missing field error = %v", err)
	}
	coded = err.(*errs.Error)
	if coded.Branch != "arena-tick" || coded.Field != "events[0].value" {
		t.Fatalf("unexpected missing field context: %+v", coded)
	}
}

func TestTimeConversionOverflowIsOverflowError(t *testing.T) {
	registry, err := semantics.NewRegistry(generator.DefaultSpecs()...)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{
		"branch":"arena-tick",
		"events":[{
			"id":"overflow","time":9223372036854775807,"priority":1,
			"blocking":false,"conditional":false,"gate":false,
			"value":10,"duration_samples":32,"sequence":["x"],"route":[0]
		}]
	}`)
	_, err = semantics.DecodeWithSampleRate(payload, "overflow.json", generator.SampleRate, registry)
	if !errs.IsCode(err, errs.EOver) {
		t.Fatalf("overflow error = %v", err)
	}
	coded := err.(*errs.Error)
	if coded.EventID != "overflow" || coded.Field != "time" {
		t.Fatalf("unexpected overflow context: %+v", coded)
	}
}

func mustStream(t *testing.T, branch string) string {
	t.Helper()
	bundle, err := generator.Generate(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	for _, stream := range bundle.Streams {
		if stream.Spec.Name == branch {
			return string(stream.Data)
		}
	}
	t.Fatalf("branch %s not found", branch)
	return ""
}
