package plancache

import "sort"

const (
	SchemaV1 = "plan-project/v1"
	SchemaV2 = "plan-project/v2"
	SchemaV3 = "plan-project/v3"

	CheckpointSchemaVersion = "checkpoint/v1"
)

// Project is the only canonical representation used by key derivation,
// reuse, invalidation, admission and checkpoint code.
type Project struct {
	SchemaVersion      string                 `json:"schema_version"`
	ProjectID          string                 `json:"project_id"`
	EngineeringVersion string                 `json:"engineering_version"`
	GeneratorSeed      int64                  `json:"generator_seed"`
	TickDurationNS     int64                  `json:"tick_duration_ns"`
	Statements         []Statement            `json:"statements"`
	KeySettingNames    []string               `json:"key_setting_names"`
	DefaultSettings    map[string]string      `json:"default_settings"`
	CompatibilityRules map[string]SettingRule `json:"compatibility_rules"`
	Capacity           Capacity               `json:"capacity"`
	Eviction           EvictionPolicy         `json:"eviction"`
	Runtime            RuntimeBudget          `json:"runtime"`
	PropagationDepth   int                    `json:"propagation_depth"`
	Requests           []SessionRequest       `json:"requests"`
	InvalidationEdits  []InvalidationEdit     `json:"invalidation_edits"`
}

type SettingRule struct {
	Mode          string   `json:"mode"`
	AllowedValues []string `json:"allowed_values,omitempty"`
	Minimum       int64    `json:"minimum,omitempty"`
}

type Capacity struct {
	MaxPlansPerStatement int   `json:"max_plans_per_statement"`
	MaxCacheBytes        int64 `json:"max_cache_bytes"`
}

type EvictionPolicy struct {
	TickGranularityMS int64  `json:"tick_granularity_ms"`
	PerStatementRule  string `json:"per_statement_rule"`
	TotalBytesRule    string `json:"total_bytes_rule"`
	TieBreakRule      string `json:"tie_break_rule"`
}

type RuntimeBudget struct {
	Workers       int `json:"workers"`
	SessionBuffer int `json:"session_buffer"`
}

type ParamSpec struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
	Type  string `json:"type"`
}

type NodeRef struct {
	StatementID string `json:"statement_id,omitempty"`
	SubplanID   string `json:"subplan_id,omitempty"`
	PlanID      string `json:"plan_id,omitempty"`
}

type Node struct {
	ID              string   `json:"id"`
	SubplanID       string   `json:"subplan_id"`
	Op              string   `json:"op"`
	Label           string   `json:"label,omitempty"`
	Arguments       []int    `json:"arguments,omitempty"`
	Children        []string `json:"children,omitempty"`
	Ref             *NodeRef `json:"ref,omitempty"`
	EstimatedCostNS int64    `json:"estimated_cost_ns"`
}

type Subplan struct {
	ID     string `json:"id"`
	RootID string `json:"root_id"`
	Nodes  []Node `json:"nodes"`
}

type Statement struct {
	ID            string      `json:"id"`
	Template      string      `json:"template"`
	Params        []ParamSpec `json:"params"`
	MainSubplanID string      `json:"main_subplan_id"`
	Subplans      []Subplan   `json:"subplans"`
	StatsVersion  string      `json:"stats_version"`
	Fingerprint   string      `json:"fingerprint"`
}

type SessionRequest struct {
	Seq         int               `json:"seq"`
	RequestID   string            `json:"request_id"`
	SessionID   string            `json:"session_id"`
	StatementID string            `json:"statement_id"`
	Settings    map[string]string `json:"settings,omitempty"`
}

type InvalidationEdit struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"`
	StatementID    string `json:"statement_id"`
	ParameterIndex int    `json:"parameter_index,omitempty"`
	OldValue       string `json:"old_value,omitempty"`
	NewValue       string `json:"new_value,omitempty"`
	Source         string `json:"source"`
}

// EffectiveSettings applies project defaults and returns a sorted copy.
func (p *Project) EffectiveSettings(overrides map[string]string) map[string]string {
	out := make(map[string]string, len(p.DefaultSettings)+len(overrides))
	for k, v := range p.DefaultSettings {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

func (p *Project) SortedKeySettingNames() []string {
	out := append([]string(nil), p.KeySettingNames...)
	sort.Strings(out)
	return out
}

func (s *Statement) ParamTypes() []string {
	out := make([]string, len(s.Params))
	for i, p := range s.Params {
		out[i] = p.Type
	}
	return out
}

func (s *Statement) FindNode(id string) (Node, bool) {
	for _, sp := range s.Subplans {
		for _, n := range sp.Nodes {
			if n.ID == id {
				return n, true
			}
		}
	}
	return Node{}, false
}

// TraversalOrder returns main subplan preorder followed by the other declared
// subplan groups in their stable declaration order.
func (s *Statement) TraversalOrder() []string {
	byID := map[string]Node{}
	roots := make([]string, 0, len(s.Subplans))
	for _, sp := range s.Subplans {
		roots = append(roots, sp.RootID)
		for _, n := range sp.Nodes {
			byID[n.ID] = n
		}
	}
	order := make([]string, 0, len(byID))
	seen := map[string]bool{}
	var visit func(string)
	visit = func(id string) {
		if id == "" || seen[id] {
			return
		}
		n, ok := byID[id]
		if !ok {
			return
		}
		seen[id] = true
		order = append(order, id)
		for _, child := range n.Children {
			visit(child)
		}
	}
	mainRoot := ""
	for _, sp := range s.Subplans {
		if sp.ID == s.MainSubplanID {
			mainRoot = sp.RootID
		}
	}
	visit(mainRoot)
	for _, root := range roots {
		visit(root)
	}
	// Defensive stable append for any orphan node in a malformed but loaded
	// document; strict validation normally rejects such trees.
	remaining := make([]string, 0)
	for id := range byID {
		if !seen[id] {
			remaining = append(remaining, id)
		}
	}
	sort.Strings(remaining)
	for _, id := range remaining {
		visit(id)
	}
	return order
}
