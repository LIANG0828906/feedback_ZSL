package loudness

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestLoaderNormalizesThreeVersions(t *testing.T) {
	cases, err := GenerateAll(20261127)
	if err != nil {
		t.Fatal(err)
	}
	for _, generated := range cases {
		loaded, err := Load(generated.Wire)
		if err != nil {
			t.Fatalf("%s load: %v", generated.Version, err)
		}
		if loaded.Provenance.OriginalVersion != generated.Version || loaded.Provenance.CurrentVersion != generated.Version {
			t.Fatalf("%s source provenance not preserved", generated.Version)
		}
		if len(loaded.Provenance.Traces) == 0 {
			t.Fatalf("%s has no field conversion traces", generated.Version)
		}
		if len(loaded.Clips) != 7 || loaded.SampleRate != 8000 || loaded.FrameLength != 256 {
			t.Fatalf("%s normalized header: clips=%d rate=%d frame=%d", generated.Version, len(loaded.Clips), loaded.SampleRate, loaded.FrameLength)
		}
		switch generated.Version {
		case SchemaV1:
			if len(loaded.Scenes) != 0 {
				t.Fatal("v1 unexpectedly has scene grouping")
			}
			for _, clip := range loaded.Clips {
				if clip.CalibrationMarker != nil {
					t.Fatal("v1 unexpectedly has a calibration marker")
				}
			}
		case SchemaV2:
			if len(loaded.Scenes) != 4 {
				t.Fatalf("v2 scenes = %d, want 4", len(loaded.Scenes))
			}
			if loaded.Clips[2].CalibrationMarker != nil {
				t.Fatal("optional v2 marker should be absent on clip 2")
			}
		case SchemaV3:
			if loaded.Clips[0].CalibrationMarker == nil {
				t.Fatal("v3 marker missing on clip 0")
			}
		}
	}
}

func TestSchemaErrorsIncludePathAndSource(t *testing.T) {
	_, err := Load([]byte(`{"schema":"game-audio/v9"}`))
	if !HasCode(err, ESchema) {
		t.Fatalf("unknown version: %v", err)
	}
	domainErr, _ := AsDomainError(err)
	if domainErr.Path != "$.schema" || domainErr.SourceVersion != Version("game-audio/v9") {
		t.Fatalf("unknown-version diagnostics: %+v", domainErr)
	}

	_, err = Load([]byte(`{"schema":"game-audio/v1"}`))
	if !HasCode(err, ESchema) {
		t.Fatalf("missing field: %v", err)
	}
	domainErr, _ = AsDomainError(err)
	if domainErr.Path != "$.project_id" || domainErr.SourceVersion != SchemaV1 {
		t.Fatalf("missing-field diagnostics: %+v", domainErr)
	}

	_, err = Load([]byte(`{"schema":`))
	if !HasCode(err, ESchema) {
		t.Fatalf("malformed JSON: %v", err)
	}
}

func TestUpgradeDefaultsAndDowngradeLossReport(t *testing.T) {
	generatedV1, err := GenerateCase(20261127, SchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	upgraded, report, err := Upgrade(generatedV1.Project, SchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Defaults) < 2 {
		t.Fatalf("expected default traces, got %d", len(report.Defaults))
	}
	foundScene := false
	foundMarker := false
	for _, trace := range report.Defaults {
		if trace.Kind != TraceDefault || trace.OldValue != "<absent>" || trace.DefaultSource == "" {
			t.Fatalf("bad default trace: %+v", trace)
		}
		foundScene = foundScene || strings.HasSuffix(trace.InternalPath, ".scene")
		foundMarker = foundMarker || strings.HasSuffix(trace.InternalPath, ".calibrationMarker")
	}
	if !foundScene || !foundMarker {
		t.Fatalf("missing default kinds: scene=%t marker=%t", foundScene, foundMarker)
	}
	exported, err := Export(upgraded, SchemaV3, RoundHalfUp)
	if err != nil {
		t.Fatalf("upgraded v3 export: %v", err)
	}
	readback, err := Load(exported.Data)
	if err != nil {
		t.Fatalf("upgraded v3 readback: %v", err)
	}
	if len(readback.Clips) != len(upgraded.Clips) || readback.Scenes[0].Name != "default" {
		t.Fatal("upgraded export did not preserve expressible content")
	}

	generatedV3, err := GenerateCase(20261127, SchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	downgraded, err := Export(generatedV3.Project, SchemaV1, RoundHalfUp)
	if !HasCode(err, ELossy) {
		t.Fatalf("expected E_LOSSY, got %v", err)
	}
	if len(downgraded.Losses) == 0 || downgraded.Data == nil {
		t.Fatal("lossy export must include losses and readable data")
	}
	hasMarkerLoss := false
	for _, loss := range downgraded.Losses {
		hasMarkerLoss = hasMarkerLoss || strings.Contains(loss.Path, "calibrationMarker")
	}
	if !hasMarkerLoss {
		t.Fatal("marker loss not reported")
	}
	flattened, err := Load(downgraded.Data)
	if err != nil {
		t.Fatalf("lossy readback: %v", err)
	}
	if len(flattened.Clips) != len(generatedV3.Project.Clips) {
		t.Fatal("lossy readback clip count changed")
	}
	for i := range flattened.Clips {
		got, want := flattened.Clips[i], generatedV3.Project.Clips[i]
		if got.ID != want.ID || got.StartSample != want.StartSample || got.EndSample != want.EndSample ||
			got.Category != want.Category || got.GainQ12 != want.GainQ12 || got.AmplitudeQ12 != want.AmplitudeQ12 {
			t.Fatalf("clip %d expressible content not preserved", i)
		}
	}
}

func TestV2MissingNestedFieldIsSchemaError(t *testing.T) {
	generated, err := GenerateCase(20261127, SchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(generated.Wire, &object); err != nil {
		t.Fatal(err)
	}
	scenes := object["scenes"].([]any)
	scene := scenes[0].(map[string]any)
	clips := scene["clips"].([]any)
	clip := clips[0].(map[string]any)
	delete(clip, "start_ms")
	mutated, _ := json.Marshal(object)
	if bytes.Equal(mutated, generated.Wire) {
		t.Fatal("test mutation did not change wire")
	}
	_, err = Load(mutated)
	if !HasCode(err, ESchema) {
		t.Fatalf("nested missing field: %v", err)
	}
	domainErr, _ := AsDomainError(err)
	if domainErr.Path != "$.scenes[0].clips[0].start_ms" || domainErr.SourceVersion != SchemaV2 {
		t.Fatalf("nested path diagnostics: %+v", domainErr)
	}
}
