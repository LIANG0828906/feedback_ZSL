package generator_test

import (
	"testing"

	"qualitybench/internal/generator"
	"qualitybench/internal/model"
)

func TestGenerationIsDeterministic(t *testing.T) {
	opts := generator.DefaultOptions()
	opts.ImageCount = 2
	opts.Seed = 77123
	a, err := generator.Generate(opts)
	if err != nil {
		t.Fatal(err)
	}
	b, err := generator.Generate(opts)
	if err != nil {
		t.Fatal(err)
	}
	if a.ProjectFP != b.ProjectFP || a.TruthFP != b.TruthFP {
		t.Fatalf("fingerprints differ: %s/%s vs %s/%s",
			a.ProjectFP, a.TruthFP, b.ProjectFP, b.TruthFP)
	}
	for id, frame := range a.Baseline {
		if frame.Hash() != b.Baseline[id].Hash() {
			t.Fatalf("baseline %s differs", id)
		}
	}
	for id, refs := range a.Reference {
		for i := range refs {
			if refs[i].Hash() != b.Reference[id][i].Hash() {
				t.Fatalf("reference %s step %d differs", id, i)
			}
		}
	}
}

func TestTruthHasDiverseAttributionAndSamples(t *testing.T) {
	opts := generator.DefaultOptions()
	ds, err := generator.Generate(opts)
	if err != nil {
		t.Fatal(err)
	}
	categories := make(map[string]bool)
	for _, image := range ds.Truth.Images {
		if len(image.Samples) < 3 {
			t.Fatalf("image %s has too few samples", image.ID)
		}
		for _, block := range image.Blocks {
			categories[block.Attribution.Category] = true
			if !model.IntervalContains(block.Metrics.NoiseVariance, block.Metrics.NoiseVariance[0]) {
				t.Fatal("invalid interval")
			}
		}
	}
	if len(categories) < 2 {
		t.Fatalf("expected multiple attribution categories, got %v", categories)
	}
}
