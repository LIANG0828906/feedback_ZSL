package schema_test

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"

	"prosodyalign/internal/errs"
	"prosodyalign/internal/generator"
	"prosodyalign/internal/model"
	"prosodyalign/internal/schema"
)

func generatedProject(t *testing.T) *model.Project {
	t.Helper()
	suite, err := generator.GenerateSuite(generator.FixedSeed, 1)
	if err != nil {
		t.Fatal(err)
	}
	return suite.Dialogs[0].Project
}

func TestAllVersionsLoadToSameCanonicalHash(t *testing.T) {
	p := generatedProject(t)
	for _, version := range []model.SchemaName{model.SchemaV1, model.SchemaV2, model.SchemaV3} {
		data, report, err := schema.Export(p, version)
		if err != nil {
			t.Fatalf("export %s: %v", version, err)
		}
		loaded, loadReport, err := schema.Load(data)
		if err != nil {
			t.Fatalf("load %s: %v report=%+v", version, err, loadReport)
		}
		if loaded.ContentHash != p.ContentHash {
			t.Fatalf("%s hash mismatch\nwant=%s\ngot =%s", version, p.ContentHash, loaded.ContentHash)
		}
		for i := range p.Phonemes {
			a, b := p.Phonemes[i], loaded.Phonemes[i]
			if a.StartSample != b.StartSample || a.EndSample != b.EndSample {
				t.Fatalf("%s phoneme %d timing mismatch: %d-%d vs %d-%d", version, i, a.StartSample, a.EndSample, b.StartSample, b.EndSample)
			}
			if a.WeightPer10k != b.WeightPer10k {
				t.Fatalf("%s phoneme %d weight mismatch", version, i)
			}
			if !reflect.DeepEqual(a.F0, b.F0) {
				t.Fatalf("%s phoneme %d F0 mismatch: %#v vs %#v", version, i, a.F0, b.F0)
			}
		}
		_ = report
	}
}

func TestV1LossyF0AndCapReportAndExtensionRoundTrip(t *testing.T) {
	p := generatedProject(t)
	p.Phonemes[1].F0[0].MilliHz = 12345
	data, report, err := schema.Export(p, model.SchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	foundF0Loss := false
	foundCapLoss := false
	for _, n := range report.Notes {
		if n.Code == errs.ELossy && n.FieldPath == "$.phonemes[1].f0_centi_hz_trajectory[0].f0_centi_hz" {
			foundF0Loss = true
		}
		if n.Code == errs.ELossy && n.FieldPath == "$.default_max_stretch_samples" {
			foundCapLoss = true
		}
	}
	if !foundF0Loss || !foundCapLoss {
		t.Fatalf("missing loss reports f0=%v cap=%v: %+v", foundF0Loss, foundCapLoss, report.Notes)
	}
	loaded, _, err := schema.Load(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Phonemes[1].F0[0].MilliHz; got != 12345 {
		t.Fatalf("loss-preserving extension roundtrip got %d", got)
	}
	if loaded.DeclaredMaxStretch != p.DeclaredMaxStretch {
		t.Fatalf("cap extension got %d want %d", loaded.DeclaredMaxStretch, p.DeclaredMaxStretch)
	}
}

func TestUnknownSchemaAndMissingRequiredField(t *testing.T) {
	_, _, err := schema.Load([]byte(`{"schema":"prosody.v9"}`))
	ce, ok := errs.As(err)
	if !ok || ce.Detail.Code != errs.ESchema || ce.Detail.FieldPath != "$.schema" || ce.Detail.SourceVersion != "prosody.v9" {
		t.Fatalf("unexpected unknown schema error: %+v", err)
	}

	p := generatedProject(t)
	data, _, err := schema.Export(p, model.SchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc, "target_duration_samples")
	bad, _ := json.Marshal(doc)
	_, _, err = schema.Load(bad)
	ce, ok = errs.As(err)
	if !ok || ce.Detail.Code != errs.ESchema || ce.Detail.FieldPath != "$.target_duration_samples" || ce.Detail.SourceVersion != schema.V1Name {
		t.Fatalf("unexpected missing field error: %+v", err)
	}
}

func TestConversionOverflow(t *testing.T) {
	_, _, err := schema.SamplesToMillis(math.MaxInt64, 16000, "$.huge")
	ce, ok := errs.As(err)
	if !ok || ce.Detail.Code != errs.EOvf || ce.Detail.FieldPath != "$.huge" || len(ce.Detail.Operands) != 2 {
		t.Fatalf("expected E_OVF with field and operands, got %+v", err)
	}
}

func TestV1DefaultFillReportForBareEnvelopeAndCap(t *testing.T) {
	doc := map[string]any{
		"schema": schema.V1Name, "dialog_id": "bare", "seed": 1, "sample_rate_hz": 16000,
		"pcm": []int16{1, -1, 2, -2}, "target_duration_samples": int64(160),
		"min_silence_hold_samples": 0, "accent_tolerance_samples": 10,
		"phonemes": []map[string]any{{
			"phoneme_id": "p0", "start_sample": 0, "end_sample": 4, "class": "vowel",
			"stretchable": true, "loudness_weight_permille": 100,
		}},
	}
	data, _ := json.Marshal(doc)
	p, report, err := schema.Load(data)
	if err != nil {
		t.Fatal(err)
	}
	if p.EnvelopeParams.FrameLengthSamples <= 0 || p.DeclaredMaxStretch <= 0 {
		t.Fatalf("defaults not filled: %+v", p.EnvelopeParams)
	}
	found := false
	for _, n := range report.Notes {
		if n.Code == "DEFAULT_FILLED" && n.FieldPath == "$.default_max_stretch_samples" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing default fill note: %+v", report.Notes)
	}
}
