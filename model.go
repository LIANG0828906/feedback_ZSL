package balance

const (
	SchemaVersion    = "battle-balance/v1"
	AlgorithmVersion = "fixed-combat-v1"
	BudgetVersion    = "budget-v1"
)

type Project struct {
	SchemaVersion    string      `json:"schemaVersion"`
	Seed             int64       `json:"seed"`
	AlgorithmVersion string      `json:"algorithmVersion"`
	BudgetVersion    string      `json:"budgetVersion"`
	Classes          []Class     `json:"classes"`
	Skills           []Skill     `json:"skills"`
	Formulas         []Formula   `json:"formulas"`
	Builds           []Build     `json:"builds"`
	Scenarios        []Scenario  `json:"scenarios"`
	Plans            []Plan      `json:"plans"`
	Thresholds       []Threshold `json:"thresholds"`
	Budget           Budget      `json:"budget"`
}

type Class struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Skill struct {
	ID                string `json:"id"`
	ClassID           string `json:"classId"`
	FormulaID         string `json:"formulaId"`
	BaseDamage        Fixed  `json:"baseDamage"`
	GrowthCoefficient Fixed  `json:"growthCoefficient"`
	Cooldown          int    `json:"cooldown"`
	ResourceCost      Fixed  `json:"resourceCost"`
	HitVarianceMinBPS int    `json:"hitVarianceMinBps"`
	HitVarianceMaxBPS int    `json:"hitVarianceMaxBps"`
}

type Formula struct {
	ID     string   `json:"id"`
	Op     string   `json:"op"`
	Value  Fixed    `json:"value"`
	Ref    string   `json:"ref,omitempty"`
	Inputs []string `json:"inputs,omitempty"`
}

type Build struct {
	ID            string           `json:"id"`
	ClassID       string           `json:"classId"`
	MaxHP         Fixed            `json:"maxHp"`
	MaxResource   Fixed            `json:"maxResource"`
	ResourceRegen Fixed            `json:"resourceRegen"`
	Attributes    map[string]Fixed `json:"attributes"`
	Rotation      []string         `json:"rotation"`
}

type Scenario struct {
	ID           string   `json:"id"`
	Kind         string   `json:"kind"`
	LeftBuilds   []string `json:"leftBuilds"`
	RightBuilds  []string `json:"rightBuilds"`
	MaxRounds    int      `json:"maxRounds"`
	ReviveRounds int      `json:"reviveRounds"`
}

type Plan struct {
	ID              string            `json:"id"`
	Priority        int               `json:"priority"`
	TargetScenarios []string          `json:"targetScenarios"`
	Adjustments     []SkillAdjustment `json:"adjustments"`
}

type SkillAdjustment struct {
	SkillID                string `json:"skillId"`
	BaseDamageDelta        Fixed  `json:"baseDamageDelta"`
	GrowthCoefficientDelta Fixed  `json:"growthCoefficientDelta"`
	ResourceCostDelta      Fixed  `json:"resourceCostDelta"`
	CooldownDelta          int    `json:"cooldownDelta"`
}

type Threshold struct {
	ID     string `json:"id"`
	PlanID string `json:"planId,omitempty"`
	Metric string `json:"metric"`
	Min    int64  `json:"min"`
	Max    int64  `json:"max"`
}

type Budget struct {
	Slots                 int    `json:"slots"`
	MaxMatchesPerRound    int    `json:"maxMatchesPerRound"`
	MemoryBuffer          int    `json:"memoryBuffer"`
	MaxRounds             int    `json:"maxRounds"`
	TargetMatchesPerCombo int    `json:"targetMatchesPerCombo"`
	MaxTotalMatches       int    `json:"maxTotalMatches"`
	PriorityRule          string `json:"priorityRule"`
}

const (
	ScenarioDuel  = "duel"
	ScenarioSquad = "squad"

	PriorityFirst = "priority_first"
	PriorityEqual = "equal_coverage"
)
