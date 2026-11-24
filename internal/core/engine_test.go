package core_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"economycore/internal/core"
	"economycore/internal/store"
)

func intPtr(v int) *int { return &v }

func baseScenario(t *testing.T) *core.Scenario {
	t.Helper()
	s := &core.Scenario{
		SchemaVersion: core.SchemaVersion,
		ID:            "unit_scene",
		Seed:          42,
		TickUnitMS:    100,
		ParamsVersion: "params/v1",
		FinalTick:     12,
		Nodes: []core.Node{
			{
				ID:              "source",
				Type:            core.NodeFarm,
				Active:          true,
				Workers:         2,
				MaxWorkers:      4,
				Outputs:         map[string]int64{"grain": 5000},
				Inputs:          map[string]int64{},
				Capacity:        map[string]int64{"grain": 4000},
				WorkersRequired: 1,
				InitialStock:    map[string]int64{"grain": 0},
				Upgrades: []core.Upgrade{{
					Level:           1,
					Outputs:         map[string]int64{"grain": 8000},
					Inputs:          map[string]int64{},
					Capacity:        map[string]int64{"grain": 12000},
					WorkersRequired: 1,
				}},
			},
			{
				ID:              "factory",
				Type:            core.NodeFactory,
				ParentID:        "source",
				Active:          true,
				Workers:         2,
				MaxWorkers:      5,
				Outputs:         map[string]int64{"food": 1000},
				Inputs:          map[string]int64{"grain": 3000},
				Capacity:        map[string]int64{"food": 5000, "grain": 10000},
				WorkersRequired: 1,
				InitialStock:    map[string]int64{"food": 0},
				Upgrades: []core.Upgrade{{
					Level:           1,
					Outputs:         map[string]int64{"food": 1500},
					Inputs:          map[string]int64{"grain": 3500},
					Capacity:        map[string]int64{"food": 8000, "grain": 12000},
					WorkersRequired: 2,
				}},
			},
		},
		Intents: []core.Intent{
			{Seq: 0, Timestamp: 2, Type: core.IntentUpgrade, NodeID: "source", Level: intPtr(1)},
			{Seq: 1, Timestamp: 4, Type: core.IntentAssignWorker, NodeID: "factory", Workers: intPtr(0)},
			{Seq: 2, Timestamp: 6, Type: core.IntentAssignWorker, NodeID: "factory", Workers: intPtr(2)},
			{Seq: 3, Timestamp: 8, Type: core.IntentRecipe, NodeID: "factory", Recipe: &core.Params{
				Outputs:         map[string]int64{"food": 1100},
				Inputs:          map[string]int64{"grain": 2800},
				Capacity:        map[string]int64{"food": 5000, "grain": 10000},
				WorkersRequired: 1,
			}},
		},
	}
	finalize(t, s)
	return s
}

func finalize(t *testing.T, s *core.Scenario) {
	t.Helper()
	if err := core.ValidateScenario(s); err != nil {
		t.Fatalf("scenario validation failed: %v", err)
	}
	hash, err := core.ComputeSceneHash(s)
	if err != nil {
		t.Fatal(err)
	}
	s.SceneHash = hash
}

func TestUpgradeSwitchesParamsAtomically(t *testing.T) {
	s := baseScenario(t)
	eng, err := core.NewEngine(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.AdvanceTo(2); err != nil {
		t.Fatal(err)
	}
	if err := eng.Step(); err != nil {
		t.Fatal(err)
	}
	n := eng.State().Nodes["source"]
	if n.Level != 1 {
		t.Fatalf("level=%d want 1", n.Level)
	}
	if n.Params.Outputs["grain"] != 8000 || n.Params.Capacity["grain"] != 12000 {
		t.Fatalf("upgrade produced mixed params: %+v", n.Params)
	}
	if n.Stock["grain"] > n.Params.Capacity["grain"] {
		t.Fatalf("stock exceeds upgraded capacity: %d > %d", n.Stock["grain"], n.Params.Capacity["grain"])
	}
}

func TestInvalidUpgradeReturnsEStateAndRollsBackStep(t *testing.T) {
	s := baseScenario(t)
	s.Intents[0].Level = intPtr(2)
	finalize(t, s)
	eng, err := core.NewEngine(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.AdvanceTo(2); err != nil {
		t.Fatal(err)
	}
	err = eng.Step()
	if core.CodeOf(err) != core.CodeState {
		t.Fatalf("code=%s want E_STATE: %v", core.CodeOf(err), err)
	}
	e := core.AsError(err)
	if e.NodeID != "source" {
		t.Fatalf("node=%q", e.NodeID)
	}
	if eng.State().Tick != 2 {
		t.Fatalf("failed step advanced tick to %d", eng.State().Tick)
	}
	if eng.State().Nodes["source"].Level != 0 {
		t.Fatalf("failed upgrade changed level")
	}
	if eng.State().IntentOffset != 0 {
		t.Fatalf("failed upgrade consumed intent")
	}
}

func TestConsumeShortageReturnsERES(t *testing.T) {
	s := baseScenario(t)
	s.Intents = []core.Intent{{
		Seq: 0, Timestamp: 0, Type: core.IntentConsume, NodeID: "factory",
		Consume: map[string]int64{"grain": 200},
	}}
	finalize(t, s)
	eng, err := core.NewEngine(s)
	if err != nil {
		t.Fatal(err)
	}
	err = eng.Step()
	if core.CodeOf(err) != core.CodeResource {
		t.Fatalf("code=%s want E_RES: %v", core.CodeOf(err), err)
	}
	e := core.AsError(err)
	if e.IntentID == nil || *e.IntentID != 0 || e.NodeID != "factory" || e.Field != "consume.grain" {
		t.Fatalf("bad E_RES locator: %+v", e)
	}
	if eng.State().Tick != 0 || eng.State().IntentOffset != 0 {
		t.Fatalf("failed step polluted state")
	}
}

func TestRuntimeMissingIntentTargetReturnsEUNIT(t *testing.T) {
	s := baseScenario(t)
	eng, err := core.NewEngine(s)
	if err != nil {
		t.Fatal(err)
	}
	eng.Scene().Intents[0].NodeID = "ghost"
	err = eng.AdvanceTo(3)
	if core.CodeOf(err) != core.CodeUnit {
		t.Fatalf("code=%s want E_UNIT: %v", core.CodeOf(err), err)
	}
	e := core.AsError(err)
	if e.NodeID != "ghost" || e.IntentID == nil {
		t.Fatalf("bad E_UNIT locator: %+v", e)
	}
}

func TestStallAndRecoveryPreserveStartTick(t *testing.T) {
	s := baseScenario(t)
	s.Nodes[0].Workers = 0
	s.Intents = []core.Intent{
		{Seq: 0, Timestamp: 1, Type: core.IntentAssignWorker, NodeID: "source", Workers: intPtr(2)},
	}
	finalize(t, s)
	eng, err := core.NewEngine(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Step(); err != nil {
		t.Fatal(err)
	}
	n := snapshotNode(t, eng.Snapshot(), "source")
	if !n.Stalled || n.StallSince == nil || *n.StallSince != 0 {
		t.Fatalf("stall not recorded: %+v", n)
	}
	if err := eng.Step(); err != nil {
		t.Fatal(err)
	}
	n = snapshotNode(t, eng.Snapshot(), "source")
	if n.Stalled || n.StallSince != nil {
		t.Fatalf("stall not cleared: %+v", n)
	}
}

func TestRecipeCapacityDowngradeTruncatesAndRecordsOverflow(t *testing.T) {
	s := baseScenario(t)
	s.FinalTick = 1
	s.Nodes[0].Active = false
	s.Nodes[0].InitialStock = map[string]int64{"grain": 3000}
	s.Intents = []core.Intent{{
		Seq: 0, Timestamp: 0, Type: core.IntentRecipe, NodeID: "source",
		Recipe: &core.Params{
			Outputs:         map[string]int64{},
			Inputs:          map[string]int64{},
			Capacity:        map[string]int64{"grain": 1000},
			WorkersRequired: 0,
		},
	}}
	finalize(t, s)
	eng, err := core.NewEngine(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Step(); err != nil {
		t.Fatal(err)
	}
	n := eng.State().Nodes["source"]
	if n.Stock["grain"] != 1000 {
		t.Fatalf("stock=%d want 1000 after capacity downgrade", n.Stock["grain"])
	}
	sources := eng.Snapshot().OverflowSources
	if len(sources) != 1 || sources[0].NodeID != "source" || sources[0].Resource != "grain" || sources[0].Total != 2000 {
		t.Fatalf("unexpected overflow sources: %+v", sources)
	}
}

func TestInitialStockIsCappedAtCapacity(t *testing.T) {
	s := baseScenario(t)
	s.Nodes[0].InitialStock = map[string]int64{"grain": 9000}
	finalize(t, s)
	eng, err := core.NewEngine(s)
	if err != nil {
		t.Fatal(err)
	}
	if got := eng.State().Nodes["source"].Stock["grain"]; got != 4000 {
		t.Fatalf("initial stock=%d want capacity 4000", got)
	}
	snap := eng.Snapshot()
	found := false
	for _, source := range snap.OverflowSources {
		if source.NodeID == "source" && source.Resource == "grain" && source.Total == 5000 {
			found = true
		}
	}
	if !found {
		t.Fatalf("initial capacity overflow not recorded: %+v", snap.OverflowSources)
	}
}

func TestPreviewAndParameterBoundsReturnEPARA(t *testing.T) {
	s := baseScenario(t)
	if _, err := core.RunPreview(s, 0, nil, s.FinalTick); core.CodeOf(err) != core.CodeParam {
		t.Fatalf("preview step code=%s: %v", core.CodeOf(err), err)
	}
	if _, err := core.RunWindowed(context.Background(), s, nil, core.WindowOptions{WindowSize: 0}); core.CodeOf(err) != core.CodeParam {
		t.Fatalf("window size code=%s: %v", core.CodeOf(err), err)
	}
	if _, _, err := core.RunWindowedParallel(context.Background(), s, nil, core.ParallelOptions{
		WindowSize: 4, Workers: 0, BufferPerWorker: 2,
	}); core.CodeOf(err) != core.CodeParam {
		t.Fatalf("workers code=%s: %v", core.CodeOf(err), err)
	}
}

func TestCancelledRunReturnsECANCEL(t *testing.T) {
	s := baseScenario(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := core.RunWindowed(ctx, s, nil, core.WindowOptions{WindowSize: 4})
	if core.CodeOf(err) != core.CodeCancel {
		t.Fatalf("code=%s want E_CANCEL: %v", core.CodeOf(err), err)
	}
}

func TestCheckpointResumeStopAndContinue(t *testing.T) {
	s := baseScenario(t)
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "checkpoint.db"))
	if err != nil {
		t.Fatal(err)
	}
	phase1, err := core.RunWindowed(context.Background(), s, st, core.WindowOptions{
		WindowSize: 4, StopAfterWindows: 1, SampleTicks: []int64{0, 4, 8, 12},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !phase1.Stopped || phase1.State.Tick != 4 || phase1.Final.Tick != 4 {
		t.Fatalf("phase1 did not stop cleanly: stopped=%v state=%d final=%d",
			phase1.Stopped, phase1.State.Tick, phase1.Final.Tick)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(filepath.Join(dir, "checkpoint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	phase2, err := core.RunWindowed(context.Background(), s, st, core.WindowOptions{
		WindowSize: 4, SampleTicks: []int64{4, 8, 12},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !phase2.Resumed {
		t.Fatal("phase2 did not resume")
	}
	if phase2.Final.Tick != s.FinalTick {
		t.Fatalf("final tick=%d", phase2.Final.Tick)
	}
	full, err := core.RunFull(s, []int64{4, 8, 12})
	if err != nil {
		t.Fatal(err)
	}
	if diff := core.CompareSnapshots(full.Final, phase2.Final); diff != nil {
		t.Fatalf("resume diverged: %v", diff)
	}
	if code := core.CodeOf(func() error {
		_, _, err := core.RunWindowedParallel(context.Background(), s, st, core.ParallelOptions{
			WindowSize: 4, Workers: 2, BufferPerWorker: 1,
		})
		return err
	}()); code != "" {
		t.Fatalf("parallel after complete checkpoint: %v", code)
	}
}

func TestResumeAtPartialFinalWindowIsIdempotent(t *testing.T) {
	s := baseScenario(t)
	s.FinalTick = 10
	finalize(t, s)
	st, err := store.Open(filepath.Join(t.TempDir(), "partial.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	first, err := core.RunWindowed(context.Background(), s, st, core.WindowOptions{WindowSize: 4})
	if err != nil {
		t.Fatal(err)
	}
	if first.Final.Tick != 10 || len(first.Windows) != 3 {
		t.Fatalf("unexpected first run: tick=%d windows=%d", first.Final.Tick, len(first.Windows))
	}
	second, err := core.RunWindowed(context.Background(), s, st, core.WindowOptions{WindowSize: 4})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Resumed || second.Final.Tick != 10 || len(second.Windows) != 0 {
		t.Fatalf("completed partial window did not resume idempotently: resumed=%v tick=%d windows=%d",
			second.Resumed, second.Final.Tick, len(second.Windows))
	}
}

func TestCheckpointVersionMismatchInvalidatesWholePrefix(t *testing.T) {
	s := baseScenario(t)
	dbPath := filepath.Join(t.TempDir(), "checkpoint.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := core.RunWindowed(context.Background(), s, st, core.WindowOptions{
		WindowSize: 4, StopAfterWindows: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := st.Count(context.Background(), s.ID); err != nil || n != 1 {
		t.Fatalf("count=%d err=%v", n, err)
	}
	changed := *s
	changed.ParamsVersion = "params/v2"
	resume, err := st.LoadResume(context.Background(), &changed)
	if err != nil {
		t.Fatal(err)
	}
	if !resume.Reset {
		t.Fatal("version mismatch did not reset checkpoints")
	}
	if n, err := st.Count(context.Background(), s.ID); err != nil || n != 0 {
		t.Fatalf("stale checkpoints remain: count=%d err=%v", n, err)
	}
	st.Close()
}

func TestWindowFailureRecordedWithoutCommittingWindow(t *testing.T) {
	s := baseScenario(t)
	s.Intents = append(s.Intents, core.Intent{
		Seq: 4, Timestamp: 5, Type: core.IntentConsume, NodeID: "factory",
		Consume: map[string]int64{"grain": 999999},
	})
	sort.Slice(s.Intents, func(i, j int) bool { return s.Intents[i].Timestamp < s.Intents[j].Timestamp })
	finalize(t, s)
	dbPath := filepath.Join(t.TempDir(), "failure.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, err = core.RunWindowed(context.Background(), s, st, core.WindowOptions{WindowSize: 4})
	if core.CodeOf(err) != core.CodeResource {
		t.Fatalf("run code=%s want E_RES: %v", core.CodeOf(err), err)
	}
	failure, err := st.Failure(context.Background(), s.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if failure == nil || !failure.Retryable || failure.Code != core.CodeResource {
		t.Fatalf("failure not recorded: %+v", failure)
	}
	if n, err := st.Count(context.Background(), s.ID); err != nil || n != 1 {
		t.Fatalf("previous committed window polluted: count=%d err=%v", n, err)
	}
	resume, err := st.LoadResume(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !resume.Found || resume.NextWindow != 1 {
		t.Fatalf("resume point wrong: %+v", resume)
	}
}

func TestDecodeReturnsEDECODE(t *testing.T) {
	if _, err := core.DecodeScenario([]byte("{broken")); core.CodeOf(err) != core.CodeDecode {
		t.Fatalf("code=%s want E_DECODE", core.CodeOf(err))
	}
}

func TestSampleManifestAndTruth(t *testing.T) {
	data, err := os.ReadFile("../../data/samples/manifest.json")
	if err != nil {
		t.Skip("repository samples not generated")
	}
	var manifest struct {
		Samples []struct {
			ID string `json:"id"`
		} `json:"samples"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, entry := range manifest.Samples {
		scene, err := core.LoadScenario(filepath.Join("../../data/samples", entry.ID+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := core.NewEngine(scene); err != nil {
			t.Fatalf("%s: %v", entry.ID, err)
		}
	}
}

func snapshotNode(t *testing.T, snap core.Snapshot, id string) core.NodeSnapshot {
	t.Helper()
	for _, n := range snap.Nodes {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("node %s not found in snapshot", id)
	return core.NodeSnapshot{}
}
