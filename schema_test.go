package lipsync

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestLoadProjectsRoundTrip(t *testing.T) {
	set := sampleSetForTest(t, 2)
	data, err := json.Marshal(set.Projects)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadProjects(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != len(set.Projects) {
		t.Fatalf("loaded %d projects, want %d", len(loaded), len(set.Projects))
	}
	for i := range loaded {
		if loaded[i].DialogueID != set.Projects[i].DialogueID {
			t.Fatalf("dialogue %d = %s, want %s", i, loaded[i].DialogueID, set.Projects[i].DialogueID)
		}
	}
}

func TestMissingFieldReturnsSchemaPath(t *testing.T) {
	set := sampleSetForTest(t, 1)
	data, err := json.Marshal(set.Projects)
	if err != nil {
		t.Fatal(err)
	}
	var root []any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	project := root[0].(map[string]any)
	segments := project["segments"].([]any)
	phonemes := segments[0].(map[string]any)["phonemes"].([]any)
	delete(phonemes[0].(map[string]any), "lip")
	data, err = json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = LoadProjects(bytes.NewReader(data))
	if ErrorCode(err) != CodeSchema {
		t.Fatalf("code = %s, want E_SCHEMA: %v", ErrorCode(err), err)
	}
	de, _ := asDomainError(err)
	if de.Field != "projects[0].segments[0].phonemes[0].lip" || de.DialogueID != set.Projects[0].DialogueID {
		t.Fatalf("error context = %+v", de)
	}
}

func TestMissingTopLevelFieldReturnsSchemaPath(t *testing.T) {
	set := sampleSetForTest(t, 1)
	data, _ := json.Marshal(set.Projects)
	var root []any
	_ = json.Unmarshal(data, &root)
	delete(root[0].(map[string]any), "fps")
	data, _ = json.Marshal(root)
	_, err := LoadProjects(bytes.NewReader(data))
	de, _ := asDomainError(err)
	if ErrorCode(err) != CodeSchema || de.Field != "projects[0].fps" {
		t.Fatalf("missing fps error = %+v", de)
	}
}

func TestValidationErrorChains(t *testing.T) {
	base := sampleSetForTest(t, 1).Projects[0]
	t.Run("order", func(t *testing.T) {
		project := base
		project.Segments = append([]Segment(nil), base.Segments...)
		project.Segments[0].Phonemes = append([]Phoneme(nil), base.Segments[0].Phonemes...)
		project.Segments[0].Phonemes[1].StartSample = project.Segments[0].Phonemes[0].StartSample
		err := ValidateProject(project)
		de, _ := asDomainError(err)
		if ErrorCode(err) != CodeOrder || de.Field != "segments[0].phonemes[1].start_sample" {
			t.Fatalf("order error = %+v", de)
		}
	})
	t.Run("rate", func(t *testing.T) {
		project := base
		project.FPS = 48001
		err := ValidateProject(project)
		de, _ := asDomainError(err)
		if ErrorCode(err) != CodeRate || de.Field != "fps" || de.FPS == nil || *de.FPS != 48001 {
			t.Fatalf("rate error = %+v", de)
		}
	})
	t.Run("lip", func(t *testing.T) {
		project := base
		project.Segments = append([]Segment(nil), base.Segments...)
		project.Segments[0].Phonemes = append([]Phoneme(nil), base.Segments[0].Phonemes...)
		project.Segments[0].Phonemes[0].Lip = "bogus"
		err := ValidateProject(project)
		de, _ := asDomainError(err)
		if ErrorCode(err) != CodeLip || de.Frame == nil || de.Field != "segments[0].phonemes[0].lip" {
			t.Fatalf("lip error = %+v", de)
		}
	})
	t.Run("weights", func(t *testing.T) {
		project := base
		project.Subtitle.Weights = append(append([]int(nil), base.Subtitle.Weights...), 1)
		err := ValidateProject(project)
		de, _ := asDomainError(err)
		if ErrorCode(err) != CodeSchema || de.Field != "subtitle.weights" {
			t.Fatalf("weight error = %+v", de)
		}
	})
	t.Run("sample_rate", func(t *testing.T) {
		project := base
		project.SampleRate = 10
		err := ValidateProject(project)
		if ErrorCode(err) != CodeRate {
			t.Fatalf("sample rate code = %s, want E_RATE", ErrorCode(err))
		}
	})
}
