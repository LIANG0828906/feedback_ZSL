package domain_test

import (
	"encoding/json"
	"strings"
	"testing"

	"lipsync.local/prototype/pkg/domain"
	"lipsync.local/prototype/pkg/sample"
)

func TestLoadProjectSchemaOrderAndRateErrors(t *testing.T) {
	project, _, err := sample.Generate(sample.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(project)
	var raw map[string]any
	_ = json.Unmarshal(data, &raw)
	dialogues := raw["dialogues"].([]any)
	first := dialogues[0].(map[string]any)
	phonemes := first["phonemes"].([]any)
	firstPhoneme := phonemes[1].(map[string]any)
	delete(firstPhoneme, "weight_permille")
	broken, _ := json.Marshal(raw)
	_, err = domain.LoadProject(broken)
	if code := domain.CodeOf(err); code != domain.ErrSchema {
		t.Fatalf("want E_SCHEMA, got %s: %v", code, err)
	}
	de := err.(*domain.Error)
	if !strings.HasSuffix(de.Path, ".weight_permille") || de.PhonemeID != project.Dialogues[0].Phonemes[1].ID {
		t.Fatalf("schema error lacks path/phoneme evidence: %+v", de)
	}

	_, err = domain.LoadProject([]byte(`{"version":`))
	if code := domain.CodeOf(err); code != domain.ErrDecode {
		t.Fatalf("want E_DECODE, got %s: %v", code, err)
	}

	reversed := clone(t, project)
	reversed.Dialogues[0].Phonemes[1].StartSample = reversed.Dialogues[0].Phonemes[0].EndSample - 1
	_, err = domain.LoadProject(mustJSON(t, reversed))
	if code := domain.CodeOf(err); code != domain.ErrOrder {
		t.Fatalf("want E_ORDER, got %s: %v", code, err)
	}

	badRate := clone(t, project)
	badRate.Recipe.FrameRate = 45
	_, err = domain.LoadProject(mustJSON(t, badRate))
	if code := domain.CodeOf(err); code != domain.ErrRate {
		t.Fatalf("want E_RATE, got %s: %v", code, err)
	}

	badCategory := clone(t, project)
	badCategory.Dialogues[0].Phonemes[0].Category = "nasal"
	_, err = domain.LoadProject(mustJSON(t, badCategory))
	if code := domain.CodeOf(err); code != domain.ErrSchema {
		t.Fatalf("want E_SCHEMA, got %s: %v", code, err)
	}
	if de := err.(*domain.Error); de.PhonemeID == "" || !strings.HasSuffix(de.Path, ".category") {
		t.Fatalf("category error lacks evidence: %+v", de)
	}
}

func clone(t *testing.T, project *domain.Project) *domain.Project {
	t.Helper()
	var out domain.Project
	if err := json.Unmarshal(mustJSON(t, project), &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
