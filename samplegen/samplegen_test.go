package samplegen_test

import (
	"testing"

	"skillbudget"
	"skillbudget/samplegen"
)

func TestGeneratedTruthMatchesAllVersions(t *testing.T) {
	for _, seed := range []int64{42, 7, 20261125} {
		bundle, err := samplegen.Generate(seed)
		if err != nil {
			t.Fatalf("seed %d: generate: %v", seed, err)
		}
		for version, data := range bundle.Versions {
			project, err := skillcore.LoadVersion(data, version)
			if err != nil {
				t.Fatalf("seed %d version %d: load: %v", seed, version, err)
			}
			if project.ContentHash() != bundle.RuleHash {
				t.Fatalf("seed %d version %d: semantic hash mismatch", seed, version)
			}
			engine, err := skillcore.NewEngine(project)
			if err != nil {
				t.Fatal(err)
			}
			result, err := engine.Run(nil, bundle.Intents, bundle.TotalTicks, skillcore.SimOptions{
				SampleEvery: 1, Workers: 3, BufferLimit: 2,
			})
			if err != nil {
				t.Fatalf("seed %d version %d: run: %v", seed, version, err)
			}
			if mismatch := skillcore.CompareSnapshots(bundle.Truth, result.Ticks); mismatch != nil {
				t.Fatalf("seed %d version %d: %v", seed, version, mismatch)
			}
		}
	}
}
