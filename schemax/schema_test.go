package schemax_test

import (
	"encoding/json"
	"strings"
	"testing"

	"iconflow/errs"
	"iconflow/generator"
	"iconflow/model"
	"iconflow/schemax"
)

func TestThreeVersionsLoadAndPreserveSource(t *testing.T) {
	project := generator.GenerateProject(generator.FixedSeed)
	for _, version := range []string{model.SchemaV1, model.SchemaV2, model.SchemaV3} {
		export, exportErr := schemax.Export(project, version)
		if exportErr != nil {
			t.Fatalf("export %s: %v", version, exportErr)
		}
		loaded, loadErr := schemax.LoadDocument(export.Data)
		if loadErr != nil {
			t.Fatalf("load %s: %v", version, loadErr)
		}
		if loaded.Project.SourceVersion != version {
			t.Fatalf("source version = %q want %q", loaded.Project.SourceVersion, version)
		}
		if loaded.Project.ThresholdPermyriad != project.ThresholdPermyriad {
			t.Fatalf("threshold mismatch: %d", loaded.Project.ThresholdPermyriad)
		}
		if len(loaded.Project.Images) != len(project.Images) {
			t.Fatalf("image count mismatch")
		}
		if len(loaded.Project.SortedBlockCoords()) != len(project.SortedBlockCoords()) {
			t.Fatalf("block count mismatch")
		}
		if diffs := schemax.SemanticDifferences(project, loaded.Project); len(diffs) > 0 {
			t.Fatalf("%s readback semantic differences: %v", version, diffs)
		}
	}
}

func TestV1LossReportAndReadable(t *testing.T) {
	project := generator.GenerateProject(generator.FixedSeed)
	export, err := schemax.Export(project, model.SchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	if export.Loss == nil || len(export.Loss.Items) < 2 {
		t.Fatalf("expected grouped layout and color tolerance loss, got %+v", export.Loss)
	}
	fields := []string{}
	for _, item := range export.Loss.Items {
		if item.Code != errs.ELossy {
			t.Fatalf("loss code = %s", item.Code)
		}
		fields = append(fields, item.Field)
		if item.Details["disposition"] == "" {
			t.Fatalf("loss disposition missing: %+v", item)
		}
	}
	loaded, loadErr := schemax.LoadDocument(export.Data)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if loaded.Project.ChannelLayout != model.LayoutFlat {
		t.Fatalf("v1 readback layout = %s", loaded.Project.ChannelLayout)
	}
	if loaded.Project.ColorTolerance != nil {
		t.Fatal("color tolerance should be dropped in v1")
	}
	if diffs := schemax.SemanticDifferences(project, loaded.Project); len(diffs) > 0 {
		t.Fatalf("lossy v1 readback should remain semantically equivalent: %v", diffs)
	}
}

func TestMissingDefaultsProduceUpgradeRecords(t *testing.T) {
	project := generator.GenerateProject(generator.FixedSeed)
	export, _ := schemax.Export(project, model.SchemaV1)
	var doc map[string]any
	if err := json.Unmarshal(export.Data, &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc, "channel_sequence")
	delete(doc, "quant_step")
	data, _ := json.Marshal(doc)
	loaded, err := schemax.LoadDocument(data)
	if err != nil {
		t.Fatal(err)
	}
	var foundChannel, foundQuant bool
	for _, record := range loaded.UpgradeRecords {
		if strings.HasPrefix(record.Field, "$.channel") {
			foundChannel = true
		}
		if record.Field == "$.quant_step" {
			foundQuant = true
		}
	}
	if !foundChannel || !foundQuant {
		t.Fatalf("missing upgrade records channel=%v quant=%v: %+v", foundChannel, foundQuant, loaded.UpgradeRecords)
	}
}

func TestSchemaErrorsPointAtFieldPath(t *testing.T) {
	_, err := schemax.LoadDocument([]byte(`{"schema":"nope"}`))
	if err == nil || err.Code != errs.ESchema || err.Field != "$.schema" {
		t.Fatalf("unexpected unknown schema error: %+v", err)
	}
	_, err = schemax.LoadDocument([]byte(`{"schema":"icon-project.v1","seed":1}`))
	if err == nil || err.Code != errs.ESchema || err.Field != "$.project_id" {
		t.Fatalf("unexpected missing field error: %+v", err)
	}
}
