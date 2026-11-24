package pipeline_test

import (
	"bytes"
	"testing"

	"virtualstage/pipeline"
	"virtualstage/sample"
)

func TestExportRoundTripAndDeterminism(t *testing.T) {
	work := t.TempDir()
	result, err := sample.Generate(sample.Options{OutDir: work, Seed: 21})
	if err != nil {
		t.Fatal(err)
	}
	model, _, err := pipeline.LoadProject(result.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	character, _ := model.Layer("character")
	character.ParentID = "background"
	if err := model.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, version := range []int{1, 2, 3} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			first, err := pipeline.ExportModel(model, version)
			if err != nil {
				t.Fatal(err)
			}
			second, err := pipeline.ExportModel(model, version)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first.LayerDocument, second.LayerDocument) || first.Report.ContentHash != second.Report.ContentHash {
				t.Fatal("repeat export is not byte deterministic")
			}
			if first.Report.Warnings == nil {
				t.Fatal("warnings must serialize as an array, not null")
			}
			path := work + "/export-" + first.Report.LayerFile
			if err := writeTestFile(path, first.LayerDocument); err != nil {
				t.Fatal(err)
			}
			reloaded, err := pipeline.LoadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(reloaded.Stack) != len(model.Stack) {
				t.Fatalf("stack length %d != %d", len(reloaded.Stack), len(model.Stack))
			}
			for i := range model.Stack {
				if reloaded.Stack[i] != model.Stack[i] {
					t.Fatalf("stack[%d] = %q, want %q", i, reloaded.Stack[i], model.Stack[i])
				}
			}
			for _, layer := range model.Layers {
				other, ok := reloaded.Layer(layer.ID)
				if !ok || other.Visible != layer.Visible || other.VisibleUntilFrame != layer.VisibleUntilFrame {
					t.Fatalf("visibility not recovered for %s", layer.ID)
				}
			}
			if !reloaded.TimeBase.Seconds().Equal(model.TimeBase.Seconds()) {
				t.Fatalf("time base not recovered: %v vs %v", reloaded.TimeBase.Seconds(), model.TimeBase.Seconds())
			}
			if version == 3 && (reloaded.TimeBase.TickRate != 24000 || reloaded.TimeBase.TicksPerFrame != 1000) {
				t.Fatalf("v3 tick timing = %d/%d, want 1000/24000", reloaded.TimeBase.TicksPerFrame, reloaded.TimeBase.TickRate)
			}
			if version < 3 {
				found := false
				for _, warning := range first.Report.Warnings {
					if warning.Code == pipeline.CodeLossy && warning.Property == "hierarchy" {
						found = true
					}
				}
				if version == 1 && !found {
					t.Fatalf("v1 export missing hierarchy E_LOSSY: %+v", first.Report.Warnings)
				}
			}
		})
	}
}

func TestUnknownTargetVersion(t *testing.T) {
	model := testModel()
	_, err := pipeline.ExportModel(model, 4)
	assertCode(t, err, pipeline.CodeVersion)
}
