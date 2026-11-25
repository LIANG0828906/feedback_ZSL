package schema_test

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"lipsync/domain"
	"lipsync/generate"
	"lipsync/schema"
	"lipsync/truth"
)

func samples(t *testing.T) (*generate.Report, *truth.Manifest) {
	t.Helper()
	rep, err := generate.Generate(generate.Config{OutDir: t.TempDir(), Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	m, err := truth.LoadFile(rep.Truth)
	if err != nil {
		t.Fatal(err)
	}
	return rep, m
}

func TestLoadsAllThreeSchemasAndRecordsTraces(t *testing.T) {
	rep, _ := samples(t)
	expectedVersions := map[string]string{"v1": "dialogue/v1", "v2": "dialogue/v2", "v3": "dialogue/v3"}
	for key, wantVersion := range expectedVersions {
		p, _, err := schema.LoadBundle(rep.Projects[key])
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if p.SourceVersion != wantVersion {
			t.Fatalf("%s source version = %s", key, p.SourceVersion)
		}
		if len(p.Traces) == 0 {
			t.Fatalf("%s did not retain conversion traces", key)
		}
		if key == "v1" && len(p.Defaults) == 0 {
			t.Fatalf("v1 did not record affinity defaults")
		}
		if key == "v3" && p.CategoryEncoding != "grouped" {
			t.Fatalf("v3 category encoding = %s", p.CategoryEncoding)
		}
	}
}

func TestUnknownVersionAndMissingFieldReturnSchemaError(t *testing.T) {
	rep, _ := samples(t)
	data, err := os.ReadFile(rep.Projects["v3"])
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	doc["schema"] = "dialogue/v99"
	b, _ := json.Marshal(doc)
	_, err = schema.Load(b)
	if domain.ErrorCode(err) != domain.CodeSchema {
		t.Fatalf("unknown version code = %s: %v", domain.ErrorCode(err), err)
	}
	de, _ := domain.AsError(err)
	if de.Path != "$.schema" || de.Version != "dialogue/v99" {
		t.Fatalf("unknown version error = %+v", de)
	}

	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	render := doc["render"].(map[string]any)
	delete(render, "target_fps")
	b, _ = json.Marshal(doc)
	_, err = schema.Load(b)
	if domain.ErrorCode(err) != domain.CodeSchema {
		t.Fatalf("missing field code = %s: %v", domain.ErrorCode(err), err)
	}
	de, _ = domain.AsError(err)
	if de.Path != "$.render.target_fps" || de.Version != "dialogue/v3" {
		t.Fatalf("missing field error = %+v", de)
	}
}

func TestDowngradeLossAndRoundTripIntervalsWeights(t *testing.T) {
	rep, _ := samples(t)
	p3, _, err := schema.LoadBundle(rep.Projects["v3"])
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []schema.Version{schema.V1, schema.V2} {
		b, report, err := schema.Marshal(p3, target)
		if err != nil {
			t.Fatalf("marshal %s: %v", target, err)
		}
		back, err := schema.Load(b)
		if err != nil {
			t.Fatalf("load exported %s: %v", target, err)
		}
		assertIntervalsWeightsEqual(t, p3, back)
		if target == schema.V1 {
			if !report.HasLosses() {
				t.Fatalf("v3->v1 did not report loss")
			}
			if domain.ErrorCode(report.AsError()) != domain.CodeLossy {
				t.Fatalf("loss report code = %s", domain.ErrorCode(report.AsError()))
			}
			foundGroup, foundAffinity := false, false
			for _, item := range report.Items {
				if item.Path == "$.category_groups" {
					foundGroup = true
				}
				if bytes.Contains([]byte(item.Path), []byte("affinity")) {
					foundAffinity = true
				}
			}
			if !foundGroup || !foundAffinity {
				t.Fatalf("loss report missing group/affinity items: %+v", report.Items)
			}
		}
	}
}

func TestHighLowHighVersionRoundTrip(t *testing.T) {
	rep, _ := samples(t)
	p3, _, err := schema.LoadBundle(rep.Projects["v3"])
	if err != nil {
		t.Fatal(err)
	}
	p1, _, err := schema.Convert(p3, schema.V1)
	if err != nil {
		t.Fatal(err)
	}
	back, _, err := schema.Convert(p1, schema.V3)
	if err != nil {
		t.Fatal(err)
	}
	assertIntervalsWeightsEqual(t, p3, back)
}

func TestLoaderCategoryErrorLocatesShardAndPhoneme(t *testing.T) {
	rep, _ := samples(t)
	data, _ := os.ReadFile(rep.Projects["v1"])
	var doc map[string]any
	_ = json.Unmarshal(data, &doc)
	dialogues := doc["dialogues"].([]any)
	phones := dialogues[1].(map[string]any)["phonemes"].([]any)
	phones[0].(map[string]any)["category"] = "X"
	b, _ := json.Marshal(doc)
	_, err := schema.Load(b)
	if domain.ErrorCode(err) != domain.CodeClass {
		t.Fatalf("code = %s: %v", domain.ErrorCode(err), err)
	}
	de, _ := domain.AsError(err)
	if de.ShardIndex != 1 || de.PhonemeID == "" || de.DialogueID == "" {
		t.Fatalf("class error location = %+v", de)
	}
}

func TestSampleAnnotationMustMatchDeclaredFrameInterval(t *testing.T) {
	rep, _ := samples(t)
	data, _ := os.ReadFile(rep.Projects["v3"])
	var doc map[string]any
	_ = json.Unmarshal(data, &doc)
	dialogues := doc["dialogues"].([]any)
	phones := dialogues[0].(map[string]any)["phonemes"].([]any)
	phone := phones[0].(map[string]any)
	phone["sample_end"] = phone["sample_end"].(float64) + 960
	b, _ := json.Marshal(doc)
	_, err := schema.Load(b)
	if domain.ErrorCode(err) != domain.CodeSchema {
		t.Fatalf("code = %s: %v", domain.ErrorCode(err), err)
	}
	de, _ := domain.AsError(err)
	if de.Path == "" || de.Version != "dialogue/v3" {
		t.Fatalf("sample mismatch context = %+v", de)
	}
}

func assertIntervalsWeightsEqual(t *testing.T, a, b *domain.Project) {
	t.Helper()
	if len(a.Dialogues) != len(b.Dialogues) {
		t.Fatalf("dialogue count %d != %d", len(a.Dialogues), len(b.Dialogues))
	}
	bm := make(map[string]domain.Dialogue)
	for _, d := range b.Dialogues {
		bm[d.ID] = d
	}
	for _, ad := range a.Dialogues {
		bd, ok := bm[ad.ID]
		if !ok || len(ad.Phonemes) != len(bd.Phonemes) {
			t.Fatalf("dialogue %s missing or phone count differs", ad.ID)
		}
		bp := make(map[string]domain.Phoneme)
		for _, ph := range bd.Phonemes {
			bp[ph.ID] = ph
		}
		for _, ph := range ad.Phonemes {
			other := bp[ph.ID]
			if ph.StartFrame != other.StartFrame || ph.EndFrame != other.EndFrame ||
				ph.WeightPermyriad != other.WeightPermyriad {
				t.Fatalf("%s/%s: %d..%d/%d != %d..%d/%d", ad.ID, ph.ID,
					ph.StartFrame, ph.EndFrame, ph.WeightPermyriad,
					other.StartFrame, other.EndFrame, other.WeightPermyriad)
			}
		}
	}
}
