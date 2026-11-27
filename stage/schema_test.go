package stage

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

func TestGeneratedSchemasLoadWithDefaultsAndLosses(t *testing.T) {
	bundle, err := GenerateSampleBundle(DefaultSampleSeed)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range SupportedSchemas {
		name := map[string]string{SchemaV1: "v1.json", SchemaV2: "v2.json", SchemaV3: "v3.json"}[version]
		p, err := LoadProject(bundle.Files[name])
		if err != nil {
			t.Fatalf("%s load: %v", version, err)
		}
		if p.SourceVersion != version {
			t.Fatalf("source version = %s, want %s", p.SourceVersion, version)
		}
		if len(p.Fixtures) != 4 || len(p.Timeline.Windows) != 5 {
			t.Fatalf("%s counts: fixtures=%d windows=%d", version, len(p.Fixtures), len(p.Timeline.Windows))
		}
		if len(p.Traces) == 0 {
			t.Fatalf("%s has no conversion traces", version)
		}
		if version == SchemaV1 {
			defaults := 0
			for _, tr := range p.Traces {
				if tr.Operation == "apply_default" && tr.Field == "fixtures[0].cone.outer_ddeg" {
					defaults++
					if tr.SourceValue != "<missing>" || tr.DefaultSource == "" {
						t.Fatalf("bad default trace: %+v", tr)
					}
				}
			}
			if defaults != 1 {
				t.Fatalf("v1 default outer traces = %d", defaults)
			}
		}
	}
	var manifest SampleManifest
	if err := json.Unmarshal(bundle.Files["manifest.json"], &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.SchemaLosses[SchemaV1]) == 0 || len(manifest.SchemaLosses[SchemaV2]) == 0 {
		t.Fatalf("expected flattening losses: %+v", manifest.SchemaLosses)
	}
	if len(manifest.SchemaLosses[SchemaV3]) != 0 {
		t.Fatalf("v3 should be lossless: %+v", manifest.SchemaLosses[SchemaV3])
	}
}

func TestSampleGeneratorIsByteDeterministic(t *testing.T) {
	first, err := GenerateSampleBundle(DefaultSampleSeed)
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateSampleBundle(DefaultSampleSeed)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Files) != len(second.Files) {
		t.Fatalf("file count differs: %d vs %d", len(first.Files), len(second.Files))
	}
	for name, data := range first.Files {
		if !bytes.Equal(data, second.Files[name]) {
			t.Fatalf("%s differs across identical seeds", name)
		}
	}
}

func TestSchemaErrorsHavePathAndVersion(t *testing.T) {
	_, err := LoadProject([]byte(`{"schema":"unknown/9","project_id":"x"}`))
	if !errors.Is(err, ErrSchema) {
		t.Fatalf("expected E_SCHEMA, got %v", err)
	}
	de := AsError(err)
	if de.Version != "unknown/9" || de.Field != "schema" {
		t.Fatalf("bad schema error: %+v", de)
	}
	_, err = LoadProject([]byte(`{"schema":"stage.project.v1","project_id":"x","seed":1,"parameter_version":"p","timebase":{"tick_hz":120},"fixtures":[],"cues":[],"timeline":{"duration_tick":1,"windows":[]}}`))
	if !errors.Is(err, ErrSchema) {
		t.Fatalf("expected E_SCHEMA for empty fixtures, got %v", err)
	}
	if AsError(err).Field != "fixtures" {
		t.Fatalf("missing field path: %v", err)
	}
}

func TestExportLossyAndLoadback(t *testing.T) {
	p, err := GenerateProject(DefaultSampleSeed)
	if err != nil {
		t.Fatal(err)
	}
	p.Fixtures[0].RGBPermyriad = Vec3{1, 5001, 9999}
	res, err := ExportProject(p, SchemaV1)
	if !errors.Is(err, ErrLossy) {
		t.Fatalf("expected E_LOSSY, got %v", err)
	}
	if len(res.Data) == 0 || len(res.Losses) < 2 {
		t.Fatalf("lossy export missing data/losses: %+v", res)
	}
	back, err := LoadProject(res.Data)
	if err != nil {
		t.Fatalf("lossy artifact does not load: %v", err)
	}
	if back.Fixtures[0].RGBPermyriad == p.Fixtures[0].RGBPermyriad {
		t.Fatal("expected quantized color on v1 loadback")
	}
}

func TestV1FixtureConversionsRoundTrip(t *testing.T) {
	bundle, err := GenerateSampleBundle(DefaultSampleSeed)
	if err != nil {
		t.Fatal(err)
	}
	v1, err := LoadProject(bundle.Files["v1.json"])
	if err != nil {
		t.Fatal(err)
	}
	v3, err := LoadProject(bundle.Files["v3.json"])
	if err != nil {
		t.Fatal(err)
	}
	for i := range v1.Fixtures {
		a, b := v1.Fixtures[i], v3.Fixtures[i]
		if a.PositionUM != b.PositionUM || a.PanMicroDeg != b.PanMicroDeg || a.TiltMicroDeg != b.TiltMicroDeg ||
			a.InnerHalfMicroDeg != b.InnerHalfMicroDeg || a.OuterHalfMicroDeg != b.OuterHalfMicroDeg ||
			a.CCTKelvin != b.CCTKelvin || a.LevelPermyriad != b.LevelPermyriad || a.RGBPermyriad != b.RGBPermyriad {
			t.Fatalf("v1/v3 fixture differs at %d: %+v vs %+v", i, a, b)
		}
	}
}

func TestV1MissingCCTUsesDeclaredDefault(t *testing.T) {
	bundle, err := GenerateSampleBundle(DefaultSampleSeed)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(bundle.Files["v1.json"], &doc); err != nil {
		t.Fatal(err)
	}
	fixtures := doc["fixtures"].([]any)
	delete(fixtures[0].(map[string]any), "cct_permille")
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	p, err := LoadProject(data)
	if err != nil {
		t.Fatal(err)
	}
	if p.Fixtures[0].CCTKelvin != 5600 {
		t.Fatalf("default CCT = %d", p.Fixtures[0].CCTKelvin)
	}
	found := false
	for _, tr := range p.Traces {
		if tr.Field == "fixtures[0].cct_permille" && tr.Operation == "apply_default" {
			found = true
			if tr.SourceValue != "<missing>" || tr.DefaultSource == "" {
				t.Fatalf("bad CCT default trace: %+v", tr)
			}
		}
	}
	if !found {
		t.Fatal("missing CCT default trace")
	}
}

func TestExportCarriesReadbackConversionTraces(t *testing.T) {
	p, err := GenerateProject(DefaultSampleSeed)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ExportProject(p, SchemaV2)
	if !errors.Is(err, ErrLossy) {
		t.Fatalf("expected grouped-array E_LOSSY, got %v", err)
	}
	if len(res.ConversionTraces) == 0 {
		t.Fatal("export did not expose loadback conversion traces")
	}
	foundAngle := false
	for _, tr := range res.ConversionTraces {
		if tr.Operation == "urad_to_microdeg" {
			foundAngle = true
			break
		}
	}
	if !foundAngle {
		t.Fatal("expected angle readback trace")
	}
}

func TestHighLowHighRoundTripWithinDeclaredResiduals(t *testing.T) {
	p, err := GenerateProject(DefaultSampleSeed)
	if err != nil {
		t.Fatal(err)
	}
	p.Fixtures[0].PanMicroDeg = 12_345_678
	p.Fixtures[0].LevelPermyriad = 5003
	p.Fixtures[0].RGBPermyriad = Vec3{5003, 9999, 1}
	for i := range p.Cues {
		if p.Cues[i].ID == "cue-fade-f1" {
			p.Cues[i].StartMS = 123
			p.Cues[i].EndMS = 277
		}
	}
	low, err := ExportProject(p, SchemaV1)
	if !errors.Is(err, ErrLossy) {
		t.Fatalf("expected E_LOSSY, got %v", err)
	}
	lowProject, err := LoadProject(low.Data)
	if err != nil {
		t.Fatal(err)
	}
	high, err := ExportProject(lowProject, SchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	back, err := LoadProject(high.Data)
	if err != nil {
		t.Fatal(err)
	}
	if abs64(back.Fixtures[0].PanMicroDeg-12_345_678) > 50_000 {
		t.Fatalf("pan residual %d", back.Fixtures[0].PanMicroDeg-12_345_678)
	}
	if abs64(back.Fixtures[0].LevelPermyriad-5003) > 5 {
		t.Fatalf("level residual %d", back.Fixtures[0].LevelPermyriad-5003)
	}
	for ch := range back.Fixtures[0].RGBPermyriad {
		if abs64(back.Fixtures[0].RGBPermyriad[ch]-p.Fixtures[0].RGBPermyriad[ch]) > 5 {
			t.Fatalf("rgb residual channel %d: %d", ch, back.Fixtures[0].RGBPermyriad[ch]-p.Fixtures[0].RGBPermyriad[ch])
		}
	}
	for i := range back.Cues {
		if back.Cues[i].ID == "cue-fade-f1" {
			if abs64(back.Cues[i].StartMS-123) > 5 || abs64(back.Cues[i].EndMS-277) > 5 {
				t.Fatalf("fade time residual start=%d end=%d", back.Cues[i].StartMS-123, back.Cues[i].EndMS-277)
			}
		}
	}
}
