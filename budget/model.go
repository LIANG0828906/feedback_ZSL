package budget

import "sort"

// SourceVersion identifies one of the three supported document dialects.
type SourceVersion string

const (
	SourceV1        SourceVersion = "v1-flat-tick"
	SourceV2        SourceVersion = "v2-flat-millis-myriad"
	SourceV3        SourceVersion = "v3-tree-millis-myriad"
	SourceCanonical SourceVersion = "canonical"
)

const (
	AlgorithmVersion          = "algo-int-tick-v2"
	DefaultParamVersion       = "params-int-v1"
	MaxCDRBasisPoints   int64 = 10_000
)

// BP is the internal fixed-point budget unit: one basis point / permyriad.
type BP int64

// FieldTrace records a source field to internal field conversion.
type FieldTrace struct {
	SourceVersion SourceVersion `json:"source_version"`
	Path          string        `json:"path"`
	SourceRaw     string        `json:"source_raw"`
	InternalPath  string        `json:"internal_path"`
	InternalValue string        `json:"internal_value"`
	Conversion    string        `json:"conversion,omitempty"`
	Rounding      Rounding      `json:"rounding,omitempty"`
	Residual      int64         `json:"residual,omitempty"`
}

type Project struct {
	ID            string          `json:"id"`
	Seed          int64           `json:"seed"`
	TickMS        int64           `json:"tick_ms"`
	TickCount     int             `json:"tick_count"`
	SourceVersion SourceVersion   `json:"source_version"`
	Resource      ResourceRule    `json:"resource"`
	Skills        []Skill         `json:"skills"`
	Groups        []CooldownGroup `json:"groups"`
	Intents       []Intent        `json:"intents"`
	Traces        []FieldTrace    `json:"traces,omitempty"`
	ContentHash   string          `json:"content_hash,omitempty"`
}

type ResourceRule struct {
	ID        string `json:"id"`
	Max       BP     `json:"max"`
	Initial   BP     `json:"initial"`
	RegenNode string `json:"regen_node"`
	RegenExpr *Expr  `json:"regen_expr,omitempty"`
	RegenFlat BP     `json:"regen_flat,omitempty"`
}

type Skill struct {
	ID            string `json:"id"`
	Priority      int    `json:"priority"`
	Cost          BP     `json:"cost"`
	CooldownTicks int64  `json:"cooldown_ticks"`
	CDRNode       string `json:"cdr_node"`
	CDRExpr       *Expr  `json:"cdr_expr,omitempty"`
	CDRFlat       int64  `json:"cdr_flat,omitempty"`
	GroupID       string `json:"group_id,omitempty"`
}

type CooldownGroup struct {
	ID            string   `json:"id"`
	CooldownTicks int64    `json:"cooldown_ticks"`
	Members       []string `json:"members,omitempty"`
}

type Intent struct {
	IntentID string `json:"intent_id"`
	Tick     int    `json:"tick"`
	SkillID  string `json:"skill_id"`
	Ordinal  int    `json:"ordinal"`
}

type Params struct {
	ParamVersion       string `json:"param_version"`
	WindowTicks        int    `json:"window_ticks"`
	StrictPriorityTies bool   `json:"strict_priority_ties"`
	// FailTick and FailSkillID inject one deterministic stage failure. A
	// negative FailTick disables the hook.
	FailTick    int    `json:"fail_tick,omitempty"`
	FailSkillID string `json:"fail_skill_id,omitempty"`

	// OnWindowCommitted is an in-process test/cancellation hook. It is not
	// persisted and must not affect deterministic settlement.
	OnWindowCommitted func(WindowCommit) `json:"-"`
}

func DefaultParams() Params {
	return Params{ParamVersion: DefaultParamVersion, WindowTicks: 4, FailTick: -1}
}

type SkillState struct {
	SkillID           string `json:"skill_id"`
	Available         bool   `json:"available"`
	CastableBefore    bool   `json:"castable_before_settlement"`
	CooldownRemaining int64  `json:"cooldown_remaining"`
}

type GroupState struct {
	GroupID           string `json:"group_id"`
	CooldownRemaining int64  `json:"cooldown_remaining"`
}

type CastRecord struct {
	IntentID          string `json:"intent_id"`
	SkillID           string `json:"skill_id"`
	Accepted          bool   `json:"accepted"`
	CostPaid          BP     `json:"cost_paid"`
	EffectiveCooldown int64  `json:"effective_cooldown"`
}

type TickState struct {
	Tick                 int          `json:"tick"`
	ResourceCurrent      BP           `json:"resource_current"`
	ResourceMax          BP           `json:"resource_max"`
	RegenApplied         BP           `json:"regen_applied"`
	Spent                BP           `json:"spent"`
	Skills               []SkillState `json:"skills"`
	Groups               []GroupState `json:"groups"`
	Casts                []CastRecord `json:"casts,omitempty"`
	ConsumedIntentOffset int          `json:"consumed_intent_offset"`
	Dependencies         []string     `json:"dependencies,omitempty"`
	CacheKey             string       `json:"cache_key,omitempty"`
}

type ArbitrationRecord struct {
	Tick       int      `json:"tick"`
	IntentID   string   `json:"intent_id"`
	SkillID    string   `json:"skill_id"`
	Priority   int      `json:"priority"`
	Ordinal    int      `json:"ordinal"`
	Contention string   `json:"contention"`
	Decision   string   `json:"decision"`
	Reason     string   `json:"reason"`
	Basis      string   `json:"basis"`
	Winners    []string `json:"winners,omitempty"`
}

type TickResult struct {
	State       TickState           `json:"state"`
	Arbitration []ArbitrationRecord `json:"arbitration"`
}

type TickRange struct {
	First int `json:"first"`
	Last  int `json:"last"`
}

type WindowCommit struct {
	WindowIndex  int    `json:"window_index"`
	FirstTick    int    `json:"first_tick"`
	LastTick     int    `json:"last_tick"`
	IntentOffset int    `json:"intent_offset"`
	Path         string `json:"path,omitempty"`
	CacheKey     string `json:"cache_key,omitempty"`
}

type RunReport struct {
	SourceVersion    SourceVersion         `json:"source_version"`
	ContentHash      string                `json:"content_hash"`
	AlgorithmVersion string                `json:"algorithm_version"`
	ParamVersion     string                `json:"param_version"`
	Ticks            []TickState           `json:"ticks"`
	Arbitration      [][]ArbitrationRecord `json:"arbitration"`
	Commits          []WindowCommit        `json:"commits,omitempty"`
}

func (r *RunReport) recordsFlat() []ArbitrationRecord {
	var out []ArbitrationRecord
	for _, group := range r.Arbitration {
		out = append(out, group...)
	}
	return out
}

func sortedSkillIDs(skills []Skill) []string {
	ids := make([]string, 0, len(skills))
	for _, skill := range skills {
		ids = append(ids, skill.ID)
	}
	sort.Strings(ids)
	return ids
}

func (p Project) skillByID(id string) (Skill, bool) {
	for _, skill := range p.Skills {
		if skill.ID == id {
			return skill, true
		}
	}
	return Skill{}, false
}

func (p Project) groupByID(id string) (CooldownGroup, bool) {
	for _, group := range p.Groups {
		if group.ID == id {
			return group, true
		}
	}
	return CooldownGroup{}, false
}
