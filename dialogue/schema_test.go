package dialogue

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSchemaErrorsCarryDialogueAndPath(t *testing.T) {
	project, err := GenerateProject(42)
	if err != nil {
		t.Fatal(err)
	}
	exported, err := ExportProject(project, Version3)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(exported.Data, &doc); err != nil {
		t.Fatal(err)
	}
	dialogueDoc := doc["dialogue"].(map[string]any)
	delete(dialogueDoc, "targetLoudnessBasisPoints")
	data, _ := json.Marshal(doc)
	_, err = LoadProject(data)
	assertSchemaError(t, err, "dialogue.targetLoudnessBasisPoints", project.ID)

	if err := json.Unmarshal(exported.Data, &doc); err != nil {
		t.Fatal(err)
	}
	scenes := doc["scenes"].([]any)
	scene := scenes[0].(map[string]any)
	segments := scene["segments"].([]any)
	segment := segments[0].(map[string]any)
	phonemes := segment["phonemes"].([]any)
	phoneme := phonemes[0].(map[string]any)
	phoneme["category"] = "nasal"
	data, _ = json.Marshal(doc)
	_, err = LoadProject(data)
	assertSchemaError(t, err, ".kind", project.ID)

	if err := json.Unmarshal(exported.Data, &doc); err != nil {
		t.Fatal(err)
	}
	scenes = doc["scenes"].([]any)
	scene = scenes[0].(map[string]any)
	segments = scene["segments"].([]any)
	segment = segments[0].(map[string]any)
	phonemes = segment["phonemes"].([]any)
	first := phonemes[0].(map[string]any)
	second := phonemes[1].(map[string]any)
	second["startSample"] = first["endSample"].(float64) - 1
	data, _ = json.Marshal(doc)
	_, err = LoadProject(data)
	assertSchemaError(t, err, ".start_sample", project.ID)

	if err := json.Unmarshal(exported.Data, &doc); err != nil {
		t.Fatal(err)
	}
	scenes = doc["scenes"].([]any)
	scene = scenes[0].(map[string]any)
	segments = scene["segments"].([]any)
	segment = segments[0].(map[string]any)
	segment["sessionGainPermille"] = 99999
	data, _ = json.Marshal(doc)
	_, err = LoadProject(data)
	assertSchemaError(t, err, ".session_gain", project.ID)

	if err := json.Unmarshal(exported.Data, &doc); err != nil {
		t.Fatal(err)
	}
	pcm := doc["pcm"].(map[string]any)
	samples := pcm["samples"].([]any)
	pcm["samples"] = append(samples, 0)
	data, _ = json.Marshal(doc)
	_, err = LoadProject(data)
	assertSchemaError(t, err, "pcm.samples", project.ID)
}

func assertSchemaError(t *testing.T, err error, pathPart, dialogueID string) {
	t.Helper()
	if CodeOf(err) != CodeSchema {
		t.Fatalf("expected E_SCHEMA, got %v", err)
	}
	kernelError := err.(*Error)
	if kernelError.DialogueID != dialogueID || !strings.Contains(kernelError.Path, pathPart) {
		t.Fatalf("wrong context: %+v", kernelError)
	}
}

func TestV1UpgradeRecordsDefaults(t *testing.T) {
	project, err := GenerateProject(42)
	if err != nil {
		t.Fatal(err)
	}
	exported, err := ExportProject(project, Version1)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadProject(exported.Data)
	if err != nil {
		t.Fatal(err)
	}
	var sceneDefaults, f0Defaults int
	for _, trace := range loaded.Conversions {
		if strings.HasSuffix(trace.Path, ".scene_group") && trace.Code == CodeDefault {
			sceneDefaults++
		}
		if strings.HasSuffix(trace.Path, ".f0_breakpoints") && trace.Code == CodeDefault {
			f0Defaults++
		}
	}
	if sceneDefaults != len(project.Segments()) || f0Defaults == 0 {
		t.Fatalf("missing upgrade traces: scenes=%d f0=%d", sceneDefaults, f0Defaults)
	}
	converted, err := ConvertVersion(exported.Data, Version3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProject(converted.Data); err != nil {
		t.Fatalf("converted v3 does not reload: %v", err)
	}
	if len(converted.Upgrades) == 0 {
		t.Fatal("converted document lost upgrade traces")
	}
}

func TestDowngradeUpgradePreservesBoundariesAndGainWithinResidual(t *testing.T) {
	project, err := GenerateProject(42)
	if err != nil {
		t.Fatal(err)
	}
	v2Export, err := ExportProject(project, Version2)
	if err != nil {
		t.Fatal(err)
	}
	v2Loaded, err := LoadProject(v2Export.Data)
	if err != nil {
		t.Fatal(err)
	}
	v1Export, err := ExportProject(v2Loaded.Project, Version1)
	if err != nil {
		t.Fatal(err)
	}
	v1Loaded, err := LoadProject(v1Export.Data)
	if err != nil {
		t.Fatal(err)
	}
	v3Export, err := ExportProject(v1Loaded.Project, Version3)
	if err != nil {
		t.Fatal(err)
	}
	v3Loaded, err := LoadProject(v3Export.Data)
	if err != nil {
		t.Fatal(err)
	}
	originalSegments := project.Segments()
	roundtripped := v3Loaded.Project.Segments()
	if len(originalSegments) != len(roundtripped) {
		t.Fatalf("segment count %d != %d", len(originalSegments), len(roundtripped))
	}
	for i := range originalSegments {
		original := originalSegments[i]
		actual := roundtripped[i]
		if original.StartSample != actual.StartSample || original.EndSample != actual.EndSample {
			t.Fatalf("%s boundary changed: [%d,%d] -> [%d,%d]", original.ID, original.StartSample, original.EndSample, actual.StartSample, actual.EndSample)
		}
		if abs64(original.SessionGain-actual.SessionGain) > 5 {
			t.Fatalf("%s gain changed beyond residual: %d -> %d", original.ID, original.SessionGain, actual.SessionGain)
		}
		if len(original.Phonemes) != len(actual.Phonemes) {
			t.Fatalf("%s phoneme count changed", original.ID)
		}
		for j := range original.Phonemes {
			if original.Phonemes[j].StartSample != actual.Phonemes[j].StartSample ||
				original.Phonemes[j].EndSample != actual.Phonemes[j].EndSample {
				t.Fatalf("%s phoneme %d boundary changed", original.ID, j)
			}
		}
	}
}
