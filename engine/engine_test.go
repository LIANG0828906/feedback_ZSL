package engine

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"

	"economycore/model"
)

func testModel() *model.Model {
	return &model.Model{
		ID:                "engine-test",
		Name:              "Engine Test",
		SourceVersion:     model.SchemaV3,
		ConversionVersion: model.DefaultConversionVersion,
		CycleMs:           1000,
		WindowCycles:      2,
		WindowCount:       2,
		RateScale:         10000,
		WorkerScale:       10000,
		WorkersTotal:      4,
		WorkersMaximum:    100,
		Resources: []model.Resource{
			{ID: "wood", Name: "Wood", Kind: model.KindRaw},
			{ID: "plank", Name: "Plank", Kind: model.KindCrafted},
			{ID: "grain", Name: "Grain", Kind: model.KindFood},
		},
		InitialStocks:              map[string]int64{"wood": 9, "plank": 0, "grain": 0},
		Capacities:                 map[string]int64{"wood": 10, "plank": 100, "grain": 100},
		OverflowPolicy:             "cap_drop",
		UpgradeDiscountBasisPoints: 0,
		Nodes: []*model.Node{
			{
				ID:                    "forest",
				Name:                  "Forest",
				Built:                 true,
				Level:                 1,
				Recipe:                "logs",
				RateExpr:              model.ConstExpr(10000),
				WorkerEfficiencyFixed: 10000,
				Workers:               1,
				Recipes: []model.Recipe{
					{ID: "logs", Inputs: map[string]int64{}, Outputs: map[string]int64{"wood": 2}},
				},
				BuildCost: map[string]int64{},
				Upgrade: model.Upgrade{
					MaxLevel:     3,
					RateAddFixed: 10000,
					CapacityAdd:  map[string]int64{"wood": 5},
					Cost:         map[string]int64{},
				},
			},
			{
				ID:                    "sawmill",
				Name:                  "Sawmill",
				Built:                 true,
				Level:                 1,
				Recipe:                "planks",
				RateExpr:              model.ConstExpr(10000),
				WorkerEfficiencyFixed: 10000,
				Workers:               1,
				Recipes: []model.Recipe{
					{ID: "planks", Inputs: map[string]int64{"wood": 1}, Outputs: map[string]int64{"plank": 1}},
				},
				BuildCost: map[string]int64{},
				Upgrade: model.Upgrade{
					MaxLevel:     3,
					RateAddFixed: 10000,
					CapacityAdd:  map[string]int64{},
					Cost:         map[string]int64{},
				},
			},
			{
				ID:                    "farm",
				Name:                  "Farm",
				Built:                 true,
				Level:                 1,
				Recipe:                "grow",
				RateExpr:              model.ConstExpr(10000),
				WorkerEfficiencyFixed: 10000,
				Workers:               1,
				Recipes: []model.Recipe{
					{ID: "grow", Inputs: map[string]int64{}, Outputs: map[string]int64{"grain": 3}},
				},
				BuildCost: map[string]int64{},
				Upgrade: model.Upgrade{
					MaxLevel:     3,
					RateAddFixed: 10000,
					CapacityAdd:  map[string]int64{},
					Cost:         map[string]int64{},
				},
			},
		},
		Intents: []model.Intent{},
		Presence: model.Presence{
			WorkersMaximum:  true,
			OverflowPolicy:  true,
			UpgradeDiscount: true,
		},
		RoundMode: "floor",
	}
}

func TestSettlementOverflowAndIncrementalEquality(t *testing.T) {
	m := testModel()
	eng, err := New(m, Options{})
	if err != nil {
		t.Fatal(err)
	}
	full, err := eng.RunFull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !full.Complete || len(full.Cycles) != 4 {
		t.Fatalf("unexpected run shape: complete=%v cycles=%d", full.Complete, len(full.Cycles))
	}
	first := full.Cycles[0]
	if first.GlobalStocks["wood"] != 9 {
		t.Fatalf("wood after sawmill consumption=9, got %d", first.GlobalStocks["wood"])
	}
	if first.GlobalStocks["plank"] != 1 {
		t.Fatalf("plank=1, got %d", first.GlobalStocks["plank"])
	}
	if full.OverflowTotals["forest/wood"] != 4 {
		t.Fatalf("forest overflow total=4, got %d", full.OverflowTotals["forest/wood"])
	}
	forestNode := findNode(t, first, "forest")
	if forestNode.Overflow["wood"] != 1 {
		t.Fatalf("first forest overflow=1, got %d", forestNode.Overflow["wood"])
	}

	report, err := eng.EditRate("forest", 20000)
	if err != nil {
		t.Fatal(err)
	}
	if report.Unaffected[0] != "farm" {
		t.Fatalf("farm should be unaffected, report=%+v", report)
	}
	incremental, err := eng.RunIncremental(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := New(eng.Model(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	fullAfter, err := fresh.RunFull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareRuns(incremental, fullAfter); err != nil {
		t.Fatalf("incremental differs: %v", err)
	}
	if incremental.ReusedCycles == 0 {
		t.Fatal("expected some cache reuse")
	}
	for _, status := range incremental.Reuse {
		if status.NodeID == "farm" && !status.Reused {
			t.Fatalf("farm cache should be reused: %+v", status)
		}
		if status.NodeID == "forest" && status.Reused {
			t.Fatalf("forest cache should be invalidated: %+v", status)
		}
		if status.NodeID == "farm" && status.ReusedCycles == 0 {
			t.Fatalf("farm reuse count should be positive: %+v", status)
		}
	}
}

func TestCacheSwapIsAtomicOnFailure(t *testing.T) {
	m := testModel()
	var inject atomic.Bool
	eng, err := New(m, Options{Fault: func(window int, cycle int64) error {
		if inject.Load() && cycle == 2 {
			return model.NewError(model.EUnit, "/injected", "cache failure").WithWindow(window)
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.RunFull(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := cloneTransitionMap(eng.cache)
	if _, err := eng.EditRate("forest", 20000); err != nil {
		t.Fatal(err)
	}
	inject.Store(true)
	if _, err := eng.RunIncremental(context.Background()); !model.IsCode(err, model.EUnit) {
		t.Fatalf("expected injected failure, got %v", err)
	}
	if !reflect.DeepEqual(before, eng.cache) {
		t.Fatal("failed incremental run partially replaced the cache")
	}
	inject.Store(false)
	if _, err := eng.RunIncremental(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(before, eng.cache) {
		t.Fatal("successful incremental run should atomically install new cache entries")
	}
}

func TestSharedResourceConsumerRecomputesAndStillMatchesFull(t *testing.T) {
	m := testModel()
	m.WorkersTotal = 5
	m.Nodes = append(m.Nodes, &model.Node{
		ID:                    "kiln",
		Name:                  "Kiln",
		Built:                 true,
		Level:                 1,
		Recipe:                "heat",
		RateExpr:              model.ConstExpr(10000),
		WorkerEfficiencyFixed: 10000,
		Workers:               1,
		Recipes: []model.Recipe{
			{ID: "heat", Inputs: map[string]int64{"wood": 1}, Outputs: map[string]int64{"grain": 1}},
		},
		BuildCost: map[string]int64{},
		Upgrade: model.Upgrade{
			MaxLevel:     3,
			RateAddFixed: 10000,
			CapacityAdd:  map[string]int64{},
			Cost:         map[string]int64{},
		},
	})
	eng, err := New(m, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.RunFull(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.EditRate("sawmill", 20000); err != nil {
		t.Fatal(err)
	}
	incremental, err := eng.RunIncremental(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := New(eng.Model(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	full, err := fresh.RunFull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareRuns(incremental, full); err != nil {
		t.Fatalf("shared-resource incremental differs: %v", err)
	}
}

func TestUpgradeInSameCycleAndIntentErrors(t *testing.T) {
	m := testModel()
	m.Intents = []model.Intent{
		{Offset: 0, Window: 0, Cycle: 0, Action: model.IntentUpgrade, NodeID: "forest"},
	}
	eng, err := New(m, Options{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := eng.RunFull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	forestNode := findNode(t, result.Cycles[0], "forest")
	if forestNode.Level != 2 || forestNode.RateFixed != 20000 {
		t.Fatalf("upgrade not effective in cycle: %+v", forestNode)
	}
	if result.Cycles[0].GlobalStocks["wood"] != 12 || result.Cycles[0].GlobalStocks["plank"] != 1 {
		t.Fatalf("unexpected cycle0 stocks: %+v", result.Cycles[0].GlobalStocks)
	}

	bad := testModel()
	bad.Intents = []model.Intent{{Offset: 0, Window: 0, Cycle: 0, Action: model.IntentUpgrade, NodeID: "missing"}}
	eng, err = New(bad, Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = eng.RunFull(context.Background())
	if !model.IsCode(err, model.EUnit) {
		t.Fatalf("expected E_UNIT, got %v", err)
	}
	typed, _ := model.AsError(err)
	if !typed.HasWindow || typed.Window != 0 || typed.NodeID != "missing" {
		t.Fatalf("missing error context: %+v", typed)
	}

	expensive := testModel()
	expensive.Nodes[0].Upgrade.Cost = map[string]int64{"wood": 999}
	expensive.Intents = []model.Intent{{Offset: 0, Window: 0, Cycle: 0, Action: model.IntentUpgrade, NodeID: "forest"}}
	eng, err = New(expensive, Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = eng.RunFull(context.Background())
	if !model.IsCode(err, model.ERes) {
		t.Fatalf("expected E_RES, got %v", err)
	}
}

func TestInputShortageStallStartIsPreserved(t *testing.T) {
	m := testModel()
	m.WorkersTotal = 1
	m.Nodes = []*model.Node{m.Nodes[1]}
	m.Nodes[0].Workers = 1
	m.InitialStocks = map[string]int64{"wood": 1, "plank": 0, "grain": 0}
	m.Intents = nil
	eng, err := New(m, Options{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := eng.RunFull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Cycles[0].Nodes[0].Stalled {
		t.Fatal("cycle 0 should consume the available input")
	}
	if !result.Cycles[1].Nodes[0].Stalled || result.Cycles[1].Nodes[0].StallStartCycle != 1 {
		t.Fatalf("cycle 1 stall start wrong: %+v", result.Cycles[1].Nodes[0])
	}
	if !result.Cycles[2].Nodes[0].Stalled || result.Cycles[2].Nodes[0].StallStartCycle != 1 {
		t.Fatalf("stall start must be preserved: %+v", result.Cycles[2].Nodes[0])
	}
}

func TestWorkerShortageResumesAfterAssignment(t *testing.T) {
	m := testModel()
	m.WorkersTotal = 3
	m.Nodes[1].Workers = 0
	m.Intents = []model.Intent{
		{Offset: 0, Window: 0, Cycle: 1, Action: model.IntentAssignWorkers, NodeID: "sawmill", Workers: intPtrLocal(1)},
	}
	eng, err := New(m, Options{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := eng.RunFull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	first := findNode(t, result.Cycles[0], "sawmill")
	if !first.Stalled || first.StallStartCycle != 0 {
		t.Fatalf("sawmill should stall for missing workers: %+v", first)
	}
	second := findNode(t, result.Cycles[1], "sawmill")
	if second.Stalled || second.Produced["plank"] != 1 {
		t.Fatalf("sawmill should resume after assignment: %+v", second)
	}
}

func intPtrLocal(value int) *int { return &value }

func TestRejectOverflowPolicyStallsInsteadOfDropping(t *testing.T) {
	m := testModel()
	m.OverflowPolicy = "reject"
	eng, err := New(m, Options{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := eng.RunFull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	forest := findNode(t, result.Cycles[0], "forest")
	if !forest.Stalled || len(forest.Produced) != 0 || forest.Overflow["wood"] != 0 {
		t.Fatalf("reject policy should stall without overflow: %+v", forest)
	}
	if result.Cycles[0].GlobalStocks["wood"] != 8 {
		t.Fatalf("sawmill may consume existing stock, wood=%d", result.Cycles[0].GlobalStocks["wood"])
	}
}

func TestRuntimeRecipeSwitchReordersTopology(t *testing.T) {
	m := &model.Model{
		ID:                "runtime-switch",
		Name:              "Runtime Switch",
		SourceVersion:     model.SchemaV3,
		ConversionVersion: model.DefaultConversionVersion,
		CycleMs:           1000,
		WindowCycles:      2,
		WindowCount:       1,
		RateScale:         10000,
		WorkerScale:       10000,
		WorkersTotal:      3,
		WorkersMaximum:    100,
		Resources: []model.Resource{
			{ID: "a", Name: "A", Kind: model.KindRaw},
			{ID: "b", Name: "B", Kind: model.KindCrafted},
			{ID: "c", Name: "C", Kind: model.KindRaw},
		},
		InitialStocks: map[string]int64{"a": 0, "b": 0, "c": 0},
		Capacities:    map[string]int64{"a": 100, "b": 100, "c": 100},
		Nodes: []*model.Node{
			{
				ID: "sourceA", Built: true, Level: 1, Recipe: "a", RateExpr: model.ConstExpr(10000),
				WorkerEfficiencyFixed: 10000, Workers: 1,
				Recipes:   []model.Recipe{{ID: "a", Inputs: map[string]int64{}, Outputs: map[string]int64{"a": 1}}},
				BuildCost: map[string]int64{}, Upgrade: model.Upgrade{MaxLevel: 1, RateAddFixed: 0, CapacityAdd: map[string]int64{}, Cost: map[string]int64{}},
			},
			{
				ID: "converter", Built: true, Level: 1, Recipe: "fromA", RateExpr: model.ConstExpr(10000),
				WorkerEfficiencyFixed: 10000, Workers: 1,
				Recipes: []model.Recipe{
					{ID: "fromA", Inputs: map[string]int64{"a": 1}, Outputs: map[string]int64{"b": 1}},
					{ID: "fromC", Inputs: map[string]int64{"c": 1}, Outputs: map[string]int64{"b": 1}},
				},
				BuildCost: map[string]int64{}, Upgrade: model.Upgrade{MaxLevel: 1, RateAddFixed: 0, CapacityAdd: map[string]int64{}, Cost: map[string]int64{}},
			},
			{
				ID: "sourceC", Built: true, Level: 1, Recipe: "c", RateExpr: model.ConstExpr(10000),
				WorkerEfficiencyFixed: 10000, Workers: 1,
				Recipes:   []model.Recipe{{ID: "c", Inputs: map[string]int64{}, Outputs: map[string]int64{"c": 1}}},
				BuildCost: map[string]int64{}, Upgrade: model.Upgrade{MaxLevel: 1, RateAddFixed: 0, CapacityAdd: map[string]int64{}, Cost: map[string]int64{}},
			},
		},
		Intents: []model.Intent{
			{Offset: 0, Window: 0, Cycle: 1, Action: model.IntentSwitchRecipe, NodeID: "converter", RecipeID: "fromC"},
		},
		Presence:  model.Presence{WorkersMaximum: true, OverflowPolicy: true, UpgradeDiscount: true},
		RoundMode: "floor",
	}
	eng, err := New(m, Options{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := eng.RunFull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	converter := findNode(t, result.Cycles[1], "converter")
	if converter.Stalled || converter.Consumed["c"] != 1 || converter.Produced["b"] != 1 {
		t.Fatalf("runtime recipe switch was not ordered after its new input producer: %+v", converter)
	}
}

func findNode(t *testing.T, cycle CycleSnapshot, id string) NodeSnapshot {
	t.Helper()
	for _, node := range cycle.Nodes {
		if node.NodeID == id {
			return node
		}
	}
	t.Fatalf("node %s not found", id)
	return NodeSnapshot{}
}
