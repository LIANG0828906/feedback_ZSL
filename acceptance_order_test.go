package dnslab_test

import (
	"testing"

	"dnslab/engine"
	"dnslab/generator"
	"dnslab/loader"
	"dnslab/spec"
)

func TestSubmissionOrderAndBatchesDoNotChangeResults(t *testing.T) {
	bundle, err := generator.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := loader.Load(bundle.Docs[spec.SchemaV2])
	if err != nil {
		t.Fatal(err)
	}

	orders := [][]int{
		{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
		{12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1, 0},
		{6, 0, 12, 3, 9, 1, 11, 4, 8, 2, 10, 5, 7},
	}
	batches := [][][2]int{
		{{0, 4}, {4, 9}, {9, 13}},
		{{0, 1}, {1, 7}, {7, 13}},
		{{0, 13}},
	}

	for orderIndex, order := range orders {
		eng := engine.NewEngine(loaded.Project)
		results := make([]engine.Result, len(bundle.Queries))
		for _, idx := range order {
			r := eng.ProcessQuery(bundle.Queries[idx])
			results[idx] = r
		}
		for i, want := range bundle.Truth {
			if !engine.CompareSemantic(results[i], want) {
				t.Fatalf("order %d query %d changed", orderIndex, i+1)
			}
		}
	}

	for batchIndex, ranges := range batches {
		eng := engine.NewEngine(loaded.Project)
		results := make([]engine.Result, len(bundle.Queries))
		for _, span := range ranges {
			for i := span[0]; i < span[1]; i++ {
				results[i] = eng.ProcessQuery(bundle.Queries[i])
			}
		}
		for i, want := range bundle.Truth {
			if !engine.CompareSemantic(results[i], want) {
				t.Fatalf("batch split %d query %d changed", batchIndex, i+1)
			}
		}
	}
}
