package schema_test

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"maskqc/model"
	"maskqc/sample"
	"maskqc/schema"
)

func TestLoadThreeVersionsAndDefaultTraces(t *testing.T) {
	bundle, err := sample.Generate(9001, []int{96})
	if err != nil {
		t.Fatal(err)
	}
	generated := bundle.Projects[0]
	var canonical []model.Stroke
	for _, version := range []model.SchemaVersion{model.SchemaV1, model.SchemaV2, model.SchemaV3} {
		project, report, err := schema.Load(generated.Artifacts[version])
		if err != nil {
			t.Fatalf("load %s: %v", version, err)
		}
		if project.Version != version {
			t.Fatalf("source version %s not preserved", version)
		}
		if len(project.Strokes) != 4 || len(project.Truth) != 3 {
			t.Fatalf("%s loaded wrong object counts", version)
		}
		defaultFills := 0
		for _, trace := range report.Traces {
			if trace.DefaultSource != "" {
				defaultFills++
				if trace.Field == "" || trace.OldValue == "" || trace.DefaultValue == "" {
					t.Fatalf("%s default trace is incomplete: %+v", version, trace)
				}
			}
		}
		switch version {
		case model.SchemaV1:
			if defaultFills != 12 {
				t.Fatalf("v1 expected 12 default fills, got %d", defaultFills)
			}
		case model.SchemaV2:
			if defaultFills != 4 {
				t.Fatalf("v2 expected 4 default direction fills, got %d", defaultFills)
			}
		case model.SchemaV3:
			if defaultFills != 0 {
				t.Fatalf("v3 expected no default fills, got %d", defaultFills)
			}
		}
		if canonical == nil {
			canonical = append([]model.Stroke(nil), project.Strokes...)
		} else {
			for index := range canonical {
				if project.Strokes[index].X0 != canonical[index].X0 ||
					project.Strokes[index].Y0 != canonical[index].Y0 ||
					project.Strokes[index].X1 != canonical[index].X1 ||
					project.Strokes[index].Y1 != canonical[index].Y1 ||
					project.Strokes[index].Seq != canonical[index].Seq ||
					project.Strokes[index].Weight != canonical[index].Weight {
					t.Fatalf("%s canonical geometry differs at stroke %d", version, index)
				}
			}
		}
		if len(generated.Lossy[version]) > 0 {
			for _, issue := range generated.Lossy[version] {
				if issue.Code != model.CodeLossy || issue.Handling == "" {
					t.Fatalf("%s lossy issue lacks handling: %+v", version, issue)
				}
			}
		}
	}
	if len(generated.Lossy[model.SchemaV1]) <= len(generated.Lossy[model.SchemaV2]) {
		t.Fatal("v1 should report at least as many lossy issues as v2")
	}
}

func TestRoundTripCoordinatesSequenceAndWeight(t *testing.T) {
	bundle, err := sample.Generate(9002, []int{96})
	if err != nil {
		t.Fatal(err)
	}
	base, _, err := schema.Load(bundle.Projects[0].Artifacts[model.SchemaV3])
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []model.SchemaVersion{model.SchemaV2, model.SchemaV1} {
		exported, err := schema.Export(base, target)
		if err != nil {
			t.Fatalf("export %s: %v", target, err)
		}
		low, _, err := schema.Load(exported.Artifact)
		if err != nil {
			t.Fatalf("read back %s: %v", target, err)
		}
		back, err := schema.Export(low, model.SchemaV3)
		if err != nil {
			t.Fatalf("re-export %s: %v", target, err)
		}
		high, _, err := schema.Load(back.Artifact)
		if err != nil {
			t.Fatalf("read back upgraded %s: %v", target, err)
		}
		if len(high.Strokes) != len(base.Strokes) {
			t.Fatal("stroke count changed")
		}
		for index := range base.Strokes {
			left, right := base.Strokes[index], high.Strokes[index]
			if left.ID != right.ID || left.Seq != right.Seq || left.X0 != right.X0 || left.Y0 != right.Y0 ||
				left.X1 != right.X1 || left.Y1 != right.Y1 || left.Weight != right.Weight {
				t.Fatalf("%s round trip changed stroke %s: %+v != %+v", target, left.ID, left, right)
			}
		}
	}
}

func TestSchemaErrors(t *testing.T) {
	bundle, err := sample.Generate(9003, []int{96})
	if err != nil {
		t.Fatal(err)
	}
	v3 := bundle.Projects[0].Artifacts[model.SchemaV3]
	var root map[string]any
	if err := json.Unmarshal(v3, &root); err != nil {
		t.Fatal(err)
	}
	root["schemaVersion"] = "v9"
	unknown := mustJSON(t, root)
	if _, _, err := schema.Load(unknown); err == nil || !model.HasCode(err, model.CodeSchema) ||
		!strings.Contains(err.Error(), "$.schemaVersion") {
		t.Fatalf("unknown version did not report E_SCHEMA with path: %v", err)
	}

	if err := json.Unmarshal(v3, &root); err != nil {
		t.Fatal(err)
	}
	strokes := root["strokes"].([]any)
	first := strokes[0].(map[string]any)
	delete(first["start"].(map[string]any), "x")
	missing := mustJSON(t, root)
	if _, _, err := schema.Load(missing); err == nil || !model.HasCode(err, model.CodeSchema) ||
		!strings.Contains(err.Error(), "$.strokes[0].start.x") || !strings.Contains(err.Error(), "s01") {
		t.Fatalf("missing field did not report path and object id: %v", err)
	}

	if err := json.Unmarshal(v3, &root); err != nil {
		t.Fatal(err)
	}
	first = root["strokes"].([]any)[0].(map[string]any)
	first["coveragePermyriad"] = 10001
	badWeight := mustJSON(t, root)
	if _, _, err := schema.Load(badWeight); err == nil || !model.HasCode(err, model.CodeParam) {
		t.Fatalf("bad weight did not report E_PARA: %v", err)
	}

	var v2 map[string]any
	if err := json.Unmarshal(bundle.Projects[0].Artifacts[model.SchemaV2], &v2); err != nil {
		t.Fatal(err)
	}
	v2Strokes := v2["strokes"].([]any)
	firstV2 := v2Strokes[0].(map[string]any)
	firstV2["relative"].(map[string]any)["dx1"] = int64(1) << 62
	secondV2 := v2Strokes[1].(map[string]any)
	secondV2["relative"].(map[string]any)["dx0"] = int64(1) << 62
	overflow := mustJSON(t, v2)
	if _, _, err := schema.Load(overflow); err == nil || !model.HasCode(err, model.CodeOverflow) ||
		!strings.Contains(err.Error(), "s02") {
		t.Fatalf("overflow did not report E_OVF with operands: %v", err)
	}

	project, _, err := schema.Load(bundle.Projects[0].Artifacts[model.SchemaV3])
	if err != nil {
		t.Fatal(err)
	}
	project.Strokes[0].X1 = 1 << 62
	project.Strokes[1].X0 = math.MinInt64
	exported, exportErr := schema.Export(project, model.SchemaV2)
	if exportErr == nil || !model.HasCode(exportErr, model.CodeOverflow) || exported.Artifact != nil {
		t.Fatalf("export overflow did not fail with E_OVF: result=%+v err=%v", exported, exportErr)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
