package engine_test

import (
	"reflect"
	"testing"

	"replayaudit/internal/engine"
	"replayaudit/internal/gen"
	"replayaudit/internal/model"
)

func precisionReplay() *model.Replay {
	return &model.Replay{
		ReplayID: "precision-test",
		Entities: []model.EntitySpec{
			{ID: "p1", Side: "blue", MaxHP: 100, MaxMP: 100},
			{ID: "p2", Side: "blue", MaxHP: 100, MaxMP: 100},
			{ID: "x1", Side: "red", MaxHP: 100, MaxMP: 100},
			{ID: "x2", Side: "red", MaxHP: 100, MaxMP: 100},
		},
		Events: []*model.Event{
			{ID: "e1", Kind: "damage", At: 0, Casters: []string{"p1"}, Targets: []string{"x1"}, Amount: 10},
			{ID: "e2", Kind: "damage", At: 1000, Casters: []string{"x1"}, Targets: []string{"p1"}, Amount: 5},
			{ID: "e4", Kind: "gain", At: 1000, Casters: []string{"p2"}, Targets: []string{"p2"}, Amount: 7},
			{ID: "e3", Kind: "heal", At: 2000, Casters: []string{"p2"}, Targets: []string{"x1"}, Amount: 4},
			{ID: "e5", Kind: "spend", At: 2000, Casters: []string{"p2"}, Targets: []string{"p2"}, Amount: 3},
			{ID: "e6", Kind: "damage", At: 3000, Casters: []string{"p1"}, Targets: []string{"x2"}, Amount: 9},
		},
	}
}

func TestPreciseInvalidationSet(t *testing.T) {
	p := model.DefaultParams()
	svc := engine.NewService(precisionReplay(), p)
	if _, err := svc.RunFull(); err != nil {
		t.Fatal(err)
	}
	res, err := svc.ApplyEdit(engine.Edit{Op: "amount", EventID: "e1", Amount: 99})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Manifest.RecomputedEvent; !reflect.DeepEqual(got, []string{"e1", "e3"}) {
		t.Fatalf("recomputed=%v want [e1 e3]", got)
	}
	reused := map[string]bool{}
	for _, id := range res.Manifest.ReusedEvents {
		reused[id] = true
	}
	for _, id := range []string{"e2", "e4", "e5", "e6"} {
		if !reused[id] {
			t.Fatalf("event %s must be reused; reused=%v", id, res.Manifest.ReusedEvents)
		}
	}
	fresh, err := engine.SettleFull(svc.Replay(), p)
	if err != nil {
		t.Fatal(err)
	}
	if mm := engine.CompareReports(fresh, res.Report); mm != nil {
		t.Fatalf("incremental != full: %v", mm)
	}
}

func TestTimeEditDirtiesOldAndNewSegments(t *testing.T) {
	p := model.DefaultParams()
	p.SegmentUnits = 1000
	r := precisionReplay()
	svc := engine.NewService(r, p)
	if _, err := svc.RunFull(); err != nil {
		t.Fatal(err)
	}
	// e1 at 0 (segment 0) -> 2500 (segment 2)
	res, err := svc.ApplyEdit(engine.Edit{Op: "time", EventID: "e1", At: 2500})
	if err != nil {
		t.Fatal(err)
	}
	dirty := map[int]bool{}
	for _, s := range res.Manifest.DirtySegments {
		dirty[s] = true
	}
	if !dirty[0] || !dirty[2] {
		t.Fatalf("old and new segments must be dirty: %v", res.Manifest.DirtySegments)
	}
	fresh, _ := engine.SettleFull(svc.Replay(), p)
	if mm := engine.CompareReports(fresh, res.Report); mm != nil {
		t.Fatalf("time edit parity: %v", mm)
	}
}

func TestFlagEditPropagatesToConditionalEvent(t *testing.T) {
	p := model.DefaultParams()
	r := precisionReplay()
	r.Flags = []model.FlagDef{{ID: "d", Name: "ult", Value: false, Priority: 0}}
	r.Events = append(r.Events, &model.Event{
		ID: "e7", Kind: "damage", At: 4000, Casters: []string{"p1"}, Targets: []string{"x2"},
		Amount: 50, Cond: &model.Cond{Flag: "ult", Want: true},
	})
	svc := engine.NewService(r, p)
	first, err := svc.RunFull()
	if err != nil {
		t.Fatal(err)
	}
	if first.ResultByID()["e7"].Status != engine.StatusSkippedCond {
		t.Fatalf("e7 should start skipped")
	}
	v := true
	res, err := svc.ApplyEdit(engine.Edit{Op: "set_flag", Flag: "ult", Bool: &v})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range res.Manifest.RecomputedEvent {
		if id == "e7" {
			found = true
		}
	}
	if !found {
		t.Fatalf("flag change must recompute e7: %v", res.Manifest.RecomputedEvent)
	}
	if res.Report.ResultByID()["e7"].Status != engine.StatusSettled {
		t.Fatalf("e7 should settle after flag toggle")
	}
	fresh, _ := engine.SettleFull(svc.Replay(), p)
	if mm := engine.CompareReports(fresh, res.Report); mm != nil {
		t.Fatal(mm)
	}
}

func TestAddRemoveSyncManifestAndParity(t *testing.T) {
	p := model.DefaultParams()
	r := precisionReplay()
	svc := engine.NewService(r, p)
	if _, err := svc.RunFull(); err != nil {
		t.Fatal(err)
	}
	added, err := svc.ApplyEdit(engine.Edit{Op: "add_sync", SyncAt: 1500})
	if err != nil {
		t.Fatal(err)
	}
	var hasSyncItem bool
	for _, it := range added.Manifest.Items {
		if it.Kind == engine.NodeSync && it.Invalidated {
			hasSyncItem = true
		}
	}
	if !hasSyncItem {
		t.Fatalf("add_sync manifest missing sync node: %+v", added.Manifest.Items)
	}
	fresh, _ := engine.SettleFull(svc.Replay(), p)
	if mm := engine.CompareReports(fresh, added.Report); mm != nil {
		t.Fatal(mm)
	}
	removed, err := svc.ApplyEdit(engine.Edit{Op: "remove_sync", SyncAt: 1500})
	if err != nil {
		t.Fatal(err)
	}
	fresh2, _ := engine.SettleFull(svc.Replay(), p)
	if mm := engine.CompareReports(fresh2, removed.Report); mm != nil {
		t.Fatal(mm)
	}
}

func TestIncrementalParityAcrossSamples(t *testing.T) {
	p := model.DefaultParams()
	for _, b := range gen.BuildAll() {
		r := b.Replay.Clone()
		svc := engine.NewService(r, p)
		if _, err := svc.RunFull(); err != nil {
			t.Fatalf("%s full: %v", b.Replay.ReplayID, err)
		}
		order := engine.CanonicalOrder(r.Events)
		edits := []engine.Edit{
			{Op: "amount", EventID: order[0].ID, Amount: order[0].Amount + 3},
			{Op: "time", EventID: order[len(order)-1].ID, At: order[len(order)-1].At + p.MsPerTurn},
		}
		if len(r.Flags) > 0 {
			v := !r.Flags[0].Value
			edits = append(edits, engine.Edit{Op: "set_flag", Flag: r.Flags[0].Name, Bool: &v})
		}
		var last *engine.IncrementalResult
		for _, ed := range edits {
			res, err := svc.ApplyEdit(ed)
			if err != nil {
				t.Fatalf("%s edit %+v: %v", b.Replay.ReplayID, ed, err)
			}
			last = res
		}
		fresh, err := engine.SettleFull(r, p)
		if err != nil {
			t.Fatal(err)
		}
		if mm := engine.CompareReports(fresh, last.Report); mm != nil {
			t.Fatalf("%s parity: %v", b.Replay.ReplayID, mm)
		}
	}
}

func TestCompareReportsLocatesField(t *testing.T) {
	p := model.DefaultParams()
	r := precisionReplay()
	want, err := engine.SettleFull(r, p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := engine.SettleFull(r.Clone(), p)
	if err != nil {
		t.Fatal(err)
	}
	got.ResultByID()["e3"].EffectiveAt++
	mm := engine.CompareReports(want, got)
	if mm == nil || mm.EventID != "e3" || mm.Field != "effective_at" {
		t.Fatalf("expected e3 effective_at mismatch, got %+v", mm)
	}

	got2, _ := engine.SettleFull(r.Clone(), p)
	got2.ResultByID()["e3"].StateAfter.HP["x1"] += 1
	mm = engine.CompareReports(want, got2)
	if mm == nil || mm.EventID != "e3" {
		t.Fatalf("expected event-scoped mismatch, got %+v", mm)
	}
	if mm.Field != "state.hp.x1" {
		t.Fatalf("expected state.hp.x1 field, got %q", mm.Field)
	}
}
