// Package sample builds deterministic local projects, schema variants and
// settlement truth.
package sample

import (
	"fmt"

	"economycore/model"
)

// Scenario returns the canonical fixed-point project for seed. Values that
// must round-trip through v1's per-mille scale are always multiples of ten.
func Scenario(seed int64) *model.Model {
	offset := seed % 5
	if offset < 0 {
		offset = -offset
	}
	initialWood := int64(30) + offset
	grainCap := int64(24) + offset
	return &model.Model{
		ID:                fmt.Sprintf("frontier-chain-%d", seed),
		Name:              "Frontier Production Chain",
		SourceVersion:     model.SchemaV3,
		ConversionVersion: model.DefaultConversionVersion,
		CycleMs:           1000,
		WindowCycles:      4,
		WindowCount:       3,
		RateScale:         10000,
		WorkerScale:       10000,
		WorkersTotal:      8,
		WorkersMaximum:    100,
		Resources: []model.Resource{
			{ID: "wood", Name: "Wood", Kind: model.KindRaw},
			{ID: "plank", Name: "Plank", Kind: model.KindCrafted},
			{ID: "furniture", Name: "Furniture", Kind: model.KindCrafted},
			{ID: "grain", Name: "Grain", Kind: model.KindFood},
			{ID: "bread", Name: "Bread", Kind: model.KindFood},
		},
		InitialStocks: map[string]int64{
			"wood":      initialWood,
			"plank":     8,
			"furniture": 2,
			"grain":     20,
			"bread":     0,
		},
		Capacities: map[string]int64{
			"wood":      200,
			"plank":     40,
			"furniture": 30,
			"grain":     grainCap,
			"bread":     30,
		},
		OverflowPolicy:             model.DefaultOverflowPolicy,
		UpgradeDiscountBasisPoints: model.DefaultUpgradeDiscountBasisPoints,
		Nodes: []*model.Node{
			{
				ID:                    "forest",
				Name:                  "Forest Camp",
				Built:                 true,
				Level:                 1,
				Recipe:                "timber",
				RateExpr:              mustExpr(model.Expr{Op: model.ExprAdd, Args: []model.Expr{model.ConstExpr(6000), model.ConstExpr(4000)}}),
				WorkerEfficiencyFixed: 10000,
				Workers:               2,
				Recipes: []model.Recipe{
					{ID: "timber", Inputs: map[string]int64{}, Outputs: map[string]int64{"wood": 4}},
				},
				BuildCost: map[string]int64{},
				Upgrade: model.Upgrade{
					MaxLevel:     3,
					RateAddFixed: 2500,
					CapacityAdd:  map[string]int64{"wood": 50},
					Cost:         map[string]int64{"wood": 10},
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
				Workers:               2,
				Recipes: []model.Recipe{
					{ID: "planks", Inputs: map[string]int64{"wood": 2}, Outputs: map[string]int64{"plank": 2}},
					{ID: "efficient", Inputs: map[string]int64{"wood": 3}, Outputs: map[string]int64{"plank": 3}},
				},
				BuildCost: map[string]int64{},
				Upgrade: model.Upgrade{
					MaxLevel:     3,
					RateAddFixed: 2500,
					CapacityAdd:  map[string]int64{"plank": 10},
					Cost:         map[string]int64{"wood": 5, "plank": 2},
				},
			},
			{
				ID:                    "workshop",
				Name:                  "Furniture Workshop",
				Built:                 true,
				Level:                 1,
				Recipe:                "furniture",
				RateExpr:              model.ConstExpr(10000),
				WorkerEfficiencyFixed: 10000,
				Workers:               1,
				Recipes: []model.Recipe{
					{ID: "furniture", Inputs: map[string]int64{"plank": 1}, Outputs: map[string]int64{"furniture": 1}},
				},
				BuildCost: map[string]int64{},
				Upgrade: model.Upgrade{
					MaxLevel:     3,
					RateAddFixed: 1000,
					CapacityAdd:  map[string]int64{"furniture": 5},
					Cost:         map[string]int64{"wood": 5},
				},
			},
			{
				ID:                    "farm",
				Name:                  "Grain Farm",
				Built:                 true,
				Level:                 1,
				Recipe:                "grain",
				RateExpr:              model.ConstExpr(10000),
				WorkerEfficiencyFixed: 10000,
				Workers:               1,
				Recipes: []model.Recipe{
					{ID: "grain", Inputs: map[string]int64{}, Outputs: map[string]int64{"grain": 6}},
				},
				BuildCost: map[string]int64{},
				Upgrade: model.Upgrade{
					MaxLevel:     3,
					RateAddFixed: 1000,
					CapacityAdd:  map[string]int64{"grain": 10},
					Cost:         map[string]int64{"wood": 2},
				},
			},
			{
				ID:                    "bakery",
				Name:                  "Bakery",
				Built:                 false,
				Level:                 1,
				Recipe:                "bread",
				RateExpr:              model.ConstExpr(10000),
				WorkerEfficiencyFixed: 10000,
				Workers:               0,
				Recipes: []model.Recipe{
					{ID: "bread", Inputs: map[string]int64{"grain": 4}, Outputs: map[string]int64{"bread": 3}},
					{ID: "hearty", Inputs: map[string]int64{"grain": 3}, Outputs: map[string]int64{"bread": 4}},
				},
				BuildCost: map[string]int64{"wood": 10, "plank": 5},
				Upgrade: model.Upgrade{
					MaxLevel:     3,
					RateAddFixed: 1000,
					CapacityAdd:  map[string]int64{"bread": 10},
					Cost:         map[string]int64{"wood": 6},
				},
			},
		},
		Intents: []model.Intent{
			{Offset: 0, Window: 0, Cycle: 0, Action: model.IntentUpgrade, NodeID: "forest"},
			{Offset: 1, Window: 0, Cycle: 1, Action: model.IntentSwitchRecipe, NodeID: "sawmill", RecipeID: "efficient"},
			{Offset: 2, Window: 0, Cycle: 2, Action: model.IntentAssignWorkers, NodeID: "workshop", Workers: intPtr(1)},
			{Offset: 3, Window: 1, Cycle: 0, Action: model.IntentBuild, NodeID: "bakery"},
			{Offset: 4, Window: 1, Cycle: 1, Action: model.IntentAssignWorkers, NodeID: "bakery", Workers: intPtr(2)},
			{Offset: 5, Window: 1, Cycle: 2, Action: model.IntentUpgrade, NodeID: "sawmill"},
			{Offset: 6, Window: 2, Cycle: 0, Action: model.IntentDemolish, NodeID: "farm"},
			{Offset: 7, Window: 2, Cycle: 1, Action: model.IntentAssignWorkers, NodeID: "bakery", Workers: intPtr(2)},
			{Offset: 8, Window: 2, Cycle: 2, Action: model.IntentSwitchRecipe, NodeID: "bakery", RecipeID: "hearty"},
		},
		Presence: model.Presence{
			WorkersMaximum:  true,
			OverflowPolicy:  true,
			UpgradeDiscount: true,
		},
		RoundMode: "floor",
	}
}

func mustExpr(expression model.Expr) model.Expr {
	if err := expression.Validate(); err != nil {
		panic(err)
	}
	return expression
}

func intPtr(value int) *int { return &value }
