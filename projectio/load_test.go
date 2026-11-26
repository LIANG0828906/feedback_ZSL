package projectio_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"dialoguefit/generate"
	"dialoguefit/model"
	"dialoguefit/projectio"
)

func writeJSON(t *testing.T, name string, v any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func domainError(t *testing.T, err error) *model.Error {
	t.Helper()
	var de *model.Error
	if !errors.As(err, &de) {
		t.Fatalf("expected *model.Error, got %T: %v", err, err)
	}
	return de
}

func TestLoadBundleMissingTruthIsEDecode(t *testing.T) {
	p := generate.GenerateProject(generate.DefaultSeed)
	projectPath := writeJSON(t, "project.json", p)
	_, _, err := projectio.LoadBundle(projectPath, filepath.Join(t.TempDir(), "missing-truth.json"))
	de := domainError(t, err)
	if de.Code != model.CodeDecode || de.Field != "truth_file" {
		t.Fatalf("unexpected error: %+v", de)
	}
}

func TestValidateProjectErrorsIdentifyDialogueAndField(t *testing.T) {
	p := generate.GenerateProject(generate.DefaultSeed)
	p.Dialogues[0].Params.TargetSamples = 0
	err := projectio.ValidateProject(p)
	de := domainError(t, err)
	if de.Code != model.CodeDecode || de.DialogueID != "dlg-01" || de.Field != "params.target_samples" {
		t.Fatalf("unexpected target error: %+v", de)
	}

	p = generate.GenerateProject(generate.DefaultSeed)
	p.Dialogues[0].PCM.Data = "!!!not-base64!!!"
	p.Dialogues[0].PCM.Samples = nil
	err = projectio.ValidateProject(p)
	de = domainError(t, err)
	if de.Code != model.CodeDecode || de.DialogueID != "dlg-01" || de.Field != "pcm.data" {
		t.Fatalf("unexpected pcm error: %+v", de)
	}

	p = generate.GenerateProject(generate.DefaultSeed)
	p.Dialogues[0].Phonemes[2].StartSample = p.Dialogues[0].Phonemes[1].EndSample - 1
	err = projectio.ValidateProject(p)
	de = domainError(t, err)
	if de.Code != model.CodeDecode || de.DialogueID != "dlg-01" || de.Field != "phonemes[2].start_sample" {
		t.Fatalf("unexpected overlap error: %+v", de)
	}

	p = generate.GenerateProject(generate.DefaultSeed)
	p.Dialogues[0].Phonemes[1].F0 = p.Dialogues[0].Phonemes[1].F0[:3]
	err = projectio.ValidateProject(p)
	de = domainError(t, err)
	if de.Code != model.CodeDecode || de.Field != "phonemes[1].f0" {
		t.Fatalf("unexpected f0 error: %+v", de)
	}
}

func TestTruncatedJSONReturnsEDecode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "project.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":"1","dialogues":[`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := projectio.LoadProjectFile(path)
	de := domainError(t, err)
	if de.Code != model.CodeDecode || de.Field != "json" {
		t.Fatalf("unexpected truncation error: %+v", de)
	}
}
