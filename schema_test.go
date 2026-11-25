package dialogue

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func sampleProject(t *testing.T) (*Project, *TruthManifest, map[int][]byte) {
	t.Helper()
	p, truth, err := GenerateProject(GeneratorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	docs, _, err := GenerateDocuments(p)
	if err != nil {
		t.Fatal(err)
	}
	return p, truth, docs
}

func TestThreeSchemaVersionsLoadUnifiedModel(t *testing.T) {
	p, _, docs := sampleProject(t)
	for _, version := range []int{1, 2, 3} {
		sourceID := "case-v" + string(rune('0'+version)) + ".json"
		loaded, err := LoadDocument(docs[version], sourceID)
		if err != nil {
			t.Fatalf("v%d load: %v", version, err)
		}
		if loaded.SourceFile != sourceID || loaded.SchemaVersion != version {
			t.Fatalf("source identity/version not retained: %s v%d", loaded.SourceFile, loaded.SchemaVersion)
		}
		if len(loaded.Clips) != len(p.Clips) {
			t.Fatalf("v%d clips=%d want %d", version, len(loaded.Clips), len(p.Clips))
		}
		for i, clip := range loaded.Clips {
			if clip.ID != p.Clips[i].ID || clip.AudioHash != p.Clips[i].AudioHash {
				t.Fatalf("v%d clip %d identity/hash mismatch", version, i)
			}
			if abs64(clip.GainBP-p.Clips[i].GainBP) > RoundTripGainToleranceBP {
				t.Fatalf("v%d gain drift: %d vs %d", version, clip.GainBP, p.Clips[i].GainBP)
			}
		}
		if len(loaded.Traces) == 0 && version != 3 {
			t.Fatalf("v%d expected default/field traces", version)
		}
	}
}

func TestSchemaUnknownAndMissingRequired(t *testing.T) {
	_, err := LoadDocument([]byte(`{"schema_version":99}`), "unknown.json")
	var be *Error
	if !errors.As(err, &be) || be.Code != CodeSchema || be.Path != "$.schema_version" || be.SourceID != "unknown.json" {
		t.Fatalf("unexpected unknown-version error: %#v", err)
	}

	missing := []byte(`{"schema":"dialogue-v1","clips":[{"id":"c1"}]}`)
	_, err = LoadDocument(missing, "missing.json")
	if !errors.As(err, &be) || be.Code != CodeSchema || be.SourceID != "missing.json" {
		t.Fatalf("expected E_SCHEMA with source id, got %#v", err)
	}
	if !strings.Contains(be.Path, "$.") {
		t.Fatalf("error path should identify field: %+v", be)
	}
}

func TestDowngradeLossReportsAndRoundTrips(t *testing.T) {
	p, _, _ := sampleProject(t)
	for _, version := range []int{1, 2} {
		rt, err := VerifyRoundTrip(p, version)
		if err != nil {
			t.Fatalf("round trip v%d: %v", version, err)
		}
		if !rt.OK {
			t.Fatalf("v%d round trip residuals outside tolerance: %+v", version, rt.Residuals)
		}
		if rt.Loss == nil || !rt.Loss.HasLoss() {
			t.Fatalf("v%d expected E_LOSSY downgrade items", version)
		}
		codes := map[string]int{}
		for _, loss := range rt.Loss.Losses {
			codes[loss.Code]++
			if loss.Code != CodeLossy || loss.Path == "" || loss.Disposition == "" || loss.Original == nil {
				t.Fatalf("incomplete loss item: %+v", loss)
			}
		}
		if codes[CodeLossy] == 0 {
			t.Fatalf("missing E_LOSSY code")
		}
	}

	drift, err := VerifyVersionPathDrift(p)
	if err != nil || !drift.OK {
		t.Fatalf("version path drift: report=%+v err=%v", drift, err)
	}
}

func TestGainScaleContradictionIsE_PARA(t *testing.T) {
	doc := []byte(`{
	  "schema_version": 3,
	  "dialogue_id": "para",
	  "calibration": {"target_loud_mdb": -20000},
	  "scenes": [{"name":"s","clips":[{
	    "id":"c1","start_sample":0,
	    "gain_linear_micro":1000000,
	    "gain_bp":12000,
	    "pcm_s16":[0,1,-1,0]
	  }]}]
	}`)
	_, err := LoadDocument(doc, "para.json")
	var be *Error
	if !errors.As(err, &be) || be.Code != CodePara || be.ClipID != "c1" {
		t.Fatalf("expected E_PARA located at c1, got %#v", err)
	}
}

func TestGeneratedDocumentsAreValidJSON(t *testing.T) {
	_, _, docs := sampleProject(t)
	for v, data := range docs {
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatalf("v%d document not JSON: %v", v, err)
		}
	}
}
