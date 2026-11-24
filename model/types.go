// Package model defines the canonical fixed-point economy domain model,
// validation, expression evaluation, dependency graph and checkpoint identity.
package model

import "time"

const (
	SchemaV1 = "v1"
	SchemaV2 = "v2"
	SchemaV3 = "v3"

	DefaultConversionVersion = "conv-1.0.0"
	DefaultAlgorithmVersion  = "settle-fixed-1.0.0"

	DefaultWorkersMaximum             int64 = 100
	DefaultOverflowPolicy                   = "cap_drop"
	DefaultUpgradeDiscountBasisPoints int64 = 0
)

// ResourceKind is the canonical resource category.
type ResourceKind string

const (
	KindRaw     ResourceKind = "raw"
	KindCrafted ResourceKind = "crafted"
	KindFood    ResourceKind = "food"
	KindEnergy  ResourceKind = "energy"
)

// Resource is a resource definition. Kind is encoded differently in each
// external schema.
type Resource struct {
	ID   string       `json:"id"`
	Name string       `json:"name"`
	Kind ResourceKind `json:"kind"`
}

// Recipe is a whole-batch transformation. Quantities are whole resource units
// per completed batch.
type Recipe struct {
	ID      string           `json:"id"`
	Inputs  map[string]int64 `json:"inputs"`
	Outputs map[string]int64 `json:"outputs"`
}

// Upgrade describes the increment applied by one upgrade step.
type Upgrade struct {
	MaxLevel     int              `json:"maxLevel"`
	RateAddFixed int64            `json:"rateAddFixed"`
	CapacityAdd  map[string]int64 `json:"capacityAdd,omitempty"`
	Cost         map[string]int64 `json:"cost"`
}

// Node is a production-chain node. RateExpr is canonical and always present;
// v1's flat rate is represented as a const expression.
type Node struct {
	ID                    string           `json:"id"`
	Name                  string           `json:"name"`
	Built                 bool             `json:"built"`
	Level                 int              `json:"level"`
	Recipe                string           `json:"recipe"`
	Recipes               []Recipe         `json:"recipes"`
	RateExpr              Expr             `json:"rateExpr"`
	WorkerEfficiencyFixed int64            `json:"workerEfficiencyFixed"`
	Workers               int              `json:"workers"`
	BuildCost             map[string]int64 `json:"buildCost"`
	Upgrade               Upgrade          `json:"upgrade"`
}

// RecipeByID returns the selected recipe definition.
func (n *Node) RecipeByID(id string) (Recipe, bool) {
	for _, recipe := range n.Recipes {
		if recipe.ID == id {
			return recipe, true
		}
	}
	return Recipe{}, false
}

// ConstantRate returns the expression's constant value when it has no runtime
// variables.
func (n *Node) ConstantRate() (int64, bool, error) {
	if n.RateExpr.HasVariable() {
		return 0, false, nil
	}
	value, err := n.RateExpr.Eval(nil)
	return value, err == nil, err
}

// IntentType is a player action.
type IntentType string

const (
	IntentBuild         IntentType = "build"
	IntentUpgrade       IntentType = "upgrade"
	IntentDemolish      IntentType = "demolish"
	IntentAssignWorkers IntentType = "assign_workers"
	IntentSwitchRecipe  IntentType = "switch_recipe"
)

// Intent is one deterministic player operation. Window and Cycle are zero
// based; Offset is the stable intent cursor persisted in checkpoints.
type Intent struct {
	Offset   int        `json:"offset"`
	Window   int        `json:"window"`
	Cycle    int        `json:"cycle"`
	Action   IntentType `json:"action"`
	NodeID   string     `json:"nodeId"`
	RecipeID string     `json:"recipeId,omitempty"`
	Workers  *int       `json:"workers,omitempty"`
}

// GlobalCycle returns the absolute cycle at which the intent is applied.
func (i Intent) GlobalCycle(windowCycles int) int64 {
	return int64(i.Window*windowCycles + i.Cycle)
}

// Presence records which version-dependent optional domain fields were
// actually present in the source document.
type Presence struct {
	WorkersMaximum  bool `json:"workersMaximum"`
	OverflowPolicy  bool `json:"overflowPolicy"`
	UpgradeDiscount bool `json:"upgradeDiscount"`
}

// ConversionTrace records a default insertion performed by an explicit
// upgrade rule.
type ConversionTrace struct {
	Path          string `json:"path"`
	OldValue      any    `json:"oldValue"`
	DefaultValue  any    `json:"defaultValue"`
	DefaultSource string `json:"defaultSource"`
	RuleVersion   string `json:"ruleVersion"`
	FromVersion   string `json:"fromVersion"`
	ToVersion     string `json:"toVersion"`
}

// Loss describes one downgrade disposition.
type Loss struct {
	Path        string `json:"path"`
	NodeID      string `json:"nodeId,omitempty"`
	Field       string `json:"field,omitempty"`
	OldValue    any    `json:"oldValue"`
	Disposition string `json:"disposition"`
	Reason      string `json:"reason"`
}

// Model is the unified internal economic model.
type Model struct {
	ID                         string           `json:"id"`
	Name                       string           `json:"name"`
	SourceVersion              string           `json:"sourceVersion"`
	ConversionVersion          string           `json:"conversionVersion"`
	CycleMs                    int64            `json:"cycleMs"`
	WindowCycles               int              `json:"windowCycles"`
	WindowCount                int              `json:"windowCount"`
	RateScale                  int64            `json:"rateScale"`
	WorkerScale                int64            `json:"workerScale"`
	WorkersTotal               int64            `json:"workersTotal"`
	WorkersMaximum             int64            `json:"workersMaximum"`
	Resources                  []Resource       `json:"resources"`
	InitialStocks              map[string]int64 `json:"initialStocks"`
	Capacities                 map[string]int64 `json:"capacities"`
	OverflowPolicy             string           `json:"overflowPolicy"`
	UpgradeDiscountBasisPoints int64            `json:"upgradeDiscountBasisPoints"`
	Nodes                      []*Node          `json:"nodes"`
	Intents                    []Intent         `json:"intents"`
	Presence                   Presence         `json:"presence"`
	RoundMode                  string           `json:"roundMode"`
}

// TotalCycles is the number of logical cycles described by the project.
func (m *Model) TotalCycles() int64 {
	return int64(m.WindowCycles * m.WindowCount)
}

// ResourceMap indexes resource definitions by ID.
func (m *Model) ResourceMap() map[string]Resource {
	out := make(map[string]Resource, len(m.Resources))
	for _, resource := range m.Resources {
		out[resource.ID] = resource
	}
	return out
}

// NodeMap indexes nodes by ID.
func (m *Model) NodeMap() map[string]*Node {
	out := make(map[string]*Node, len(m.Nodes))
	for _, node := range m.Nodes {
		out[node.ID] = node
	}
	return out
}

// Normalize fills implicit zero stocks and capacities for every declared
// resource. It does not change wire semantics because an absent value and an
// explicit zero are equivalent.
func (m *Model) Normalize() {
	if m == nil {
		return
	}
	if m.RateScale == 0 {
		m.RateScale = 10000
	}
	if m.WorkerScale == 0 {
		m.WorkerScale = 10000
	}
	if m.WorkersMaximum == 0 {
		m.WorkersMaximum = DefaultWorkersMaximum
		if m.WorkersTotal > m.WorkersMaximum {
			m.WorkersMaximum = m.WorkersTotal
		}
	}
	if m.OverflowPolicy == "" {
		m.OverflowPolicy = DefaultOverflowPolicy
	}
	if m.InitialStocks == nil {
		m.InitialStocks = make(map[string]int64)
	}
	if m.Capacities == nil {
		m.Capacities = make(map[string]int64)
	}
	for _, resource := range m.Resources {
		if _, ok := m.InitialStocks[resource.ID]; !ok {
			m.InitialStocks[resource.ID] = 0
		}
		if _, ok := m.Capacities[resource.ID]; !ok {
			m.Capacities[resource.ID] = 0
		}
	}
}

// Clone returns a deep copy.
func (m *Model) Clone() *Model {
	if m == nil {
		return nil
	}
	clone := *m
	clone.Resources = append([]Resource(nil), m.Resources...)
	clone.InitialStocks = cloneIntMap(m.InitialStocks)
	clone.Capacities = cloneIntMap(m.Capacities)
	clone.Nodes = make([]*Node, len(m.Nodes))
	for i, node := range m.Nodes {
		clone.Nodes[i] = node.Clone()
	}
	clone.Intents = make([]Intent, len(m.Intents))
	for i, intent := range m.Intents {
		clone.Intents[i] = intent
		if intent.Workers != nil {
			value := *intent.Workers
			clone.Intents[i].Workers = &value
		}
	}
	return &clone
}

// Clone deep-copies a node.
func (n *Node) Clone() *Node {
	if n == nil {
		return nil
	}
	clone := *n
	clone.Recipes = make([]Recipe, len(n.Recipes))
	for i, recipe := range n.Recipes {
		clone.Recipes[i] = Recipe{
			ID:      recipe.ID,
			Inputs:  cloneIntMap(recipe.Inputs),
			Outputs: cloneIntMap(recipe.Outputs),
		}
	}
	clone.RateExpr = n.RateExpr.Clone()
	clone.BuildCost = cloneIntMap(n.BuildCost)
	clone.Upgrade = Upgrade{
		MaxLevel:     n.Upgrade.MaxLevel,
		RateAddFixed: n.Upgrade.RateAddFixed,
		CapacityAdd:  cloneIntMap(n.Upgrade.CapacityAdd),
		Cost:         cloneIntMap(n.Upgrade.Cost),
	}
	return &clone
}

func cloneIntMap(in map[string]int64) map[string]int64 {
	if in == nil {
		return map[string]int64{}
	}
	out := make(map[string]int64, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// Diff identifies the first incremental/full mismatch.
type Diff struct {
	Cycle       int64  `json:"cycle"`
	Window      int    `json:"window"`
	NodeID      string `json:"nodeId,omitempty"`
	Field       string `json:"field"`
	Incremental any    `json:"incremental"`
	Full        any    `json:"full"`
}

// Fingerprint is the all-or-nothing checkpoint compatibility identity.
type Fingerprint struct {
	ContentHash       string `json:"contentHash"`
	SourceVersion     string `json:"sourceVersion"`
	ConversionVersion string `json:"conversionVersion"`
	AlgorithmVersion  string `json:"algorithmVersion"`
}

// RuntimeState is a JSON-serializable checkpointable engine state.
type RuntimeState struct {
	NextCycle      int64                  `json:"nextCycle"`
	IntentIndex    int                    `json:"intentIndex"`
	Stocks         map[string]int64       `json:"stocks"`
	Capacities     map[string]int64       `json:"capacities"`
	Nodes          map[string]NodeRuntime `json:"nodes"`
	OverflowTotals map[string]int64       `json:"overflowTotals"`
}

// NodeRuntime is the persisted state of one node.
type NodeRuntime struct {
	Built           bool   `json:"built"`
	Level           int    `json:"level"`
	Recipe          string `json:"recipe"`
	Workers         int    `json:"workers"`
	Progress        int64  `json:"progress"`
	Stalled         bool   `json:"stalled"`
	StallStartCycle int64  `json:"stallStartCycle"`
	RateBonusFixed  int64  `json:"rateBonusFixed"`
}

// NowUTC is injectable for deterministic tests and future audit fields.
var NowUTC = func() time.Time { return time.Now().UTC() }
