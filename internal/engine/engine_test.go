package engine

import (
	"testing"

	"skillsched/internal/errx"
	"skillsched/internal/gen"
	"skillsched/internal/model"
)

func assertTruthEqual(t *testing.T, got []model.TickTruth, want *model.Truth) {
	t.Helper()
	if len(got) != len(want.Ticks) {
		t.Fatalf("tick count got=%d want=%d", len(got), len(want.Ticks))
	}
	for i := range got {
		g, w := got[i], want.Ticks[i]
		if g.Tick != w.Tick {
			t.Fatalf("tick index mismatch %d!=%d", g.Tick, w.Tick)
		}
		wm := map[string]model.SkillTick{}
		for _, s := range w.Skills {
			wm[s.SkillID] = s
		}
		for _, gs := range g.Skills {
			ws := wm[gs.SkillID]
			if gs.Available != ws.Available {
				t.Fatalf("tick %d skill %s available got=%v want=%v", g.Tick, gs.SkillID, gs.Available, ws.Available)
			}
			if gs.CooldownRem != ws.CooldownRem {
				t.Fatalf("tick %d skill %s cooldown got=%d want=%d", g.Tick, gs.SkillID, gs.CooldownRem, ws.CooldownRem)
			}
			if gs.PoolRemain != ws.PoolRemain {
				t.Fatalf("tick %d skill %s pool got=%d want=%d", g.Tick, gs.SkillID, gs.PoolRemain, ws.PoolRemain)
			}
		}
		if len(g.Arbitrations) != len(w.Arbitrations) {
			t.Fatalf("tick %d arbitration count got=%d want=%d", g.Tick, len(g.Arbitrations), len(w.Arbitrations))
		}
		for k := range g.Arbitrations {
			if g.Arbitrations[k] != w.Arbitrations[k] {
				t.Fatalf("tick %d arbitration[%d] got=%+v want=%+v", g.Tick, k, g.Arbitrations[k], w.Arbitrations[k])
			}
		}
	}
}

func TestFullMatchesGeneratorTruth(t *testing.T) {
	sc := gen.Generate("engine", gen.DefaultConfig())
	flat, err := RunFull(sc.Rules, sc.Intents, false)
	if err != nil {
		t.Fatal(err)
	}
	assertTruthEqual(t, flat, sc.Truth)
}

func TestTreeAndFlatEquivalent(t *testing.T) {
	sc := gen.Generate("equiv", gen.DefaultConfig())
	flat, err := RunFull(sc.Rules, sc.Intents, false)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := RunFull(sc.Rules, sc.Intents, true)
	if err != nil {
		t.Fatal(err)
	}
	if tick, sid, field, ok := Compare(flat, tree); ok {
		t.Fatalf("tree/flat differ at tick=%d skill=%s field=%s", tick, sid, field)
	}
}

func TestDeterministicAcrossRuns(t *testing.T) {
	sc := gen.Generate("det", gen.DefaultConfig())
	first, _ := RunFull(sc.Rules, sc.Intents, true)
	for i := 0; i < 5; i++ {
		again, _ := RunFull(sc.Rules, sc.Intents, true)
		if tick, sid, field, ok := Compare(first, again); ok {
			t.Fatalf("non-deterministic at tick=%d skill=%s field=%s", tick, sid, field)
		}
	}
}

func TestConflictPreservesState(t *testing.T) {
	sc := gen.Generate("conflict", gen.DefaultConfig())
	sid := sc.Rules.Skills[0].ID
	bad := append([]model.Intent{}, sc.Intents...)
	bad = append(bad,
		model.Intent{Tick: 0, SkillID: sid, Seq: 9001},
		model.Intent{Tick: 0, SkillID: sid, Seq: 9002},
	)
	eng := New(sc.Rules, InitialRuntime(sc.Rules), false)
	before := eng.snapshot()
	_, err := eng.RunWindow(0, sc.Rules.Params.WindowSize, bad)
	if errx.CodeOf(err) != errx.EConflict {
		t.Fatalf("want E_CONFLICT got %v", err)
	}
	after := eng.Runtime()
	if after.Tick != before.Tick || after.IntentOffset != before.IntentOffset {
		t.Fatal("state not preserved after E_CONFLICT")
	}
	for pid, v := range before.PoolRemain {
		if after.PoolRemain[pid] != v {
			t.Fatal("pool state mutated after conflict rollback")
		}
	}
}

func TestStateTransitions(t *testing.T) {
	if err := transitionSkill(SkillCommitted, SkillSettling); errx.CodeOf(err) != errx.EState {
		t.Fatalf("want E_STATE got %v", err)
	}
	if err := transitionWindow(WinCommitted, WinScheduling); errx.CodeOf(err) != errx.EState {
		t.Fatalf("want E_STATE got %v", err)
	}
	if err := transitionSkill(SkillFailed, SkillReady); err != nil {
		t.Fatal("failed->ready retry should be legal")
	}
}
