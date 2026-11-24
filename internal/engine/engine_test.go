package engine

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"replaykernel/internal/gen"
	"replaykernel/internal/model"
	"replaykernel/internal/store"
)

func fixtureReplay(t *testing.T) (*model.Replay, []byte, *model.MatchTruth) {
	t.Helper()
	dir := t.TempDir()
	_, err := gen.Generate(gen.Spec{
		OutDir: dir,
		Seed:   99,
		MatchSpecs: []gen.MatchSpec{
			{MatchID: "t1", Turns: 120, Units: 4},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := readFile(filepath.Join(dir, "replays", "t1.json"))
	if err != nil {
		t.Fatal(err)
	}
	truthRaw, err := readFile(filepath.Join(dir, "truth", "t1.truth.json"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := LoadReplay(raw)
	if err != nil {
		t.Fatal(err)
	}
	var truth model.MatchTruth
	if err := json.Unmarshal(truthRaw, &truth); err != nil {
		t.Fatal(err)
	}
	return r, raw, &truth
}

func readFile(path string) ([]byte, error) {
	return osReadFile(path)
}

func TestLoaderSchemaAndOrder(t *testing.T) {
	r, raw, _ := fixtureReplay(t)

	t.Run("decode error", func(t *testing.T) {
		if _, err := LoadReplay([]byte("{broken")); !codeIs(err, model.EDecode) {
			t.Fatalf("want E_DECODE got %v", err)
		}
	})

	t.Run("missing field", func(t *testing.T) {
		var anyMap map[string]any
		if err := json.Unmarshal(raw, &anyMap); err != nil {
			t.Fatal(err)
		}
		delete(anyMap, "match_id")
		bad, _ := json.Marshal(anyMap)
		_, err := LoadReplay(bad)
		if !codeIs(err, model.ESchema) {
			t.Fatalf("want E_SCHEMA got %v", err)
		}
	})

	t.Run("turn jump", func(t *testing.T) {
		cp := clone(r)
		cp.Turns[5].TurnNo = 99
		bad, _ := json.Marshal(cp)
		_, err := LoadReplay(bad)
		if !codeIs(err, model.EOrder) {
			t.Fatalf("want E_ORDER got %v", err)
		}
		ke, _ := model.AsKernelError(err)
		if ke.Expected != 6 || ke.Got != 99 {
			t.Fatalf("order context wrong: %+v", ke)
		}
	})

	t.Run("bad order permutation", func(t *testing.T) {
		cp := clone(r)
		cp.Turns[0].Order[0] = "ghost"
		bad, _ := json.Marshal(cp)
		_, err := LoadReplay(bad)
		if !codeIs(err, model.ESchema) {
			t.Fatalf("want E_SCHEMA got %v", err)
		}
	})

	t.Run("unknown skill ref", func(t *testing.T) {
		cp := clone(r)
		cp.Turns[0].Intents[0].SkillID = "nope"
		bad, _ := json.Marshal(cp)
		_, err := LoadReplay(bad)
		if !codeIs(err, model.ESchema) {
			t.Fatalf("want E_SCHEMA got %v", err)
		}
	})
}

func TestDeterministicRunsMatchTruth(t *testing.T) {
	r, raw, truth := fixtureReplay(t)
	params := model.DefaultParams()

	st, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	runner := NewRunner(raw, HashTruth(truth), params)
	if _, err := runner.RunLive(r, st); err != nil {
		t.Fatal(err)
	}
	snaps, err := st.LoadSnapshots(r.MatchID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != len(truth.Turns) {
		t.Fatalf("snapshot count %d != truth %d", len(snaps), len(truth.Turns))
	}
	for i := range truth.Turns {
		if dp, ok := diffSnapshots([]model.TurnState{truth.Turns[i]}, []model.TurnState{snaps[i]}); !ok {
			t.Fatalf("truth mismatch: %+v", dp)
		}
	}
}

func TestThreePathsIdentical(t *testing.T) {
	r, raw, truth := fixtureReplay(t)
	params := model.DefaultParams()
	dir := t.TempDir()

	openDB := func(name string) *store.Store {
		st, err := store.Open(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	liveDB := openDB("live.db")
	if _, err := NewRunner(raw, HashTruth(truth), params).RunLive(r, liveDB); err != nil {
		t.Fatal(err)
	}

	recPath := filepath.Join(dir, "rec.db")
	if err := store.SnapshotFile(filepath.Join(dir, "live.db"), recPath); err != nil {
		t.Fatal(err)
	}
	recDB := openDB("rec.db")
	res, err := NewRunner(raw, HashTruth(truth), params).RunRecovery(r, recDB)
	if err != nil || !res.Recovered {
		t.Fatalf("recovery failed: res=%+v err=%v", res, err)
	}

	fullDB := openDB("full.db")
	if _, err := NewRunner(raw, HashTruth(truth), params).RunFullReplay(r, fullDB); err != nil {
		t.Fatal(err)
	}

	if err := CompareStores(r.MatchID, liveDB, recDB); err != nil {
		t.Fatalf("live vs recovery: %v", err)
	}
	if err := CompareStores(r.MatchID, liveDB, fullDB); err != nil {
		t.Fatalf("live vs full: %v", err)
	}
	liveDB.Close()
	recDB.Close()
	fullDB.Close()
}

func TestWindowStateMachine(t *testing.T) {
	w := WindowState{No: 0, Status: model.WindowPending}
	if err := w.Transition(model.WindowCommitted); !codeIs(err, model.EState) {
		t.Fatalf("pending->committed must be E_STATE, got %v", err)
	}
	if w.Status != model.WindowPending {
		t.Fatal("illegal transition must keep original status")
	}
	if err := w.Transition(model.WindowRunning); err != nil {
		t.Fatal(err)
	}
	if err := w.Transition(model.WindowFailed); err != nil {
		t.Fatal(err)
	}
	if err := w.Transition(model.WindowPending); err != nil {
		t.Fatal("failed->pending retry must be legal")
	}
}

func TestAdaptiveBothAlgorithms(t *testing.T) {
	r, raw, truth := fixtureReplay(t)
	dir := t.TempDir()

	smallBudget := model.DefaultParams()
	smallBudget.MemBudgetBytes = 1024 // 强制走预聚合路径
	d := ChooseAlgorithm(r.MatchID, len(r.Turns), len(r.Units), smallBudget)
	if d.Algorithm != model.AlgoAggregate {
		t.Fatalf("want aggregate, got %s", d.Algorithm)
	}
	aggDB, err := store.Open(filepath.Join(dir, "agg.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer aggDB.Close()
	if _, err := NewRunner(raw, HashTruth(truth), smallBudget).RunLive(r, aggDB); err != nil {
		t.Fatal(err)
	}

	fineDB, err := store.Open(filepath.Join(dir, "fine.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fineDB.Close()
	if _, err := NewRunner(raw, HashTruth(truth), model.DefaultParams()).RunLive(r, fineDB); err != nil {
		t.Fatal(err)
	}
	if err := CompareStores(r.MatchID, aggDB, fineDB); err != nil {
		t.Fatalf("two algorithms must be identical: %v", err)
	}
}

func TestCheckpointInvalidation(t *testing.T) {
	r, raw, truth := fixtureReplay(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "c.db")

	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(raw, HashTruth(truth), model.DefaultParams())
	if _, err := runner.RunLive(r, st); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// 篡改回放内容哈希后恢复，必须整体作废并从头重算。
	st2, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	tampered := NewRunner(append(raw, 'x'), HashTruth(truth), model.DefaultParams())
	res, err := tampered.RunRecovery(r, st2)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Invalidated {
		t.Fatal("hash mismatch must invalidate all checkpoints")
	}
	snaps, err := st2.LoadSnapshots(r.MatchID)
	if err != nil || len(snaps) != len(truth.Turns) {
		t.Fatalf("rebuild after invalidation failed: %v snaps=%d", err, len(snaps))
	}
}

func TestConflictAndStageInjection(t *testing.T) {
	r, _, _ := fixtureReplay(t)
	dir := t.TempDir()

	specs := []gen.InjectSpec{
		{MatchID: "fc", Kind: "conflict", Turn: 10},
		{MatchID: "fs", Kind: "stage", Turn: 10},
	}
	if err := gen.BuildInjectSamples(r, dir, specs); err != nil {
		t.Fatal(err)
	}
	for _, sp := range specs {
		raw, err := osReadFile(filepath.Join(dir, "faults", sp.MatchID+".json"))
		if err != nil {
			t.Fatal(err)
		}
		bad, err := LoadReplay(raw)
		if err != nil {
			t.Fatal(err)
		}
		st, err := store.Open(filepath.Join(dir, sp.MatchID+".db"))
		if err != nil {
			t.Fatal(err)
		}
		_, err = NewRunner(raw, "x", model.DefaultParams()).RunLive(bad, st)
		_ = st.Close()
		want := model.EConflict
		if sp.Kind == "stage" {
			want = model.EStage
		}
		if !codeIs(err, want) {
			t.Fatalf("%s: want %s got %v", sp.MatchID, want, err)
		}
	}
}

func TestRulingPriorityOrderIndependent(t *testing.T) {
	r, raw, truth := fixtureReplay(t)
	// 人为构造同时资源不足+冷却中的意图，验证固定优先级。
	cp := clone(r)
	cp.Turns[0].Intents[0].SkillID = "heavy"
	rt := newMatchRuntime(cp)
	rt.units[cp.Turns[0].Intents[0].UnitID].mp = 0
	rt.units[cp.Turns[0].Intents[0].UnitID].cooldowns["heavy"] = 3
	intent := cp.Turns[0].Intents[0]
	skill := rt.skills[intent.SkillID]
	actor := rt.units[intent.UnitID]
	ruling, allowed, err := rt.adjudicate(1, intent, actor, skill)
	if err != nil || allowed {
		t.Fatalf("adjudicate: allowed=%v err=%v", allowed, err)
	}
	if ruling.Reason != model.ViolationInsufficientResource {
		t.Fatalf("priority wrong: reason=%s", ruling.Reason)
	}
	if len(ruling.Violations) < 2 {
		t.Fatalf("all violations must be recorded: %+v", ruling.Violations)
	}
	_ = raw
	_ = truth
}

func codeIs(err error, code string) bool {
	var ke *model.KernelError
	if errors.As(err, &ke) {
		return ke.Code == code
	}
	return false
}

func clone(r *model.Replay) *model.Replay {
	b, _ := json.Marshal(r)
	var cp model.Replay
	_ = json.Unmarshal(b, &cp)
	return &cp
}
