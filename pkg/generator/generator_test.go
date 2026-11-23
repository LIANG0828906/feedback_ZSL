package generator_test

import (
	"testing"

	"stageworkbench/pkg/generator"
	"stageworkbench/pkg/stage"
)

func TestMultiScaleGeneration(t *testing.T) {
	for _, presetName := range []string{"tiny", "small", "medium"} {
		t.Run(presetName, func(t *testing.T) {
			project, err := generator.Generate(generator.Options{Preset: presetName, Seed: 77})
			if err != nil {
				t.Fatal(err)
			}
			if len(project.Cues) < 48 {
				t.Fatalf("unexpected cue count %d", len(project.Cues))
			}
			if len(project.Truth.Samples) == 0 {
				t.Fatal("generator did not record truth samples")
			}
			if err := stage.ValidateProject(project, stage.DefaultValidateOptions()); err != nil {
				t.Fatalf("validate: %v", err)
			}
		})
	}
}

func TestLargePresetSelectsMergedMode(t *testing.T) {
	if testing.Short() {
		t.Skip("large deterministic sample skipped in short mode")
	}
	project, err := generator.Generate(generator.Options{Preset: "large", Seed: 88})
	if err != nil {
		t.Fatal(err)
	}
	if len(project.Cues) < 30_000 {
		t.Fatalf("large preset generated only %d cues", len(project.Cues))
	}
	decision := stage.SelectMode(project, stage.DefaultParams())
	if decision.Mode != stage.ModeMerged {
		t.Fatalf("large preset should choose merged mode: %+v", decision)
	}
	if decision.CueCount != len(project.Cues) || decision.LightCount != len(project.Lights) {
		t.Fatalf("mode decision did not use actual scale: %+v", decision)
	}
}
