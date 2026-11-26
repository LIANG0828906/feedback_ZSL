package dnslab_test

import (
	"context"
	"testing"

	"dnslab/editor"
	"dnslab/engine"
	"dnslab/generator"
	"dnslab/loader"
	"dnslab/spec"
)

func TestUpstreamAndNegativeIncrementalEdits(t *testing.T) {
	bundle, err := generator.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := loader.Load(bundle.Docs[spec.SchemaV3])
	if err != nil {
		t.Fatal(err)
	}

	edits := []editor.Edit{
		{Kind: "upstream_timeout", UpstreamIndex: 2, TimeoutMS: 3000},
		{Kind: "negative_enabled", Enabled: boolPtr(false)},
	}
	for _, edit := range edits {
		next, err := editor.Apply(loaded.Project, edit)
		if err != nil {
			t.Fatalf("apply %+v: %v", edit, err)
		}
		old := engine.NewEngine(loaded.Project)
		reconciled, report := engine.Reconcile(old, next, spec.SchemaV3, spec.SchemaV3, nil)
		if len(report.Sources) == 0 {
			t.Fatalf("edit %+v produced no invalidation source", edit)
		}
		inc, err := engine.RunSamples(context.Background(), reconciled, bundle.Queries, 1)
		if err != nil {
			t.Fatal(err)
		}
		full, err := engine.RunSamples(context.Background(), engine.NewEngine(next), bundle.Queries, 1)
		if err != nil {
			t.Fatal(err)
		}
		for i := range inc.Results {
			want := spec.Truth{
				Code: full.Results[i].Code, Matched: full.Results[i].Matched,
				RuleNumber: full.Results[i].RuleNumber, DecisionBasis: full.Results[i].DecisionBasis,
				CandidateRules: full.Results[i].CandidateRules, UpstreamIndex: full.Results[i].UpstreamIndex,
				HopCount: full.Results[i].HopCount, Source: full.Results[i].Source,
				Negative: full.Results[i].Negative, Records: full.Results[i].Records,
				RemainingTTLMS:   full.Results[i].RemainingTTLMS,
				EntryRemainingMS: full.Results[i].EntryRemainingMS,
				VirtualElapsedMS: full.Results[i].VirtualElapsedMS,
			}
			if !engine.CompareSemantic(inc.Results[i], want) {
				t.Fatalf("edit %s query %d differs from full recompute", edit.Kind, i+1)
			}
		}
	}
}

func boolPtr(v bool) *bool { return &v }
