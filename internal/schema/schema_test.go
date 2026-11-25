package schema_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"featurekernel/internal/engine"
	"featurekernel/internal/gen"
	"featurekernel/internal/kernel"
	"featurekernel/internal/schema"
)

func TestThreeSchemaVersionsMapToSameCanonicalContent(t *testing.T) {
	project, err := gen.BuildProject(gen.DefaultSeed, 1)
	if err != nil {
		t.Fatal(err)
	}
	loaded := map[string]*kernel.Project{}
	for _, version := range []string{"v1", "v2", "v3"} {
		data, err := schema.Encode(project, version)
		if err != nil {
			t.Fatalf("encode %s: %v", version, err)
		}
		parsed, err := schema.Load(data)
		if err != nil {
			t.Fatalf("load %s: %v", version, err)
		}
		loaded[version] = parsed
	}

	v1, v2, v3 := loaded["v1"], loaded["v2"], loaded["v3"]
	if !reflect.DeepEqual(v1.Features, v2.Features) || !reflect.DeepEqual(v1.Features, v3.Features) {
		t.Fatalf("canonical features differ\nv1=%#v\nv2=%#v\nv3=%#v", v1.Features, v2.Features, v3.Features)
	}
	if !reflect.DeepEqual(v1.Records, v2.Records) || !reflect.DeepEqual(v1.Records, v3.Records) {
		t.Fatalf("canonical records differ")
	}
	if !reflect.DeepEqual(v1.Derived, v2.Derived) || !reflect.DeepEqual(v1.Derived, v3.Derived) {
		t.Fatalf("derived definitions differ")
	}
	if !reflect.DeepEqual(v1.PathPolicy, v2.PathPolicy) || !reflect.DeepEqual(v1.Budget, v3.Budget) {
		t.Fatalf("policy/budget differ: %#v %#v %#v", v1.PathPolicy, v2.PathPolicy, v3.PathPolicy)
	}
	if v3.WatermarkPolicy.ExplicitFieldEnabled || !v3.WatermarkPolicy.Degraded {
		t.Fatalf("v3 watermark status should be marked degraded/disabled: %+v", v3.WatermarkPolicy)
	}
	if v1.WatermarkPolicy.ExplicitFieldEnabled != true || v2.WatermarkPolicy.ExplicitFieldEnabled != true {
		t.Fatalf("v1/v2 explicit watermark status wrong")
	}
}

func TestThreeSchemaVersionsProduceSameResults(t *testing.T) {
	project, err := gen.BuildProject(gen.DefaultSeed, 0)
	if err != nil {
		t.Fatal(err)
	}
	truth, err := gen.BuildTruth(project)
	if err != nil {
		t.Fatal(err)
	}
	results := map[string]*kernel.ResultSet{}
	for _, version := range []string{"v1", "v2", "v3"} {
		data, err := schema.Encode(project, version)
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := schema.Load(data)
		if err != nil {
			t.Fatal(err)
		}
		eng, err := engine.New(loaded)
		if err != nil {
			t.Fatal(err)
		}
		result, err := eng.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := kernel.CompareWindowResults(truth.Expected.Windows, result.Windows, loaded.FixedTolerance); err != nil {
			t.Fatalf("%s truth: %v", version, err)
		}
		results[version] = result
	}
	for _, version := range []string{"v2", "v3"} {
		if err := kernel.CompareWindowResults(results["v1"].Windows, results[version].Windows, project.FixedTolerance); err != nil {
			t.Fatalf("%s vs v1: %v", version, err)
		}
		if !reflect.DeepEqual(results["v1"].RecomputedWindows, results[version].RecomputedWindows) {
			t.Fatalf("%s recomputed mismatch", version)
		}
		if !reflect.DeepEqual(results["v1"].Discarded, results[version].Discarded) {
			t.Fatalf("%s discarded mismatch", version)
		}
	}
}

func TestUnknownFieldReturnsStableSchemaCode(t *testing.T) {
	project, err := gen.BuildProject(gen.DefaultSeed, 0)
	if err != nil {
		t.Fatal(err)
	}
	data, err := schema.Encode(project, "v1")
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	raw["unrecognized_future_field"] = "reject"
	modified, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	_, err = schema.Load(modified)
	if code := kernel.CodeOf(err); code != kernel.CodeSchemaUnknownField {
		t.Fatalf("expected %s, got %v", kernel.CodeSchemaUnknownField, err)
	}
}

func TestUnsupportedVersionReturnsStableCode(t *testing.T) {
	_, err := schema.Load([]byte(`{"schema_version":"v9","project_id":"x"}`))
	if code := kernel.CodeOf(err); code != kernel.CodeSchemaUnsupportedVersion {
		t.Fatalf("expected %s, got %v", kernel.CodeSchemaUnsupportedVersion, err)
	}
}

func TestRelativeTimeRestoresEpochAxis(t *testing.T) {
	project, err := gen.BuildProject(gen.DefaultSeed, 0)
	if err != nil {
		t.Fatal(err)
	}
	v2Data, err := schema.Encode(project, "v2")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := schema.Load(v2Data)
	if err != nil {
		t.Fatal(err)
	}
	for i := range project.Records {
		if project.Records[i].Kind != kernel.RecordEvent {
			continue
		}
		if parsed.Records[i].Event.TimeMs != project.Records[i].Event.TimeMs {
			t.Fatalf("time mismatch at seq %d: %d vs %d", project.Records[i].Seq, parsed.Records[i].Event.TimeMs, project.Records[i].Event.TimeMs)
		}
	}
}
