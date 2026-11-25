package thermalcache

import (
	"encoding/json"
	"testing"
)

func TestIncrementalEditsMatchFullRecompute(t *testing.T) {
	f := fixture42(t)
	p := loadVersion(t, f, SchemaV1)
	before, _ := json.Marshal(p)
	sim, err := NewSimulator(p)
	if err != nil {
		t.Fatal(err)
	}
	base, err := sim.Run()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		edit  Edit
		limit int64
	}{
		{
			name: "object access counts",
			edit: Edit{
				Kind: EditObjectAccess, ObjectID: "o03", AtMs: 200,
				Counts: []int64{2, 0, 0, 0, 0, 3, 0},
			},
			limit: 100,
		},
		{
			name: "layer capacity",
			edit: Edit{
				Kind: EditLayerCapacity, Tier: TierHot, AtMs: 300, CapacityBytes: 20000,
			},
			limit: 200,
		},
		{
			name: "append access event",
			edit: Edit{
				Kind:  EditAppendEvent,
				Event: AccessEvent{AtMs: 400, ObjectID: "o04", Count: 10},
			},
			limit: 300,
		},
		{
			name: "remove access event",
			edit: Edit{
				Kind: EditRemoveEvent, AtMs: 300, EventIndex: 0,
				Event: AccessEvent{AtMs: 300, ObjectID: "o08"},
			},
			limit: 200,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sim.Apply(tc.edit)
			if err != nil {
				t.Fatal(err)
			}
			if !got.FullMatch {
				t.Fatal("incremental result differs from full recompute")
			}
			if len(got.Invalidations) != 1 {
				t.Fatalf("expected one invalidation, got %d", len(got.Invalidations))
			}
			inv := got.Invalidations[0]
			if len(got.ReusedTicks) == 0 || got.ReusedTicks[len(got.ReusedTicks)-1] > tc.limit {
				t.Fatalf("cached prefix was not reused up to %d: %v", tc.limit, got.ReusedTicks)
			}
			if len(got.RecomputedTicks) == 0 {
				t.Fatal("no dependent suffix was recomputed")
			}
			if len(inv.Closure) == 0 {
				t.Fatal("invalidation closure is empty")
			}
			if got.Result.ProjectID != base.ProjectID || len(got.Result.Ticks) != len(base.Ticks) {
				t.Fatal("incremental result lost project tick identity")
			}
		})
	}
	after, _ := json.Marshal(p)
	if string(before) != string(after) {
		t.Fatal("incremental edits modified the source project")
	}
}
