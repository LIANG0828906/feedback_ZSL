package gateway

import (
	"testing"
)

func TestIncrementalEditsMatchFullRecompute(t *testing.T) {
	bundle := testBundle(t)
	originalJSON := CanonicalJSON(bundle.Canonical)

	added := bundle.Canonical.Records[10]
	added.ID = "rec-added-incremental"
	added.ArrivalSeq = 7
	added.EventTimeMS = bundle.Canonical.TimeOriginMS + 23_000
	added.SourceEventSeq = nil

	updated := bundle.Canonical.Records[15]
	updated.ResponseBytes += 777
	updated.StatusCode = 418

	edits := []struct {
		name string
		edit Edit
	}{
		{
			name: "error_rule",
			edit: Edit{Kind: EditErrorRule, RouteID: "route-auth-login", ErrorRules: []ErrorRule{
				{Class: "reclassified", StatusMin: 100, StatusMax: 599},
			}},
		},
		{name: "add_record", edit: Edit{Kind: EditAddRecord, Record: &added}},
		{name: "update_record", edit: Edit{Kind: EditUpdateRecord, Record: &updated}},
		{name: "delete_record", edit: Edit{Kind: EditDeleteRecord, RecordID: "rec-010"}},
		{name: "allowed_lateness", edit: Edit{Kind: EditLateness, AllowedLatenessMS: 100}},
		{name: "window_width", edit: Edit{Kind: EditWindowConfig, WidthMS: 12_000, SlideMS: 4_000}},
	}

	for _, tc := range edits {
		t.Run(tc.name, func(t *testing.T) {
			source := bundle.Canonical.Clone()
			ie, err := NewIncrementalEngine(source)
			if err != nil {
				t.Fatal(err)
			}
			result, err := ie.Apply(tc.edit)
			if err != nil {
				t.Fatalf("Apply(%s): %v", tc.name, err)
			}
			expected := mustRun(t, ie.Project())
			assertAggregatesMatch(t, expected.Windows, result.Report.Windows, 0)
			if !CompareBatches(expected.Batches, result.Report.Batches) {
				t.Fatalf("%s incremental batches differ from full recompute", tc.name)
			}
			if CanonicalJSON(expected.Dropped) != CanonicalJSON(result.Report.Dropped) {
				t.Fatalf("%s drops differ\nexpected %s\nactual   %s", tc.name, CanonicalJSON(expected.Dropped), CanonicalJSON(result.Report.Dropped))
			}
			if CanonicalJSON(expected.RecomputedKeys) != CanonicalJSON(result.Report.RecomputedKeys) {
				t.Fatalf("%s recomputed keys differ\nexpected %v\nactual   %v", tc.name, expected.RecomputedKeys, result.Report.RecomputedKeys)
			}
			if len(result.Dependency.Sources) == 0 || len(result.Dependency.PropagationEdges) == 0 {
				t.Fatalf("%s missing dependency provenance: %+v", tc.name, result.Dependency)
			}
			if tc.name == "error_rule" && len(result.Dependency.ReusedWindows) == 0 {
				t.Fatal("error rule edit should reuse at least some unrelated windows")
			}
			if tc.name == "window_width" && len(result.Dependency.ReusedWindows) != 0 {
				t.Fatalf("window width edit should invalidate all windows, reused %v", result.Dependency.ReusedWindows)
			}
		})
	}

	if CanonicalJSON(bundle.Canonical) != originalJSON {
		t.Fatal("incremental test mutated shared source project")
	}
}

func TestIncrementalPropagationClosureIncludesParentAndBatch(t *testing.T) {
	bundle := testBundle(t)
	ie, err := NewIncrementalEngine(bundle.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	result, err := ie.Apply(Edit{Kind: EditErrorRule, RouteID: "route-billing-charges", ErrorRules: []ErrorRule{
		{Class: "changed", StatusMin: 200, StatusMax: 599},
	}})
	if err != nil {
		t.Fatal(err)
	}
	hasServiceParent := false
	for _, key := range result.Dependency.InvalidWindowKeys {
		parsed, err := ParseWindowKey(key)
		if err != nil {
			t.Fatal(err)
		}
		if parsed.Level == "service" && parsed.ServiceID == "svc-billing" {
			hasServiceParent = true
		}
	}
	if !hasServiceParent {
		t.Fatalf("closure lacks service parent: %v", result.Dependency.InvalidWindowKeys)
	}
	if len(result.Dependency.BatchClosure) == 0 {
		t.Fatal("expected export batch propagation closure")
	}
}
