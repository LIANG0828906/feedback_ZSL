package engine_test

import (
	"testing"

	"presentkernel/internal/engine"
	"presentkernel/internal/generator"
)

func TestPreciseInvalidationAndCacheKeys(t *testing.T) {
	set, err := generator.GenerateSampleSet()
	if err != nil {
		t.Fatal(err)
	}
	p := set.Canonical
	before := map[string]string{}
	for di := range p.Dialogues {
		for si := range p.Dialogues[di].Segments {
			pid := p.Dialogues[di].Segments[si].ID
			_, _, key := engine.PartitionCacheKey(p, di, si, p.TargetFPS)
			before[pid] = key
		}
	}

	graph := engine.BuildGraph(p, p.TargetFPS)
	weightEdit := engine.Edit{Kind: "char_weight", DialogueID: "D0", PartitionIndex: 0, CharIndex: 1, Value: 1500}
	plan := graph.PlanEdit(p, weightEdit)
	if len(plan.Invalid) != 1 || plan.Invalid[0].PartitionID != "D0-P0" || len(plan.Reused) != 5 {
		t.Fatalf("weight plan invalid=%+v reused=%d", plan.Invalid, len(plan.Reused))
	}
	if err := engine.ApplyEdit(p, weightEdit); err != nil {
		t.Fatal(err)
	}
	for di := range p.Dialogues {
		for si := range p.Dialogues[di].Segments {
			pid := p.Dialogues[di].Segments[si].ID
			_, _, key := engine.PartitionCacheKey(p, di, si, p.TargetFPS)
			if pid == "D0-P0" {
				if key == before[pid] {
					t.Fatal("edited partition cache key did not change")
				}
			} else if key != before[pid] {
				t.Fatalf("unexpected cache key change for unaffected %s", pid)
			}
		}
	}

	durationEdit := engine.Edit{Kind: "segment_duration", DialogueID: "D1", PartitionIndex: 1, Value: 48}
	graph = engine.BuildGraph(p, p.TargetFPS)
	plan = graph.PlanEdit(p, durationEdit)
	if len(plan.Invalid) != 1 || plan.Invalid[0].PartitionID != "D1-P1" {
		t.Fatalf("duration plan = %+v", plan.Invalid)
	}
	if err := engine.ApplyEdit(p, durationEdit); err != nil {
		t.Fatal(err)
	}

	fpsEdit := engine.Edit{Kind: "target_fps", Value: 48}
	graph = engine.BuildGraph(p, p.TargetFPS)
	plan = graph.PlanEdit(p, fpsEdit)
	if len(plan.Invalid) != 6 || len(plan.Reused) != 0 {
		t.Fatalf("fps should invalidate all partitions, invalid=%d reused=%d", len(plan.Invalid), len(plan.Reused))
	}
}
