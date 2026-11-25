package formats_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"lipsync/arbitration"
	"lipsync/domain"
	"lipsync/formats"
	"lipsync/scenario"
)

func generated(t *testing.T) *scenario.Artifacts {
	t.Helper()
	artifacts, err := scenario.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	return artifacts
}

func documentMap(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestSchemaErrorsCarryPathAndChannel(t *testing.T) {
	artifacts := generated(t)
	unknown := documentMap(t, artifacts.Documents["v3"])
	unknown["schema_version"] = "v9"
	data, _ := json.Marshal(unknown)
	_, err := formats.Load(data)
	if domain.CodeOf(err) != domain.CodeSchema || !strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("unknown version did not return E_SCHEMA path: %v", err)
	}

	missing := documentMap(t, artifacts.Documents["v1"])
	delete(missing, "channels")
	data, _ = json.Marshal(missing)
	_, err = formats.Load(data)
	if domain.CodeOf(err) != domain.CodeSchema || !strings.Contains(err.Error(), "channels") {
		t.Fatalf("missing field did not return E_SCHEMA path: %v", err)
	}

	negative := documentMap(t, artifacts.Documents["v1"])
	channels := negative["channels"].([]any)
	channels[0].(map[string]any)["slot_limit"] = -1
	data, _ = json.Marshal(negative)
	_, err = formats.Load(data)
	if domain.CodeOf(err) != domain.CodeSchema || !strings.Contains(err.Error(), "slot_limit") ||
		!strings.Contains(err.Error(), "channel=spk_main") {
		t.Fatalf("negative slot did not identify path/channel: %v", err)
	}
}

func TestUpgradeDefaultFillsAreAuditable(t *testing.T) {
	artifacts := generated(t)
	v1, err := formats.Load(artifacts.Documents["v1"])
	if err != nil {
		t.Fatal(err)
	}
	if !hasFill(v1.DefaultFills, "preemption.strategy", "protocol:v1-default") ||
		!hasFill(v1.DefaultFills, "preemption.recovery_offset_frames", "protocol:v1-default") {
		t.Fatalf("v1 upgrade did not record recovery defaults: %+v", v1.DefaultFills)
	}
	if v1.SchemaVersion != "v1" {
		t.Fatalf("source version not retained: %s", v1.SchemaVersion)
	}
	v2, err := formats.Load(artifacts.Documents["v2"])
	if err != nil {
		t.Fatal(err)
	}
	if !hasFill(v2.DefaultFills, "preemption.recovery_offset_frames", "protocol:v2-default") ||
		!hasFill(v2.DefaultFills, "preemption.allow_abandon", "protocol:v2-default") {
		t.Fatalf("v2 partial upgrade did not record defaults: %+v", v2.DefaultFills)
	}
	v3, err := formats.Load(artifacts.Documents["v3"])
	if err != nil {
		t.Fatal(err)
	}
	if len(v3.DefaultFills) != 0 {
		t.Fatalf("complete v3 unexpectedly filled defaults: %+v", v3.DefaultFills)
	}
}

func TestLossyExportAndReadbackBudgetResidual(t *testing.T) {
	artifacts := generated(t)
	v1Data, report, err := formats.Export(artifacts.Canonical, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if report.Code != domain.CodeLossy || !hasLoss(report, "scene_tree") || !hasLoss(report, "preemption_policy") {
		t.Fatalf("v1 export lacks explicit loss report: %+v", report)
	}
	v1, err := formats.Load(v1Data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(v1.ArbitrationOrder, artifacts.Canonical.ArbitrationOrder) {
		t.Fatal("v1 round-trip changed arbitration order")
	}
	canonicalChannels := map[string]domain.Channel{}
	for _, channel := range artifacts.Canonical.Channels {
		canonicalChannels[channel.ID] = channel
	}
	for _, channel := range v1.Channels {
		original := canonicalChannels[channel.ID]
		if channel.SlotLimit != original.SlotLimit {
			t.Fatalf("%s slot changed", channel.ID)
		}
		residual := original.FrameBudgetBasisPoints - channel.FrameBudgetBasisPoints
		if residual < 0 || residual > 9 {
			t.Fatalf("%s budget residual %d exceeds declared permille precision", channel.ID, residual)
		}
	}

	lossyProject := artifacts.Canonical
	lossyProject.Recovery.RecoveryOffsetFrame = 3
	lossyProject.Recovery.AllowAbandon = false
	v2Data, v2Report, err := formats.Export(lossyProject, "v2")
	if err != nil {
		t.Fatal(err)
	}
	if !hasLoss(v2Report, "recovery_offset") || !hasLoss(v2Report, "allow_abandon") {
		t.Fatalf("v2 missing-field losses not reported: %+v", v2Report)
	}
	v2, err := formats.Load(v2Data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(v2.ArbitrationOrder, artifacts.Canonical.ArbitrationOrder) {
		t.Fatal("v2 round-trip changed order")
	}
}

func TestEveryVersionProducesSameArbitrationTruth(t *testing.T) {
	artifacts := generated(t)
	projects, err := artifacts.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	for version, project := range projects {
		result, err := arbitration.Run(*project)
		if err != nil {
			t.Fatalf("%s: %v", version, err)
		}
		if !reflect.DeepEqual(*result, artifacts.Truth) {
			t.Fatalf("%s arbitration differs from generator truth", version)
		}
	}
}

func hasFill(fills []domain.DefaultFill, path, source string) bool {
	for _, fill := range fills {
		if fill.Path == path && fill.Source == source && fill.OldValue == nil {
			return true
		}
	}
	return false
}

func hasLoss(report *formats.LossReport, kind string) bool {
	for _, item := range report.Items {
		if item.Kind == kind {
			return true
		}
	}
	return false
}
