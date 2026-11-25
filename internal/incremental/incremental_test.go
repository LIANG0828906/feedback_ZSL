package incremental_test

import (
	"context"
	"encoding/json"
	"testing"

	"featurekernel/internal/engine"
	"featurekernel/internal/gen"
	"featurekernel/internal/incremental"
	"featurekernel/internal/kernel"
)

func TestEventUpdateRecomputesOnlyDependencyClosure(t *testing.T) {
	project, err := gen.BuildProject(gen.DefaultSeed, 0)
	if err != nil {
		t.Fatal(err)
	}
	cache, _, err := incremental.NewCache(project)
	if err != nil {
		t.Fatal(err)
	}
	before := mustJSON(t, project)

	newValue := 12345.0
	report, err := cache.Apply(incremental.Edit{
		UpdateEvents: map[int64]incremental.EventPatch{
			1: {Value: &newValue},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	after := mustJSON(t, project)
	if before != after {
		t.Fatalf("source project was modified by incremental edit")
	}

	full, err := engine.New(cache.Project())
	if err != nil {
		t.Fatal(err)
	}
	want, err := full.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := kernel.CompareWindowResults(want.Windows, report.ResultSet.Windows, project.FixedTolerance); err != nil {
		t.Fatal(err)
	}
	if !contains(report.RecomputedKeys, "window:latency_ms:0") {
		t.Fatalf("latency window 0 should be recomputed: %v", report.RecomputedKeys)
	}
	if len(report.ReusedKeys) == 0 {
		t.Fatalf("unaffected windows should be reused")
	}
	var sawReused bool
	var sawDerivedClosure bool
	for _, window := range report.ResultSet.Windows {
		if window.Invalidation != nil && window.Invalidation.Status == kernel.ProvenanceReused {
			sawReused = true
		}
		if window.FeatureID == "latency_ms" && window.WindowIndex == 0 && window.Invalidation != nil {
			for _, closureKey := range window.Invalidation.PropagationClosure {
				if len(closureKey) > 8 && closureKey[:8] == "derived:" {
					sawDerivedClosure = true
				}
			}
		}
	}
	if !sawReused {
		t.Fatalf("no window provenance marked reused")
	}
	if !sawDerivedClosure {
		t.Fatalf("edited window closure did not include derived result")
	}
}

func TestWidthAggregationAppendAndDeleteMatchFull(t *testing.T) {
	cases := []struct {
		name string
		edit incremental.Edit
	}{
		{"width", incremental.Edit{ChangeWidth: map[string]int64{"jitter_ms": 120_000}}},
		{"aggregations", incremental.Edit{ChangeAggregations: map[string][]kernel.AggFunc{
			"latency_ms": {kernel.AggCount, kernel.AggSum},
		}}},
		{"append", incremental.Edit{AppendEvents: []kernel.Event{{
			FeatureID: "amount_cent", TimeMs: 1704067200000 + 15*60_000, Channel: "web", Value: 4242,
		}}}},
		{"delete", incremental.Edit{DeleteEvents: []int64{2}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			project, err := gen.BuildProject(gen.DefaultSeed, 2)
			if err != nil {
				t.Fatal(err)
			}
			cache, _, err := incremental.NewCache(project)
			if err != nil {
				t.Fatal(err)
			}
			report, err := cache.Apply(tc.edit)
			if err != nil {
				t.Fatal(err)
			}
			full, err := engine.New(cache.Project())
			if err != nil {
				t.Fatal(err)
			}
			want, err := full.Run(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := kernel.CompareWindowResults(want.Windows, report.ResultSet.Windows, project.FixedTolerance); err != nil {
				t.Fatal(err)
			}
			if len(report.RecomputedKeys) == 0 {
				t.Fatalf("expected recomputation")
			}
		})
	}
}

func TestInvalidWidthReturnsStableCodeWithoutMutatingSource(t *testing.T) {
	project, err := gen.BuildProject(gen.DefaultSeed, 0)
	if err != nil {
		t.Fatal(err)
	}
	before := mustJSON(t, project)
	cache, _, err := incremental.NewCache(project)
	if err != nil {
		t.Fatal(err)
	}
	_, err = cache.Apply(incremental.Edit{ChangeWidth: map[string]int64{"latency_ms": -60_000}})
	if code := kernel.CodeOf(err); code != kernel.CodeDefinitionNegativeWidth {
		t.Fatalf("code=%s err=%v", code, err)
	}
	after := mustJSON(t, project)
	if before != after {
		t.Fatalf("source project changed after invalid edit")
	}
}

func TestZeroStepAndWeightOutOfRangeCodes(t *testing.T) {
	project, err := gen.BuildProject(gen.DefaultSeed, 0)
	if err != nil {
		t.Fatal(err)
	}
	project.Features[0].SlideMs = 0
	if _, err := engine.New(project); kernel.CodeOf(err) != kernel.CodeDefinitionZeroStep {
		t.Fatalf("zero step err=%v", err)
	}

	project, err = gen.BuildProject(gen.DefaultSeed, 0)
	if err != nil {
		t.Fatal(err)
	}
	project.Features[0].ChannelWeightsPPM["web"] = 2_000_000
	if _, err := engine.New(project); kernel.CodeOf(err) != kernel.CodeAggWeightOutOfRange {
		t.Fatalf("weight err=%v", err)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
