package format

import (
	"encoding/json"
	"fmt"

	"economycore/fixed"
	"economycore/model"
)

func permilleToFixed(value int64, path, nodeID, field string) (int64, error) {
	if value < 0 {
		return 0, model.NewError(model.ESchema, path, "value cannot be negative").WithNode(nodeID, field)
	}
	scaled, err := fixed.Mul(value, 10)
	if err != nil {
		return 0, model.NewError(model.EOvf, path, "per-mille scale overflow").WithNode(nodeID, field)
	}
	return scaled, nil
}

func loadV1(root jsonObject) (*model.Model, error) {
	projectID, err := root.requiredString("project_id", "")
	if err != nil {
		return nil, err
	}
	projectName, err := root.requiredString("project_name", "")
	if err != nil {
		return nil, err
	}
	conversionVersion, err := root.requiredString("conversion_version", "")
	if err != nil {
		return nil, err
	}
	roundMode, err := readRoundMode(root, "")
	if err != nil {
		return nil, err
	}
	timing, err := root.requiredChild("timing", "")
	if err != nil {
		return nil, err
	}
	unit, err := timing.requiredString("unit", "/timing")
	if err != nil {
		return nil, err
	}
	if unit != "tick" {
		return nil, model.NewError(model.ESchema, "/timing/unit", "v1 requires tick timing")
	}
	ticksPerCycle, err := timing.requiredInt("ticks_per_cycle", "/timing")
	if err != nil {
		return nil, err
	}
	msPerTick, err := timing.requiredInt("milliseconds_per_tick", "/timing")
	if err != nil {
		return nil, err
	}
	if ticksPerCycle <= 0 || msPerTick <= 0 {
		return nil, model.NewError(model.ESchema, "/timing", "tick counts and milliseconds per tick must be positive")
	}
	cycleMs, err := fixed.PeriodToMs(ticksPerCycle, msPerTick)
	if err != nil {
		return nil, model.NewError(model.EOvf, "/timing", "cycle duration overflow").WithCause(err)
	}
	if cycleMs <= 0 {
		return nil, model.NewError(model.ESchema, "/timing", "cycle duration must be positive")
	}
	windowCycles, err := root.requiredInt("window_cycles", "")
	if err != nil {
		return nil, err
	}
	windowCount, err := root.requiredInt("window_count", "")
	if err != nil {
		return nil, err
	}
	workersTotal, err := root.requiredInt("workforce", "")
	if err != nil {
		return nil, err
	}
	resourceItems, err := root.requiredArray("resources", "")
	if err != nil {
		return nil, err
	}
	resources := make([]model.Resource, 0, len(resourceItems))
	for i := range resourceItems {
		path := model.JSONPointer("resources", fmt.Sprint(i))
		item, err := arrayItem(resourceItems, i, "/resources")
		if err != nil {
			return nil, err
		}
		id, err := item.requiredString("id", path)
		if err != nil {
			return nil, err
		}
		name, err := item.requiredString("name", path)
		if err != nil {
			return nil, err
		}
		kindText, err := item.requiredString("kind", path)
		if err != nil {
			return nil, err
		}
		kind, err := resourceKindFromString(kindText, path+"/kind")
		if err != nil {
			return nil, err
		}
		resources = append(resources, model.Resource{ID: id, Name: name, Kind: kind})
	}
	initial, err := requiredIntMap(root, "initial_stock", "")
	if err != nil {
		return nil, err
	}
	capacity, err := requiredIntMap(root, "capacity", "")
	if err != nil {
		return nil, err
	}
	nodeItems, err := root.requiredArray("nodes", "")
	if err != nil {
		return nil, err
	}
	nodes := make([]*model.Node, 0, len(nodeItems))
	for i := range nodeItems {
		path := model.JSONPointer("nodes", fmt.Sprint(i))
		item, err := arrayItem(nodeItems, i, "/nodes")
		if err != nil {
			return nil, err
		}
		id, err := item.requiredString("id", path)
		if err != nil {
			return nil, err
		}
		name, err := item.requiredString("name", path)
		if err != nil {
			return nil, err
		}
		built, err := item.requiredBool("built", path)
		if err != nil {
			return nil, err
		}
		level, err := item.requiredInt("level", path)
		if err != nil {
			return nil, err
		}
		recipeID, err := item.requiredString("recipe", path)
		if err != nil {
			return nil, err
		}
		recipesRaw, err := item.requiredChild("recipes", path)
		if err != nil {
			return nil, err
		}
		recipes := make([]model.Recipe, 0, len(recipesRaw))
		for _, recipeID := range sortedKeys(recipesRaw) {
			rawRecipe := recipesRaw[recipeID]
			if string(rawRecipe) == "null" {
				return nil, model.NewError(model.ESchema, path+"/recipes/"+recipeID, "recipe must be an object")
			}
			recipeObj, err := decodeObject(rawRecipe, path+"/recipes/"+recipeID)
			if err != nil {
				return nil, err
			}
			inputs, err := optionalIntMap(recipeObj, "inputs", path+"/recipes/"+recipeID)
			if err != nil {
				return nil, err
			}
			outputs, err := optionalIntMap(recipeObj, "outputs", path+"/recipes/"+recipeID)
			if err != nil {
				return nil, err
			}
			recipes = append(recipes, model.Recipe{ID: recipeID, Inputs: inputs, Outputs: outputs})
		}
		ratePermille, err := item.requiredInt("rate_permille", path)
		if err != nil {
			return nil, err
		}
		workerPermille, err := item.requiredInt("worker_efficiency_permille", path)
		if err != nil {
			return nil, err
		}
		rate, err := permilleToFixed(ratePermille, path+"/rate_permille", id, "rate")
		if err != nil {
			return nil, err
		}
		workerEfficiency, err := permilleToFixed(workerPermille, path+"/worker_efficiency_permille", id, "workerEfficiency")
		if err != nil {
			return nil, err
		}
		workers, err := item.requiredInt("workers", path)
		if err != nil {
			return nil, err
		}
		buildCost, err := requiredIntMap(item, "build_cost", path)
		if err != nil {
			return nil, err
		}
		upgradeObj, err := item.requiredChild("upgrade", path)
		if err != nil {
			return nil, err
		}
		maxLevel, err := upgradeObj.requiredInt("max_level", path+"/upgrade")
		if err != nil {
			return nil, err
		}
		rateAddPermille, err := upgradeObj.requiredInt("rate_add_permille", path+"/upgrade")
		if err != nil {
			return nil, err
		}
		rateAdd, err := permilleToFixed(rateAddPermille, path+"/upgrade/rate_add_permille", id, "upgradeRateAdd")
		if err != nil {
			return nil, err
		}
		capacityAdd, err := optionalIntMap(upgradeObj, "capacity_add", path+"/upgrade")
		if err != nil {
			return nil, err
		}
		upgradeCost, err := requiredIntMap(upgradeObj, "cost", path+"/upgrade")
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, &model.Node{
			ID:                    id,
			Name:                  name,
			Built:                 built,
			Level:                 int(level),
			Recipe:                recipeID,
			Recipes:               recipes,
			RateExpr:              model.ConstExpr(rate),
			WorkerEfficiencyFixed: workerEfficiency,
			Workers:               int(workers),
			BuildCost:             buildCost,
			Upgrade: model.Upgrade{
				MaxLevel:     int(maxLevel),
				RateAddFixed: rateAdd,
				CapacityAdd:  capacityAdd,
				Cost:         upgradeCost,
			},
		})
	}
	intents, err := loadIntentsV1(root)
	if err != nil {
		return nil, err
	}
	workersMaximum := model.DefaultWorkersMaximum
	if workersTotal > workersMaximum {
		workersMaximum = workersTotal
	}
	return &model.Model{
		ID:                         projectID,
		Name:                       projectName,
		SourceVersion:              model.SchemaV1,
		ConversionVersion:          conversionVersion,
		CycleMs:                    cycleMs,
		WindowCycles:               int(windowCycles),
		WindowCount:                int(windowCount),
		RateScale:                  10000,
		WorkerScale:                10000,
		WorkersTotal:               workersTotal,
		WorkersMaximum:             workersMaximum,
		Resources:                  resources,
		InitialStocks:              initial,
		Capacities:                 capacity,
		OverflowPolicy:             model.DefaultOverflowPolicy,
		UpgradeDiscountBasisPoints: model.DefaultUpgradeDiscountBasisPoints,
		Nodes:                      nodes,
		Intents:                    intents,
		Presence:                   model.Presence{},
		RoundMode:                  roundMode,
	}, nil
}

func loadIntentsV1(root jsonObject) ([]model.Intent, error) {
	items, err := root.requiredArray("intents", "")
	if err != nil {
		return nil, err
	}
	out := make([]model.Intent, 0, len(items))
	for i := range items {
		path := model.JSONPointer("intents", fmt.Sprint(i))
		item, err := arrayItem(items, i, "/intents")
		if err != nil {
			return nil, err
		}
		offset, err := item.requiredInt("offset", path)
		if err != nil {
			return nil, err
		}
		window, err := item.requiredInt("window", path)
		if err != nil {
			return nil, err
		}
		cycle, err := item.requiredInt("cycle", path)
		if err != nil {
			return nil, err
		}
		action, err := item.requiredString("action", path)
		if err != nil {
			return nil, err
		}
		nodeID, err := item.requiredString("node", path)
		if err != nil {
			return nil, err
		}
		recipe, err := item.string("recipe", path, false)
		if err != nil {
			return nil, err
		}
		workersRaw, workersSet, err := item.integer("workers", path, false)
		if err != nil {
			return nil, err
		}
		var workers *int
		if workersSet {
			value := int(workersRaw)
			workers = &value
		}
		out = append(out, model.Intent{
			Offset:   int(offset),
			Window:   int(window),
			Cycle:    int(cycle),
			Action:   model.IntentType(action),
			NodeID:   nodeID,
			RecipeID: recipe,
			Workers:  workers,
		})
	}
	return out, nil
}

type v1Resource struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type v1Recipe struct {
	Inputs  map[string]int64 `json:"inputs"`
	Outputs map[string]int64 `json:"outputs"`
}

type v1Upgrade struct {
	MaxLevel        int              `json:"max_level"`
	RateAddPermille int64            `json:"rate_add_permille"`
	CapacityAdd     map[string]int64 `json:"capacity_add,omitempty"`
	Cost            map[string]int64 `json:"cost"`
}

type v1Node struct {
	ID                       string              `json:"id"`
	Name                     string              `json:"name"`
	Built                    bool                `json:"built"`
	Level                    int                 `json:"level"`
	Recipe                   string              `json:"recipe"`
	Recipes                  map[string]v1Recipe `json:"recipes"`
	RatePermille             int64               `json:"rate_permille"`
	WorkerEfficiencyPermille int64               `json:"worker_efficiency_permille"`
	Workers                  int                 `json:"workers"`
	BuildCost                map[string]int64    `json:"build_cost"`
	Upgrade                  v1Upgrade           `json:"upgrade"`
}

type v1Intent struct {
	Offset  int    `json:"offset"`
	Window  int    `json:"window"`
	Cycle   int    `json:"cycle"`
	Action  string `json:"action"`
	Node    string `json:"node"`
	Recipe  string `json:"recipe,omitempty"`
	Workers *int   `json:"workers,omitempty"`
}

type v1Document struct {
	SchemaVersion     string `json:"schema_version"`
	ProjectID         string `json:"project_id"`
	ProjectName       string `json:"project_name"`
	ConversionVersion string `json:"conversion_version"`
	Rounding          string `json:"rounding,omitempty"`
	Timing            struct {
		Unit                string `json:"unit"`
		TicksPerCycle       int64  `json:"ticks_per_cycle"`
		MillisecondsPerTick int64  `json:"milliseconds_per_tick"`
	} `json:"timing"`
	WindowCycles int              `json:"window_cycles"`
	WindowCount  int              `json:"window_count"`
	Workforce    int64            `json:"workforce"`
	Resources    []v1Resource     `json:"resources"`
	InitialStock map[string]int64 `json:"initial_stock"`
	Capacity     map[string]int64 `json:"capacity"`
	Nodes        []v1Node         `json:"nodes"`
	Intents      []v1Intent       `json:"intents"`
}

func encodeV1(m *model.Model, opts exportState) ([]byte, error) {
	if m.CycleMs%250 != 0 {
		opts.addLoss(model.Loss{Path: "/timing/ticks_per_cycle", Field: "cycleMs", OldValue: m.CycleMs, Disposition: "rounded_floor", Reason: "v1 tick is 250ms"})
	}
	ticks, err := fixed.PeriodFromMs(m.CycleMs, 250, opts.roundMode)
	if err != nil {
		return nil, model.NewError(model.EOvf, "/timing/ticks_per_cycle", err.Error())
	}
	resources := make([]v1Resource, 0, len(m.Resources))
	for _, resource := range m.Resources {
		resources = append(resources, v1Resource{ID: resource.ID, Name: resource.Name, Kind: string(resource.Kind)})
	}
	nodes := make([]v1Node, 0, len(m.Nodes))
	for _, node := range m.Nodes {
		rateExpr := node.RateExpr
		if rateExpr.HasVariable() {
			if !opts.allowLossy {
				opts.addLoss(model.Loss{Path: "/nodes/" + node.ID + "/rate_formula", NodeID: node.ID, Field: "rateFormula", OldValue: rateExpr, Disposition: "requires_flattening", Reason: "v1 cannot express runtime formula variables"})
			} else {
				value, evalErr := rateExpr.Eval(map[string]int64{model.ExprWorkers: int64(node.Workers)})
				if evalErr != nil {
					return nil, model.NewError(model.ESchema, "/nodes/"+node.ID+"/rate_formula", evalErr.Error())
				}
				rateExpr = model.ConstExpr(value)
				opts.addLoss(model.Loss{Path: "/nodes/" + node.ID + "/rate_formula", NodeID: node.ID, Field: "rateFormula", OldValue: "workers_expression", Disposition: "flattened_at_export_worker_count", Reason: "v1 uses flat constant rates"})
			}
		}
		rate, evalErr := rateExpr.Eval(nil)
		if evalErr != nil {
			return nil, model.NewError(model.ESchema, "/nodes/"+node.ID+"/rate_formula", evalErr.Error())
		}
		ratePermille, exact, err := downscale(rate, 10, opts.roundMode)
		if err != nil {
			return nil, model.NewError(model.EOvf, "/nodes/"+node.ID+"/rate_permille", err.Error()).WithNode(node.ID, "rate")
		}
		if !exact {
			opts.addLoss(model.Loss{Path: "/nodes/" + node.ID + "/rate_permille", NodeID: node.ID, Field: "rate", OldValue: rate, Disposition: "rounded_" + string(opts.roundMode), Reason: "v1 rate scale is per-mille"})
		}
		worker, exact, err := downscale(node.WorkerEfficiencyFixed, 10, opts.roundMode)
		if err != nil {
			return nil, model.NewError(model.EOvf, "/nodes/"+node.ID+"/worker_efficiency_permille", err.Error()).WithNode(node.ID, "workerEfficiency")
		}
		if !exact {
			opts.addLoss(model.Loss{Path: "/nodes/" + node.ID + "/worker_efficiency_permille", NodeID: node.ID, Field: "workerEfficiency", OldValue: node.WorkerEfficiencyFixed, Disposition: "rounded_" + string(opts.roundMode), Reason: "v1 worker scale is per-mille"})
		}
		recipes := make(map[string]v1Recipe, len(node.Recipes))
		for _, recipe := range node.Recipes {
			recipes[recipe.ID] = v1Recipe{Inputs: recipe.Inputs, Outputs: recipe.Outputs}
		}
		upgradeCost := cloneCost(node.Upgrade.Cost)
		if m.UpgradeDiscountBasisPoints != 0 {
			if !opts.allowLossy {
				opts.addLoss(model.Loss{Path: "/upgradeDiscountBasisPoints", Field: "upgradeDiscountBasisPoints", OldValue: m.UpgradeDiscountBasisPoints, Disposition: "requires_flattening", Reason: "v1 has no upgrade discount field"})
			} else {
				for id, cost := range upgradeCost {
					flattened, flattenErr := flattenDiscount(cost, m.UpgradeDiscountBasisPoints)
					if flattenErr != nil {
						return nil, model.NewError(model.EOvf, "/nodes/"+node.ID+"/upgrade/cost/"+id, flattenErr.Error()).WithNode(node.ID, "upgradeCost")
					}
					upgradeCost[id] = flattened
				}
				opts.addLoss(model.Loss{Path: "/upgradeDiscountBasisPoints", Field: "upgradeDiscountBasisPoints", OldValue: m.UpgradeDiscountBasisPoints, Disposition: "flattened_into_upgrade_costs", Reason: "v1 has no upgrade discount field"})
			}
		}
		nodes = append(nodes, v1Node{
			ID:                       node.ID,
			Name:                     node.Name,
			Built:                    node.Built,
			Level:                    node.Level,
			Recipe:                   node.Recipe,
			Recipes:                  recipes,
			RatePermille:             ratePermille,
			WorkerEfficiencyPermille: worker,
			Workers:                  node.Workers,
			BuildCost:                node.BuildCost,
			Upgrade: v1Upgrade{
				MaxLevel: node.Upgrade.MaxLevel,
				RateAddPermille: func() int64 {
					value, _, _ := downscale(node.Upgrade.RateAddFixed, 10, opts.roundMode)
					return value
				}(),
				CapacityAdd: node.Upgrade.CapacityAdd,
				Cost:        upgradeCost,
			},
		})
	}
	if m.WorkersMaximum != model.DefaultWorkersMaximum && m.WorkersMaximum != m.WorkersTotal {
		opts.addLoss(model.Loss{Path: "/workersMaximum", Field: "workersMaximum", OldValue: m.WorkersMaximum, Disposition: "dropped", Reason: "v1 has no workforce maximum field"})
	}
	if m.OverflowPolicy != "" && m.OverflowPolicy != model.DefaultOverflowPolicy {
		if !opts.allowLossy {
			opts.addLoss(model.Loss{Path: "/overflowPolicy", Field: "overflowPolicy", OldValue: m.OverflowPolicy, Disposition: "requires_cap_drop", Reason: "v1 implicitly caps and drops overflow"})
		} else {
			opts.addLoss(model.Loss{Path: "/overflowPolicy", Field: "overflowPolicy", OldValue: m.OverflowPolicy, Disposition: "forced_cap_drop", Reason: "v1 implicitly caps and drops overflow"})
		}
	}
	intents := make([]v1Intent, 0, len(m.Intents))
	for _, intent := range m.Intents {
		intents = append(intents, v1Intent{
			Offset:  intent.Offset,
			Window:  intent.Window,
			Cycle:   intent.Cycle,
			Action:  string(intent.Action),
			Node:    intent.NodeID,
			Recipe:  intent.RecipeID,
			Workers: intent.Workers,
		})
	}
	doc := v1Document{
		SchemaVersion:     "economy-v1",
		ProjectID:         m.ID,
		ProjectName:       m.Name,
		ConversionVersion: m.ConversionVersion,
		Rounding:          string(opts.roundMode),
		WindowCycles:      m.WindowCycles,
		WindowCount:       m.WindowCount,
		Workforce:         m.WorkersTotal,
		Resources:         resources,
		InitialStock:      m.InitialStocks,
		Capacity:          m.Capacities,
		Nodes:             nodes,
		Intents:           intents,
	}
	doc.Timing.Unit = "tick"
	doc.Timing.TicksPerCycle = ticks
	doc.Timing.MillisecondsPerTick = 250
	return json.MarshalIndent(doc, "", "  ")
}

func loadV2(root jsonObject) (*model.Model, error) {
	project, err := root.requiredChild("project", "")
	if err != nil {
		return nil, err
	}
	projectID, err := project.requiredString("id", "/project")
	if err != nil {
		return nil, err
	}
	projectName, err := project.requiredString("name", "/project")
	if err != nil {
		return nil, err
	}
	conversionVersion, err := root.requiredString("conversionVersion", "")
	if err != nil {
		return nil, err
	}
	roundMode, err := readRoundMode(root, "")
	if err != nil {
		return nil, err
	}
	period, err := root.requiredChild("period", "")
	if err != nil {
		return nil, err
	}
	unit, err := period.requiredString("unit", "/period")
	if err != nil {
		return nil, err
	}
	if unit != "ms" {
		return nil, model.NewError(model.ESchema, "/period/unit", "v2 requires ms timing")
	}
	cycleMs, err := period.requiredInt("cycleMs", "/period")
	if err != nil {
		return nil, err
	}
	window, err := root.requiredChild("window", "")
	if err != nil {
		return nil, err
	}
	windowCycles, err := window.requiredInt("cycles", "/window")
	if err != nil {
		return nil, err
	}
	windowCount, err := window.requiredInt("count", "/window")
	if err != nil {
		return nil, err
	}
	workforce, err := root.requiredChild("workforce", "")
	if err != nil {
		return nil, err
	}
	workersTotal, err := workforce.requiredInt("total", "/workforce")
	if err != nil {
		return nil, err
	}
	workersMaximum, err := workforce.requiredInt("maximum", "/workforce")
	if err != nil {
		return nil, err
	}
	resourceItems, err := root.requiredArray("resources", "")
	if err != nil {
		return nil, err
	}
	resources := make([]model.Resource, 0, len(resourceItems))
	for i := range resourceItems {
		path := model.JSONPointer("resources", fmt.Sprint(i))
		item, err := arrayItem(resourceItems, i, "/resources")
		if err != nil {
			return nil, err
		}
		id, err := item.requiredString("id", path)
		if err != nil {
			return nil, err
		}
		name, err := item.requiredString("name", path)
		if err != nil {
			return nil, err
		}
		code, err := item.requiredInt("resourceType", path)
		if err != nil {
			return nil, err
		}
		kind, err := resourceKindFromCode(code, path+"/resourceType")
		if err != nil {
			return nil, err
		}
		resources = append(resources, model.Resource{ID: id, Name: name, Kind: kind})
	}
	initial, err := requiredIntMap(root, "initialStock", "")
	if err != nil {
		return nil, err
	}
	capacity, err := requiredIntMap(root, "capacity", "")
	if err != nil {
		return nil, err
	}
	nodeItems, err := root.requiredArray("nodes", "")
	if err != nil {
		return nil, err
	}
	nodes := make([]*model.Node, 0, len(nodeItems))
	for i := range nodeItems {
		path := model.JSONPointer("nodes", fmt.Sprint(i))
		item, err := arrayItem(nodeItems, i, "/nodes")
		if err != nil {
			return nil, err
		}
		node, err := loadNodeV2(item, path)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	intents, err := loadIntentsV2(root)
	if err != nil {
		return nil, err
	}
	return &model.Model{
		ID:                         projectID,
		Name:                       projectName,
		SourceVersion:              model.SchemaV2,
		ConversionVersion:          conversionVersion,
		CycleMs:                    cycleMs,
		WindowCycles:               int(windowCycles),
		WindowCount:                int(windowCount),
		RateScale:                  10000,
		WorkerScale:                10000,
		WorkersTotal:               workersTotal,
		WorkersMaximum:             workersMaximum,
		Resources:                  resources,
		InitialStocks:              initial,
		Capacities:                 capacity,
		OverflowPolicy:             model.DefaultOverflowPolicy,
		UpgradeDiscountBasisPoints: model.DefaultUpgradeDiscountBasisPoints,
		Nodes:                      nodes,
		Intents:                    intents,
		Presence: model.Presence{
			WorkersMaximum: true,
		},
		RoundMode: roundMode,
	}, nil
}

func loadNodeV2(item jsonObject, path string) (*model.Node, error) {
	id, err := item.requiredString("id", path)
	if err != nil {
		return nil, err
	}
	name, err := item.requiredString("name", path)
	if err != nil {
		return nil, err
	}
	built, err := item.requiredBool("built", path)
	if err != nil {
		return nil, err
	}
	level, err := item.requiredInt("level", path)
	if err != nil {
		return nil, err
	}
	recipeID, err := item.requiredString("recipe", path)
	if err != nil {
		return nil, err
	}
	recipesRaw, err := item.requiredChild("recipes", path)
	if err != nil {
		return nil, err
	}
	recipes := make([]model.Recipe, 0, len(recipesRaw))
	for _, recipeID := range sortedKeys(recipesRaw) {
		recipeObj, err := decodeObject(recipesRaw[recipeID], path+"/recipes/"+recipeID)
		if err != nil {
			return nil, err
		}
		inputs, err := optionalIntMap(recipeObj, "inputs", path+"/recipes/"+recipeID)
		if err != nil {
			return nil, err
		}
		outputs, err := optionalIntMap(recipeObj, "outputs", path+"/recipes/"+recipeID)
		if err != nil {
			return nil, err
		}
		recipes = append(recipes, model.Recipe{ID: recipeID, Inputs: inputs, Outputs: outputs})
	}
	rate, err := item.requiredInt("rateBasisPoints", path)
	if err != nil {
		return nil, err
	}
	worker, err := item.requiredInt("workerEfficiencyBasisPoints", path)
	if err != nil {
		return nil, err
	}
	formulaRaw, err := item.required("productionFormula", path)
	if err != nil {
		return nil, err
	}
	expression, err := parseFormula(formulaRaw, path+"/productionFormula")
	if err != nil {
		return nil, err
	}
	if !expression.HasVariable() {
		value, evalErr := expression.Eval(nil)
		if evalErr != nil {
			return nil, model.NewError(model.ESchema, path+"/productionFormula", evalErr.Error())
		}
		if value != rate {
			return nil, model.NewError(model.ESchema, path+"/productionFormula", "constant formula differs from rateBasisPoints")
		}
	}
	workers, err := item.requiredInt("workers", path)
	if err != nil {
		return nil, err
	}
	buildCost, err := requiredIntMap(item, "buildCost", path)
	if err != nil {
		return nil, err
	}
	upgradeObj, err := item.requiredChild("upgrade", path)
	if err != nil {
		return nil, err
	}
	maxLevel, err := upgradeObj.requiredInt("maxLevel", path+"/upgrade")
	if err != nil {
		return nil, err
	}
	rateAdd, err := upgradeObj.requiredInt("rateAddBasisPoints", path+"/upgrade")
	if err != nil {
		return nil, err
	}
	capacityAdd, err := optionalIntMap(upgradeObj, "capacityAdd", path+"/upgrade")
	if err != nil {
		return nil, err
	}
	upgradeCost, err := requiredIntMap(upgradeObj, "cost", path+"/upgrade")
	if err != nil {
		return nil, err
	}
	return &model.Node{
		ID:                    id,
		Name:                  name,
		Built:                 built,
		Level:                 int(level),
		Recipe:                recipeID,
		Recipes:               recipes,
		RateExpr:              expression,
		WorkerEfficiencyFixed: worker,
		Workers:               int(workers),
		BuildCost:             buildCost,
		Upgrade: model.Upgrade{
			MaxLevel:     int(maxLevel),
			RateAddFixed: rateAdd,
			CapacityAdd:  capacityAdd,
			Cost:         upgradeCost,
		},
	}, nil
}

func loadIntentsV2(root jsonObject) ([]model.Intent, error) {
	items, err := root.requiredArray("intents", "")
	if err != nil {
		return nil, err
	}
	out := make([]model.Intent, 0, len(items))
	for i := range items {
		path := model.JSONPointer("intents", fmt.Sprint(i))
		item, err := arrayItem(items, i, "/intents")
		if err != nil {
			return nil, err
		}
		offset, err := item.requiredInt("offset", path)
		if err != nil {
			return nil, err
		}
		window, err := item.requiredInt("window", path)
		if err != nil {
			return nil, err
		}
		cycle, err := item.requiredInt("cycle", path)
		if err != nil {
			return nil, err
		}
		action, err := item.requiredString("type", path)
		if err != nil {
			return nil, err
		}
		nodeID, err := item.requiredString("nodeId", path)
		if err != nil {
			return nil, err
		}
		recipe, err := item.string("recipeId", path, false)
		if err != nil {
			return nil, err
		}
		workersRaw, workersSet, err := item.integer("workerCount", path, false)
		if err != nil {
			return nil, err
		}
		var workers *int
		if workersSet {
			value := int(workersRaw)
			workers = &value
		}
		out = append(out, model.Intent{
			Offset:   int(offset),
			Window:   int(window),
			Cycle:    int(cycle),
			Action:   model.IntentType(action),
			NodeID:   nodeID,
			RecipeID: recipe,
			Workers:  workers,
		})
	}
	return out, nil
}

type v2Document struct {
	FormatVersion string `json:"formatVersion"`
	Project       struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"project"`
	ConversionVersion string `json:"conversionVersion"`
	Rounding          string `json:"rounding,omitempty"`
	Period            struct {
		Unit    string `json:"unit"`
		CycleMs int64  `json:"cycleMs"`
	} `json:"period"`
	Window struct {
		Cycles int `json:"cycles"`
		Count  int `json:"count"`
	} `json:"window"`
	Workforce struct {
		Total   int64 `json:"total"`
		Maximum int64 `json:"maximum"`
	} `json:"workforce"`
	Resources []struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		ResourceType int64  `json:"resourceType"`
	} `json:"resources"`
	InitialStock map[string]int64 `json:"initialStock"`
	Capacity     map[string]int64 `json:"capacity"`
	Nodes        []struct {
		ID                          string              `json:"id"`
		Name                        string              `json:"name"`
		Built                       bool                `json:"built"`
		Level                       int                 `json:"level"`
		Recipe                      string              `json:"recipe"`
		Recipes                     map[string]v1Recipe `json:"recipes"`
		RateBasisPoints             int64               `json:"rateBasisPoints"`
		WorkerEfficiencyBasisPoints int64               `json:"workerEfficiencyBasisPoints"`
		ProductionFormula           model.Expr          `json:"productionFormula"`
		Workers                     int                 `json:"workers"`
		BuildCost                   map[string]int64    `json:"buildCost"`
		Upgrade                     struct {
			MaxLevel           int              `json:"maxLevel"`
			RateAddBasisPoints int64            `json:"rateAddBasisPoints"`
			CapacityAdd        map[string]int64 `json:"capacityAdd,omitempty"`
			Cost               map[string]int64 `json:"cost"`
		} `json:"upgrade"`
	} `json:"nodes"`
	Intents []struct {
		Offset      int    `json:"offset"`
		Window      int    `json:"window"`
		Cycle       int    `json:"cycle"`
		Type        string `json:"type"`
		NodeID      string `json:"nodeId"`
		RecipeID    string `json:"recipeId,omitempty"`
		WorkerCount *int   `json:"workerCount,omitempty"`
	} `json:"intents"`
}

func encodeV2(m *model.Model, opts exportState) ([]byte, error) {
	doc := v2Document{
		FormatVersion:     "v2",
		ConversionVersion: m.ConversionVersion,
		Rounding:          string(opts.roundMode),
		InitialStock:      m.InitialStocks,
		Capacity:          m.Capacities,
	}
	doc.Project.ID = m.ID
	doc.Project.Name = m.Name
	doc.Period.Unit = "ms"
	if m.CycleMs <= 0 {
		return nil, model.NewError(model.EOvf, "/period/cycleMs", "cycle duration must be positive")
	}
	doc.Period.CycleMs = m.CycleMs
	doc.Window.Cycles = m.WindowCycles
	doc.Window.Count = m.WindowCount
	doc.Workforce.Total = m.WorkersTotal
	doc.Workforce.Maximum = m.WorkersMaximum
	if m.OverflowPolicy != "" && m.OverflowPolicy != model.DefaultOverflowPolicy {
		if !opts.allowLossy {
			opts.addLoss(model.Loss{Path: "/overflowPolicy", Field: "overflowPolicy", OldValue: m.OverflowPolicy, Disposition: "requires_cap_drop", Reason: "v2 has no overflow policy field"})
		} else {
			opts.addLoss(model.Loss{Path: "/overflowPolicy", Field: "overflowPolicy", OldValue: m.OverflowPolicy, Disposition: "forced_cap_drop", Reason: "v2 has no overflow policy field"})
		}
	}
	if m.UpgradeDiscountBasisPoints != 0 {
		if !opts.allowLossy {
			opts.addLoss(model.Loss{Path: "/upgradeDiscountBasisPoints", Field: "upgradeDiscountBasisPoints", OldValue: m.UpgradeDiscountBasisPoints, Disposition: "requires_flattening", Reason: "v2 has no upgrade discount field"})
		} else {
			opts.addLoss(model.Loss{Path: "/upgradeDiscountBasisPoints", Field: "upgradeDiscountBasisPoints", OldValue: m.UpgradeDiscountBasisPoints, Disposition: "flattened_into_upgrade_costs", Reason: "v2 has no upgrade discount field"})
		}
	}
	for _, resource := range m.Resources {
		code, ok := codeFromResourceKind(resource.Kind)
		if !ok {
			if !opts.allowLossy {
				opts.addLoss(model.Loss{Path: "/resources/" + resource.ID + "/resourceType", Field: "resourceType", OldValue: resource.Kind, Disposition: "requires_numeric_mapping", Reason: "unknown resource kind"})
				continue
			}
			code = 0
			opts.addLoss(model.Loss{Path: "/resources/" + resource.ID + "/resourceType", Field: "resourceType", OldValue: resource.Kind, Disposition: "mapped_to_raw", Reason: "unknown resource kind"})
		}
		doc.Resources = append(doc.Resources, struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			ResourceType int64  `json:"resourceType"`
		}{ID: resource.ID, Name: resource.Name, ResourceType: code})
	}
	for _, node := range m.Nodes {
		recipes := make(map[string]v1Recipe, len(node.Recipes))
		for _, recipe := range node.Recipes {
			recipes[recipe.ID] = v1Recipe{Inputs: recipe.Inputs, Outputs: recipe.Outputs}
		}
		upgradeCost := cloneCost(node.Upgrade.Cost)
		if m.UpgradeDiscountBasisPoints != 0 && opts.allowLossy {
			for id, cost := range upgradeCost {
				flattened, err := flattenDiscount(cost, m.UpgradeDiscountBasisPoints)
				if err != nil {
					return nil, model.NewError(model.EOvf, "/nodes/"+node.ID+"/upgrade/cost/"+id, err.Error()).WithNode(node.ID, "upgradeCost")
				}
				upgradeCost[id] = flattened
			}
		}
		rate, err := node.RateExpr.Eval(map[string]int64{model.ExprWorkers: int64(node.Workers)})
		if err != nil {
			return nil, model.NewError(model.ESchema, "/nodes/"+node.ID+"/productionFormula", err.Error())
		}
		entry := struct {
			ID                          string              `json:"id"`
			Name                        string              `json:"name"`
			Built                       bool                `json:"built"`
			Level                       int                 `json:"level"`
			Recipe                      string              `json:"recipe"`
			Recipes                     map[string]v1Recipe `json:"recipes"`
			RateBasisPoints             int64               `json:"rateBasisPoints"`
			WorkerEfficiencyBasisPoints int64               `json:"workerEfficiencyBasisPoints"`
			ProductionFormula           model.Expr          `json:"productionFormula"`
			Workers                     int                 `json:"workers"`
			BuildCost                   map[string]int64    `json:"buildCost"`
			Upgrade                     struct {
				MaxLevel           int              `json:"maxLevel"`
				RateAddBasisPoints int64            `json:"rateAddBasisPoints"`
				CapacityAdd        map[string]int64 `json:"capacityAdd,omitempty"`
				Cost               map[string]int64 `json:"cost"`
			} `json:"upgrade"`
		}{ID: node.ID, Name: node.Name, Built: node.Built, Level: node.Level, Recipe: node.Recipe, Recipes: recipes,
			RateBasisPoints: rate, WorkerEfficiencyBasisPoints: node.WorkerEfficiencyFixed,
			ProductionFormula: node.RateExpr, Workers: node.Workers, BuildCost: node.BuildCost}
		entry.Upgrade.MaxLevel = node.Upgrade.MaxLevel
		entry.Upgrade.RateAddBasisPoints = node.Upgrade.RateAddFixed
		entry.Upgrade.CapacityAdd = node.Upgrade.CapacityAdd
		entry.Upgrade.Cost = upgradeCost
		doc.Nodes = append(doc.Nodes, entry)
	}
	for _, intent := range m.Intents {
		doc.Intents = append(doc.Intents, struct {
			Offset      int    `json:"offset"`
			Window      int    `json:"window"`
			Cycle       int    `json:"cycle"`
			Type        string `json:"type"`
			NodeID      string `json:"nodeId"`
			RecipeID    string `json:"recipeId,omitempty"`
			WorkerCount *int   `json:"workerCount,omitempty"`
		}{Offset: intent.Offset, Window: intent.Window, Cycle: intent.Cycle, Type: string(intent.Action), NodeID: intent.NodeID, RecipeID: intent.RecipeID, WorkerCount: intent.Workers})
	}
	return json.MarshalIndent(doc, "", "  ")
}

func loadV3(root jsonObject) (*model.Model, error) {
	project, err := root.requiredChild("project", "")
	if err != nil {
		return nil, err
	}
	projectID, err := project.requiredString("projectId", "/project")
	if err != nil {
		return nil, err
	}
	projectName, err := project.requiredString("displayName", "/project")
	if err != nil {
		return nil, err
	}
	conversionVersion, err := root.requiredString("conversionRuleVersion", "")
	if err != nil {
		return nil, err
	}
	roundMode, err := readRoundMode(root, "")
	if err != nil {
		return nil, err
	}
	clock, err := root.requiredChild("clock", "")
	if err != nil {
		return nil, err
	}
	unit, err := clock.requiredString("unit", "/clock")
	if err != nil {
		return nil, err
	}
	if unit != "tick" {
		return nil, model.NewError(model.ESchema, "/clock/unit", "v3 requires tick timing")
	}
	ticksPerCycle, err := clock.requiredInt("ticksPerCycle", "/clock")
	if err != nil {
		return nil, err
	}
	msPerTick, err := clock.requiredInt("millisecondsPerTick", "/clock")
	if err != nil {
		return nil, err
	}
	if ticksPerCycle <= 0 || msPerTick <= 0 {
		return nil, model.NewError(model.ESchema, "/clock", "tick counts and milliseconds per tick must be positive")
	}
	cycleMs, err := fixed.PeriodToMs(ticksPerCycle, msPerTick)
	if err != nil {
		return nil, model.NewError(model.EOvf, "/clock", "cycle duration overflow").WithCause(err)
	}
	windows, err := root.requiredChild("windows", "")
	if err != nil {
		return nil, err
	}
	windowCycles, err := windows.requiredInt("cyclesPerWindow", "/windows")
	if err != nil {
		return nil, err
	}
	windowCount, err := windows.requiredInt("windowCount", "/windows")
	if err != nil {
		return nil, err
	}
	workforce, err := root.requiredChild("workforce", "")
	if err != nil {
		return nil, err
	}
	workersTotal, err := workforce.requiredInt("total", "/workforce")
	if err != nil {
		return nil, err
	}
	workersMaximum, err := workforce.requiredInt("maximum", "/workforce")
	if err != nil {
		return nil, err
	}
	resourceItems, err := root.requiredArray("resources", "")
	if err != nil {
		return nil, err
	}
	resources := make([]model.Resource, 0, len(resourceItems))
	for i := range resourceItems {
		path := model.JSONPointer("resources", fmt.Sprint(i))
		item, err := arrayItem(resourceItems, i, "/resources")
		if err != nil {
			return nil, err
		}
		id, err := item.requiredString("id", path)
		if err != nil {
			return nil, err
		}
		name, err := item.requiredString("name", path)
		if err != nil {
			return nil, err
		}
		typeObj, err := item.requiredChild("resourceType", path)
		if err != nil {
			return nil, err
		}
		code, err := typeObj.requiredString("code", path+"/resourceType")
		if err != nil {
			return nil, err
		}
		kind, err := resourceKindFromString(code, path+"/resourceType/code")
		if err != nil {
			return nil, err
		}
		resources = append(resources, model.Resource{ID: id, Name: name, Kind: kind})
	}
	storage, err := root.requiredChild("storage", "")
	if err != nil {
		return nil, err
	}
	initial, err := requiredIntMap(storage, "initialStock", "/storage")
	if err != nil {
		return nil, err
	}
	capacity, err := requiredIntMap(storage, "capacity", "/storage")
	if err != nil {
		return nil, err
	}
	overflowPresent := storage.has("overflowPolicy")
	overflowPolicy, err := storage.string("overflowPolicy", "/storage", false)
	if err != nil {
		return nil, err
	}
	if !overflowPresent {
		overflowPolicy = model.DefaultOverflowPolicy
	}
	discount, discountPresent, err := root.integer("upgradeDiscountBasisPoints", "", false)
	if err != nil {
		return nil, err
	}
	if !discountPresent {
		discount = model.DefaultUpgradeDiscountBasisPoints
	}
	nodeItems, err := root.requiredArray("nodes", "")
	if err != nil {
		return nil, err
	}
	nodes := make([]*model.Node, 0, len(nodeItems))
	for i := range nodeItems {
		path := model.JSONPointer("nodes", fmt.Sprint(i))
		item, err := arrayItem(nodeItems, i, "/nodes")
		if err != nil {
			return nil, err
		}
		node, err := loadNodeV3(item, path)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	intents, err := loadIntentsV3(root)
	if err != nil {
		return nil, err
	}
	return &model.Model{
		ID:                         projectID,
		Name:                       projectName,
		SourceVersion:              model.SchemaV3,
		ConversionVersion:          conversionVersion,
		CycleMs:                    cycleMs,
		WindowCycles:               int(windowCycles),
		WindowCount:                int(windowCount),
		RateScale:                  10000,
		WorkerScale:                10000,
		WorkersTotal:               workersTotal,
		WorkersMaximum:             workersMaximum,
		Resources:                  resources,
		InitialStocks:              initial,
		Capacities:                 capacity,
		OverflowPolicy:             overflowPolicy,
		UpgradeDiscountBasisPoints: discount,
		Nodes:                      nodes,
		Intents:                    intents,
		Presence: model.Presence{
			WorkersMaximum:  true,
			OverflowPolicy:  overflowPresent,
			UpgradeDiscount: discountPresent,
		},
		RoundMode: roundMode,
	}, nil
}

func loadNodeV3(item jsonObject, path string) (*model.Node, error) {
	id, err := item.requiredString("id", path)
	if err != nil {
		return nil, err
	}
	name, err := item.requiredString("name", path)
	if err != nil {
		return nil, err
	}
	built, err := item.requiredBool("built", path)
	if err != nil {
		return nil, err
	}
	level, err := item.requiredInt("level", path)
	if err != nil {
		return nil, err
	}
	recipeID, err := item.requiredString("recipe", path)
	if err != nil {
		return nil, err
	}
	recipesRaw, err := item.requiredChild("recipes", path)
	if err != nil {
		return nil, err
	}
	recipes := make([]model.Recipe, 0, len(recipesRaw))
	for _, recipeID := range sortedKeys(recipesRaw) {
		recipeObj, err := decodeObject(recipesRaw[recipeID], path+"/recipes/"+recipeID)
		if err != nil {
			return nil, err
		}
		inputs, err := requiredIntMap(recipeObj, "inputs", path+"/recipes/"+recipeID)
		if err != nil {
			return nil, err
		}
		outputs, err := requiredIntMap(recipeObj, "outputs", path+"/recipes/"+recipeID)
		if err != nil {
			return nil, err
		}
		recipes = append(recipes, model.Recipe{ID: recipeID, Inputs: inputs, Outputs: outputs})
	}
	production, err := item.requiredChild("production", path)
	if err != nil {
		return nil, err
	}
	formulaRaw, err := production.required("rateFormula", path+"/production")
	if err != nil {
		return nil, err
	}
	expression, err := parseFormula(formulaRaw, path+"/production/rateFormula")
	if err != nil {
		return nil, err
	}
	workerObj, err := production.requiredChild("workerEfficiency", path+"/production")
	if err != nil {
		return nil, err
	}
	scale, err := workerObj.requiredString("scale", path+"/production/workerEfficiency")
	if err != nil {
		return nil, err
	}
	if scale != "per_ten_thousand" {
		return nil, model.NewError(model.ESchema, path+"/production/workerEfficiency/scale", "v3 requires per_ten_thousand scale")
	}
	worker, err := workerObj.requiredInt("value", path+"/production/workerEfficiency")
	if err != nil {
		return nil, err
	}
	workers, err := item.requiredInt("workers", path)
	if err != nil {
		return nil, err
	}
	buildCost, err := requiredIntMap(item, "buildCost", path)
	if err != nil {
		return nil, err
	}
	upgradeObj, err := item.requiredChild("upgrade", path)
	if err != nil {
		return nil, err
	}
	maxLevel, err := upgradeObj.requiredInt("maxLevel", path+"/upgrade")
	if err != nil {
		return nil, err
	}
	rateAdd, err := upgradeObj.requiredInt("rateAddBasisPoints", path+"/upgrade")
	if err != nil {
		return nil, err
	}
	capacityAdd, err := optionalIntMap(upgradeObj, "capacityAdd", path+"/upgrade")
	if err != nil {
		return nil, err
	}
	upgradeCost, err := requiredIntMap(upgradeObj, "cost", path+"/upgrade")
	if err != nil {
		return nil, err
	}
	return &model.Node{
		ID:                    id,
		Name:                  name,
		Built:                 built,
		Level:                 int(level),
		Recipe:                recipeID,
		Recipes:               recipes,
		RateExpr:              expression,
		WorkerEfficiencyFixed: worker,
		Workers:               int(workers),
		BuildCost:             buildCost,
		Upgrade: model.Upgrade{
			MaxLevel:     int(maxLevel),
			RateAddFixed: rateAdd,
			CapacityAdd:  capacityAdd,
			Cost:         upgradeCost,
		},
	}, nil
}

func loadIntentsV3(root jsonObject) ([]model.Intent, error) {
	items, err := root.requiredArray("intents", "")
	if err != nil {
		return nil, err
	}
	out := make([]model.Intent, 0, len(items))
	for i := range items {
		path := model.JSONPointer("intents", fmt.Sprint(i))
		item, err := arrayItem(items, i, "/intents")
		if err != nil {
			return nil, err
		}
		offset, err := item.requiredInt("offset", path)
		if err != nil {
			return nil, err
		}
		window, err := item.requiredInt("window", path)
		if err != nil {
			return nil, err
		}
		cycle, err := item.requiredInt("cycle", path)
		if err != nil {
			return nil, err
		}
		action, err := item.requiredString("kind", path)
		if err != nil {
			return nil, err
		}
		nodeID, err := item.requiredString("node", path)
		if err != nil {
			return nil, err
		}
		recipe, err := item.string("recipe", path, false)
		if err != nil {
			return nil, err
		}
		workersRaw, workersSet, err := item.integer("workers", path, false)
		if err != nil {
			return nil, err
		}
		var workers *int
		if workersSet {
			value := int(workersRaw)
			workers = &value
		}
		out = append(out, model.Intent{
			Offset:   int(offset),
			Window:   int(window),
			Cycle:    int(cycle),
			Action:   model.IntentType(action),
			NodeID:   nodeID,
			RecipeID: recipe,
			Workers:  workers,
		})
	}
	return out, nil
}

type v3Document struct {
	SpecVersion string `json:"specVersion"`
	Project     struct {
		ProjectID   string `json:"projectId"`
		DisplayName string `json:"displayName"`
	} `json:"project"`
	ConversionRuleVersion string `json:"conversionRuleVersion"`
	Rounding              string `json:"rounding,omitempty"`
	Clock                 struct {
		Unit                string `json:"unit"`
		TicksPerCycle       int64  `json:"ticksPerCycle"`
		MillisecondsPerTick int64  `json:"millisecondsPerTick"`
	} `json:"clock"`
	Windows struct {
		CyclesPerWindow int `json:"cyclesPerWindow"`
		WindowCount     int `json:"windowCount"`
	} `json:"windows"`
	Workforce struct {
		Total   int64 `json:"total"`
		Maximum int64 `json:"maximum"`
	} `json:"workforce"`
	Resources []struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		ResourceType struct {
			Code     string `json:"code"`
			Category string `json:"category"`
		} `json:"resourceType"`
	} `json:"resources"`
	Storage struct {
		InitialStock   map[string]int64 `json:"initialStock"`
		Capacity       map[string]int64 `json:"capacity"`
		OverflowPolicy string           `json:"overflowPolicy"`
	} `json:"storage"`
	UpgradeDiscountBasisPoints int64 `json:"upgradeDiscountBasisPoints"`
	Nodes                      []struct {
		ID         string              `json:"id"`
		Name       string              `json:"name"`
		Built      bool                `json:"built"`
		Level      int                 `json:"level"`
		Recipe     string              `json:"recipe"`
		Recipes    map[string]v1Recipe `json:"recipes"`
		Production struct {
			RateFormula      model.Expr `json:"rateFormula"`
			WorkerEfficiency struct {
				Scale string `json:"scale"`
				Value int64  `json:"value"`
			} `json:"workerEfficiency"`
		} `json:"production"`
		Workers   int              `json:"workers"`
		BuildCost map[string]int64 `json:"buildCost"`
		Upgrade   struct {
			MaxLevel           int              `json:"maxLevel"`
			RateAddBasisPoints int64            `json:"rateAddBasisPoints"`
			CapacityAdd        map[string]int64 `json:"capacityAdd,omitempty"`
			Cost               map[string]int64 `json:"cost"`
		} `json:"upgrade"`
	} `json:"nodes"`
	Intents []struct {
		Offset  int    `json:"offset"`
		Window  int    `json:"window"`
		Cycle   int    `json:"cycle"`
		Kind    string `json:"kind"`
		Node    string `json:"node"`
		Recipe  string `json:"recipe,omitempty"`
		Workers *int   `json:"workers,omitempty"`
	} `json:"intents"`
}

func encodeV3(m *model.Model, opts exportState) ([]byte, error) {
	doc := v3Document{
		SpecVersion:                "v3",
		ConversionRuleVersion:      m.ConversionVersion,
		Rounding:                   string(opts.roundMode),
		UpgradeDiscountBasisPoints: m.UpgradeDiscountBasisPoints,
	}
	doc.Project.ProjectID = m.ID
	doc.Project.DisplayName = m.Name
	doc.Clock.Unit = "tick"
	doc.Clock.MillisecondsPerTick = 1
	doc.Clock.TicksPerCycle = m.CycleMs
	doc.Windows.CyclesPerWindow = m.WindowCycles
	doc.Windows.WindowCount = m.WindowCount
	doc.Workforce.Total = m.WorkersTotal
	doc.Workforce.Maximum = m.WorkersMaximum
	doc.Storage.InitialStock = m.InitialStocks
	doc.Storage.Capacity = m.Capacities
	doc.Storage.OverflowPolicy = m.OverflowPolicy
	if doc.Storage.OverflowPolicy == "" {
		doc.Storage.OverflowPolicy = model.DefaultOverflowPolicy
	}
	for _, resource := range m.Resources {
		category := "natural"
		switch resource.Kind {
		case model.KindCrafted:
			category = "manufactured"
		case model.KindFood:
			category = "consumable"
		case model.KindEnergy:
			category = "utility"
		}
		entry := struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			ResourceType struct {
				Code     string `json:"code"`
				Category string `json:"category"`
			} `json:"resourceType"`
		}{ID: resource.ID, Name: resource.Name}
		entry.ResourceType.Code = string(resource.Kind)
		entry.ResourceType.Category = category
		doc.Resources = append(doc.Resources, entry)
	}
	for _, node := range m.Nodes {
		recipes := make(map[string]v1Recipe, len(node.Recipes))
		for _, recipe := range node.Recipes {
			recipes[recipe.ID] = v1Recipe{Inputs: recipe.Inputs, Outputs: recipe.Outputs}
		}
		entry := struct {
			ID         string              `json:"id"`
			Name       string              `json:"name"`
			Built      bool                `json:"built"`
			Level      int                 `json:"level"`
			Recipe     string              `json:"recipe"`
			Recipes    map[string]v1Recipe `json:"recipes"`
			Production struct {
				RateFormula      model.Expr `json:"rateFormula"`
				WorkerEfficiency struct {
					Scale string `json:"scale"`
					Value int64  `json:"value"`
				} `json:"workerEfficiency"`
			} `json:"production"`
			Workers   int              `json:"workers"`
			BuildCost map[string]int64 `json:"buildCost"`
			Upgrade   struct {
				MaxLevel           int              `json:"maxLevel"`
				RateAddBasisPoints int64            `json:"rateAddBasisPoints"`
				CapacityAdd        map[string]int64 `json:"capacityAdd,omitempty"`
				Cost               map[string]int64 `json:"cost"`
			} `json:"upgrade"`
		}{ID: node.ID, Name: node.Name, Built: node.Built, Level: node.Level, Recipe: node.Recipe,
			Recipes: recipes, Workers: node.Workers, BuildCost: node.BuildCost}
		entry.Production.RateFormula = node.RateExpr
		entry.Production.WorkerEfficiency.Scale = "per_ten_thousand"
		entry.Production.WorkerEfficiency.Value = node.WorkerEfficiencyFixed
		entry.Upgrade.MaxLevel = node.Upgrade.MaxLevel
		entry.Upgrade.RateAddBasisPoints = node.Upgrade.RateAddFixed
		entry.Upgrade.CapacityAdd = node.Upgrade.CapacityAdd
		entry.Upgrade.Cost = node.Upgrade.Cost
		doc.Nodes = append(doc.Nodes, entry)
	}
	for _, intent := range m.Intents {
		doc.Intents = append(doc.Intents, struct {
			Offset  int    `json:"offset"`
			Window  int    `json:"window"`
			Cycle   int    `json:"cycle"`
			Kind    string `json:"kind"`
			Node    string `json:"node"`
			Recipe  string `json:"recipe,omitempty"`
			Workers *int   `json:"workers,omitempty"`
		}{Offset: intent.Offset, Window: intent.Window, Cycle: intent.Cycle, Kind: string(intent.Action), Node: intent.NodeID, Recipe: intent.RecipeID, Workers: intent.Workers})
	}
	return json.MarshalIndent(doc, "", "  ")
}
