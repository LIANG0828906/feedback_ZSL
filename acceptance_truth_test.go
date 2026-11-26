package dnslab_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"dnslab/engine"
	"dnslab/generator"
	"dnslab/loader"
)

func TestThreeSchemaEquivalenceAndTruth(t *testing.T) {
	bundle, err := generator.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	for _, schema := range []string{"dns-project/v1-flat-literal-seconds", "dns-project/v2-flat-list-millis", "dns-project/v3-tree-absolute-negative"} {
		t.Run(schema, func(t *testing.T) {
			data := bundle.Docs[schema]
			loaded, err := loader.Load(data)
			if err != nil {
				t.Fatalf("load %s: %v", schema, err)
			}
			if !reflect.DeepEqual(loaded.Project, bundle.Project) {
				wantJSON, _ := json.MarshalIndent(bundle.Project, "", "  ")
				gotJSON, _ := json.MarshalIndent(loaded.Project, "", "  ")
				t.Fatalf("canonical project mismatch\nwant:\n%s\ngot:\n%s", wantJSON, gotJSON)
			}

			eng := engine.NewEngine(loaded.Project)
			report, err := engine.RunSamples(context.Background(), eng, bundle.Queries, loaded.Project.WorkerBudget)
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Results) != len(bundle.Truth) {
				t.Fatalf("got %d results, want %d", len(report.Results), len(bundle.Truth))
			}
			for i, want := range bundle.Truth {
				got := report.Results[i]
				if !engine.CompareSemantic(got, want) {
					gotJSON, _ := json.MarshalIndent(got, "", "  ")
					wantJSON, _ := json.MarshalIndent(want, "", "  ")
					t.Fatalf("query %d/%s mismatch\ngot:\n%s\nwant:\n%s", i+1, want.QueryID, gotJSON, wantJSON)
				}
			}
		})
	}
}
