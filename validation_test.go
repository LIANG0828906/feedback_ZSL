package audiofp_test

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"audiofp"
	"audiofp/samplegen"
)

func TestGeneratedMaterialValid(t *testing.T) {
	m := samplegen.GenerateDefault()
	if err := m.Validate(); err != nil {
		t.Fatalf("generated material must be valid: %v", err)
	}
	if m.ContentHash() == "" || m.PCM.SHA256 != m.ContentHash() {
		t.Fatalf("generator must set stable content hash")
	}
}

func TestLoadMissingFieldIsSchema(t *testing.T) {
	dir := t.TempDir()
	pcmPath := filepath.Join(dir, "bad.pcm.json")
	manifestPath := filepath.Join(dir, "bad.manifest.json")
	if err := os.WriteFile(pcmPath, []byte(`{
		"schema_version":"v1","id":"x","channels":1,"sample_format":"float64-json",
		"sample_count":1,"samples":[0.1]
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte(`{"schema_version":"v1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := audiofp.LoadMaterial(pcmPath, manifestPath)
	if !audiofp.HasCode(err, audiofp.CodeSchema) {
		t.Fatalf("expected E_SCHEMA, got %v", err)
	}
	if de, ok := audiofp.AsError(err); !ok || de.Field != "sample_rate" {
		t.Fatalf("expected field sample_rate, got %#v", de)
	}
}

func TestZeroSampleCountIsRange(t *testing.T) {
	m := samplegen.GenerateDefault()
	m.PCM.SampleCount = 0
	m.PCM.Samples = []float64{}
	m.Manifest.Events = nil
	err := m.Validate()
	if !audiofp.HasCode(err, audiofp.CodeRange) {
		t.Fatalf("expected E_RANGE, got %v", err)
	}
	if de, ok := audiofp.AsError(err); !ok || de.Field != "sample_count" {
		t.Fatalf("expected sample_count field, got %#v", de)
	}
}

func TestEventOutOfRangeNamesEventNumber(t *testing.T) {
	m := samplegen.GenerateDefault()
	m.Manifest.Events[1].EndSample = m.PCM.SampleCount + 4096
	err := m.Validate()
	if !audiofp.HasCode(err, audiofp.CodeRange) {
		t.Fatalf("expected E_RANGE, got %v", err)
	}
	de, ok := audiofp.AsError(err)
	if !ok || de.EventNo != 2 {
		t.Fatalf("expected event #2, got %#v", err)
	}
}

func TestOverlappingEventsRejected(t *testing.T) {
	m := samplegen.GenerateDefault()
	m.Manifest.Events[0].EndSample = m.Manifest.Events[1].StartSample + 10
	err := m.Validate()
	if !audiofp.HasCode(err, audiofp.CodeRange) {
		t.Fatalf("expected overlap E_RANGE, got %v", err)
	}
	de, _ := audiofp.AsError(err)
	if de == nil || de.EventNo != 2 {
		t.Fatalf("expected later event #2 in error, got %#v", de)
	}
}

func TestUnknownCategoryRejected(t *testing.T) {
	m := samplegen.GenerateDefault()
	m.Manifest.Events[0].Category = "does-not-exist"
	err := m.Validate()
	if !audiofp.HasCode(err, audiofp.CodeSchema) {
		t.Fatalf("expected E_SCHEMA for category enum, got %v", err)
	}
}

func TestNonFiniteToneRejected(t *testing.T) {
	m := samplegen.GenerateDefault()
	m.Manifest.Events[0].Tone.FrequencyHz = math.NaN()
	err := m.Validate()
	if !audiofp.HasCode(err, audiofp.CodeSchema) {
		t.Fatalf("expected E_SCHEMA for non-finite tone, got %v", err)
	}
}

func TestParameterBoundsNameFields(t *testing.T) {
	cases := []struct {
		name   string
		params audiofp.Params
		field  string
	}{
		{"frame length", audiofp.Params{FrameLength: 16, FrameShift: 256, BandCount: 16}, "frame_length"},
		{"frame shift", audiofp.Params{FrameLength: 1024, FrameShift: 1, BandCount: 16}, "frame_shift"},
		{"band count", audiofp.Params{FrameLength: 256, FrameShift: 128, BandCount: 999}, "band_count"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.params.Validate()
			if !audiofp.HasCode(err, audiofp.CodePara) {
				t.Fatalf("expected E_PARA, got %v", err)
			}
			de, _ := audiofp.AsError(err)
			if de.Field != tc.field {
				t.Fatalf("expected field %s, got %s", tc.field, de.Field)
			}
		})
	}
}
