package codec_test

import (
	"testing"

	"dialogueaudio/audioerr"
	"dialogueaudio/codec"
	"dialogueaudio/generator"
	"dialogueaudio/model"
)

func TestLoadAllGeneratedVersions(t *testing.T) {
	set, err := generator.Build(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	source := make(map[string]model.Clip)
	for _, clip := range set.Project.Clips {
		source[clip.ID] = clip
	}
	for version := 1; version <= 3; version++ {
		project, err := codec.Load(set.Files[map[int]string{1: "schema_v1.json", 2: "schema_v2.json", 3: "schema_v3.json"}[version]])
		if err != nil {
			t.Fatalf("v%d: %v", version, err)
		}
		if project.SourceVersion != version {
			t.Fatalf("v%d source version %d", version, project.SourceVersion)
		}
		if len(project.Clips) != len(set.Project.Clips) {
			t.Fatalf("v%d clip count", version)
		}
		for _, clip := range project.Clips {
			want := source[clip.ID]
			if clip.StartSample != want.StartSample || clip.EndSample != want.EndSample ||
				clip.GainQ16 != want.GainQ16 || clip.LoudnessWeightQ16 != want.LoudnessWeightQ16 ||
				clip.ContentHash != want.ContentHash {
				t.Fatalf("v%d clip %s core mismatch: got %+v want %+v", version, clip.ID, clip, want)
			}
		}
	}
}

func TestSchemaErrorsCarryPath(t *testing.T) {
	_, err := codec.Load([]byte(`{"schema_version":99}`))
	coded, ok := audioerr.As(err)
	if !ok || coded.Code != audioerr.CodeSchema || coded.Field != "schema_version" {
		t.Fatalf("unexpected unknown-version error: %v", err)
	}
	_, err = codec.Load([]byte(`{"schema_version":1,"project":{}}`))
	coded, ok = audioerr.As(err)
	if !ok || coded.Field != "project.id" {
		t.Fatalf("unexpected missing-field error: %v", err)
	}
}

func TestDowngradeLossyAndRoundTrip(t *testing.T) {
	set, err := generator.Build(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	low, err := codec.Export(set.Project, 1)
	if err != nil {
		t.Fatal(err)
	}
	if low.Losses.Code != audioerr.CodeLossy || len(low.Losses.Items) == 0 {
		t.Fatalf("expected E_LOSSY items, got %+v", low.Losses)
	}
	loadedLow, err := codec.Load(low.Data)
	if err != nil {
		t.Fatal(err)
	}
	high, err := codec.Export(*loadedLow, 3)
	if err != nil {
		t.Fatal(err)
	}
	loadedHigh, err := codec.Load(high.Data)
	if err != nil {
		t.Fatal(err)
	}
	restored := make(map[string]model.Clip)
	for _, clip := range loadedHigh.Clips {
		restored[clip.ID] = clip
	}
	for _, clip := range set.Project.Clips {
		got := restored[clip.ID]
		if got.StartSample != clip.StartSample || got.EndSample != clip.EndSample ||
			got.GainQ16 != clip.GainQ16 || got.LoudnessWeightQ16 != clip.LoudnessWeightQ16 {
			t.Fatalf("round trip changed %s: got %+v want %+v", clip.ID, got, clip)
		}
	}
}

func TestV1UpgradeDefaultsAreTraced(t *testing.T) {
	set, err := generator.Build(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	project, err := codec.Load(set.Files["schema_v1.json"])
	if err != nil {
		t.Fatal(err)
	}
	foundCalibration := false
	for _, trace := range project.Defaults {
		if trace.Path == "project.calibration_enabled" {
			foundCalibration = true
			if trace.DefaultValue != true || trace.Source != "schema-v1.defaults" {
				t.Fatalf("bad default trace: %+v", trace)
			}
		}
	}
	if !foundCalibration {
		t.Fatal("missing calibration default trace")
	}
}
