package engine_test

import (
	"testing"

	"replayaudit/internal/engine"
	"replayaudit/internal/model"
)

func TestGraphRecordsExplicitDependencies(t *testing.T) {
	p := model.DefaultParams()
	r := precisionReplay()
	r.Flags = []model.FlagDef{{ID: "d", Name: "ult", Value: true}}
	r.Mutex = []model.MutexGroup{{ID: "g1", EventIDs: []string{"e1", "e6"}}}
	r.Sync = []model.SyncPoint{{ID: "s1", At: 1500}}
	r.Events = append(r.Events, &model.Event{
		ID: "e7", Kind: "damage", At: 4000, Casters: []string{"p1"}, Targets: []string{"x2"},
		Amount: 1, Cond: &model.Cond{Flag: "ult", Want: true},
	})

	g := engine.BuildGraph(r, p)

	if !g.HasEdge("event:e1", "event:e3") {
		t.Fatalf("expected RAW hp:x1 edge e1->e3; edges=%+v", g.Edges())
	}
	if g.HasEdge("event:e1", "event:e6") {
		t.Fatalf("e1 and e6 touch different keys, no RAW edge expected")
	}
	if !g.HasEdge("flag:ult", "event:e7") {
		t.Fatalf("expected flag->conditional event edge")
	}
	if !g.HasEdge("event:e1", "mutex:g1") || !g.HasEdge("mutex:g1", "event:e6") {
		t.Fatalf("expected event<->mutex edges")
	}
	if !g.HasEdge("event:e1", "sync:s1") {
		t.Fatalf("expected earlier event -> sync dependency")
	}
	if !g.HasEdge("event:e1", "segment:0") {
		t.Fatalf("expected event -> segment edge")
	}
	if len(g.Edges()) < 8 {
		t.Fatalf("expected a substantive explicit edge set, got %d", len(g.Edges()))
	}
}
