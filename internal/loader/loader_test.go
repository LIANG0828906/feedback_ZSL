package loader_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"warmcache/internal/domain"
	"warmcache/internal/gen"
	"warmcache/internal/loader"
)

func TestThreeSchemasNormalizeToSameProject(t *testing.T) {
	samples, err := gen.Samples(gen.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	var baseline domain.Project
	for index, sample := range samples {
		project, err := loader.Load(sample.Raw, sample.Version)
		if err != nil {
			t.Fatalf("%s: %v", sample.Version, err)
		}
		if index == 0 {
			baseline = project
			continue
		}
		if err := domain.MustEqualProjects(baseline, project); err != nil {
			t.Fatal(err)
		}
		if project.BudgetMilli != 200 || project.TickMS != 250 {
			t.Fatalf("unexpected normalized units: %+v", project)
		}
		for _, node := range project.Nodes {
			if node.LeaseTicks != 6 {
				t.Fatalf("%s lease normalization failed: %+v", sample.Version, node)
			}
		}
	}
}

func TestCommittedSamplesLoadAndMatch(t *testing.T) {
	var baseline domain.Project
	for index, version := range []domain.SchemaVersion{domain.SchemaV1, domain.SchemaV2, domain.SchemaV3} {
		path := filepath.Join("..", "..", "samples", string(version)+".json")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		project, err := loader.Load(raw, version)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if index == 0 {
			baseline = project
			continue
		}
		if err := domain.MustEqualProjects(baseline, project); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSchemaConflictNeedsWatermark(t *testing.T) {
	samples, err := gen.Samples(gen.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(samples[1].Raw, &raw); err != nil {
		t.Fatal(err)
	}
	clock := raw["clock"].(map[string]any)
	clock["leaseTicks"] = 9
	mutated, _ := json.Marshal(raw)
	_, err = loader.Load(mutated, domain.SchemaV2)
	if domain.AsBizError(err).Code != domain.CodeSchemaConflict {
		t.Fatalf("expected schema conflict, got %v", err)
	}
	raw["preheatWatermark"] = 1000
	mutated, _ = json.Marshal(raw)
	if _, err := loader.Load(mutated, domain.SchemaV2); err != nil {
		t.Fatalf("watermark should resolve ambiguity: %v", err)
	}
}

func TestDuplicateShardRejected(t *testing.T) {
	samples, _ := gen.Samples(gen.DefaultSeed)
	var raw map[string]any
	_ = json.Unmarshal(samples[0].Raw, &raw)
	shards := raw["shards"].([]any)
	raw["shards"] = append(shards, shards[0])
	mutated, _ := json.Marshal(raw)
	_, err := loader.Load(mutated, domain.SchemaV1)
	if domain.AsBizError(err).Code != domain.CodeDuplicateShard {
		t.Fatalf("expected duplicate shard code, got %v", err)
	}
}
