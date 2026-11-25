package scenario_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"lipsync/arbitration"
	"lipsync/domain"
	"lipsync/formats"
	"lipsync/scenario"
)

func TestGeneratedTruthMatchesArbitration(t *testing.T) {
	artifacts, err := scenario.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	got, err := arbitration.Run(artifacts.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(artifacts.Truth, *got) {
		wantJSON, _ := json.MarshalIndent(artifacts.Truth, "", "  ")
		gotJSON, _ := json.MarshalIndent(*got, "", "  ")
		t.Fatalf("truth mismatch\nwant=%s\ngot=%s", wantJSON, gotJSON)
	}
	if len(artifacts.Truth.Preemptions) == 0 || len(artifacts.Truth.Resumes) == 0 || len(artifacts.Truth.Abandonments) == 0 {
		t.Fatalf("scenario missing expected events: preempt=%d resume=%d abandon=%d",
			len(artifacts.Truth.Preemptions), len(artifacts.Truth.Resumes), len(artifacts.Truth.Abandonments))
	}
}

func TestWriteArtifacts(t *testing.T) {
	artifacts, err := scenario.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := artifacts.WriteArtifacts(dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"project.v1.json", "project.v2.json", "project.v3.json", "truth.json", "loss-reports.json",
	} {
		if info, err := os.Stat(filepath.Join(dir, name)); err != nil || info.Size() == 0 {
			t.Fatalf("artifact %s missing or empty: %v", name, err)
		}
	}
}

func TestThreeVersionsReadBackCanonicalSemantics(t *testing.T) {
	artifacts, err := scenario.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range domain.SupportedVersions {
		loaded, err := formats.Load(artifacts.Documents[version])
		if err != nil {
			t.Fatalf("%s: %v", version, err)
		}
		if loaded.ContentHash != artifacts.Canonical.ContentHash {
			t.Fatalf("%s content hash mismatch: %s != %s", version, loaded.ContentHash, artifacts.Canonical.ContentHash)
		}
		if !reflect.DeepEqual(channelSemantics(loaded.Channels), channelSemantics(artifacts.Canonical.Channels)) {
			t.Fatalf("%s channel semantics differ", version)
		}
		if !reflect.DeepEqual(loaded.ArbitrationOrder, artifacts.Canonical.ArbitrationOrder) {
			t.Fatalf("%s order differs", version)
		}
	}
}

func channelSemantics(channels []domain.Channel) map[string]struct {
	Slots  int
	Budget int64
} {
	out := map[string]struct {
		Slots  int
		Budget int64
	}{}
	for _, channel := range channels {
		out[channel.ID] = struct {
			Slots  int
			Budget int64
		}{Slots: channel.SlotLimit, Budget: channel.FrameBudgetBasisPoints}
	}
	return out
}
