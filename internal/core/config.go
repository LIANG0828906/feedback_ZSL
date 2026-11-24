package core

import (
	"encoding/json"
	"fmt"
	"sort"
)

type FieldMapping struct {
	Role string `json:"role"`
	Path string `json:"path"`
}

type ListMapping struct {
	Role string `json:"role"`
	Path string `json:"path"`
}

type TimeMapping struct {
	Path         string `json:"path"`
	EpochSeconds int64  `json:"epochSeconds"`
	TickNanos    int64  `json:"tickNanos"`
	Rounding     string `json:"rounding"`
}

type FlagMapping struct {
	Semantic string `json:"semantic"`
	Path     string `json:"path"`
	Invert   bool   `json:"invert"`
}

// ConditionRule is a small, declarative boolean graph. Op is one of flag, and,
// or, not. For flag, Flag is a semantic flag name resolved through FlagMapping.
type ConditionRule struct {
	ID   string   `json:"id"`
	Op   string   `json:"op"`
	Flag string   `json:"flag,omitempty"`
	Refs []string `json:"refs,omitempty"`
}

type ConditionalMapping struct {
	Path        string `json:"path"`
	Role        string `json:"role"`
	ConditionID string `json:"conditionId"`
	Default     int64  `json:"default"`
}

type ConflictPolicy struct {
	Action   string `json:"action"`
	Priority int    `json:"priority"`
}

type BranchSpec struct {
	ID            string               `json:"id"`
	DisplayName   string               `json:"displayName"`
	Fields        []FieldMapping       `json:"fields"`
	Lists         []ListMapping        `json:"lists"`
	Time          TimeMapping          `json:"time"`
	Flags         []FlagMapping        `json:"flags"`
	Conditions    []ConditionRule      `json:"conditions"`
	Conditionals  []ConditionalMapping `json:"conditionals"`
	ConflictRules []ConflictPolicy     `json:"conflictRules"`
}

type MappingTable struct {
	Version  string       `json:"version"`
	Branches []BranchSpec `json:"branches"`
}

type MappingDescriptor struct {
	Branch       string `json:"branch"`
	Role         string `json:"role"`
	Kind         string `json:"kind"`
	PhysicalPath string `json:"physicalPath"`
	SemanticPath string `json:"semanticPath"`
}

func (t *MappingTable) Branch(id string) (BranchSpec, bool) {
	for _, branch := range t.Branches {
		if branch.ID == id {
			return branch, true
		}
	}
	return BranchSpec{}, false
}

func (t MappingTable) Query(branchID, role string) []MappingDescriptor {
	branch, ok := t.Branch(branchID)
	if !ok {
		return nil
	}
	out := make([]MappingDescriptor, 0)
	for _, field := range branch.Fields {
		if role == "" || field.Role == role {
			out = append(out, MappingDescriptor{
				Branch: branch.ID, Role: field.Role, Kind: "int",
				PhysicalPath: field.Path, SemanticPath: semanticPath(field.Role),
			})
		}
	}
	for _, list := range branch.Lists {
		if role == "" || list.Role == role {
			out = append(out, MappingDescriptor{
				Branch: branch.ID, Role: list.Role, Kind: "string-list",
				PhysicalPath: list.Path, SemanticPath: semanticPath(list.Role),
			})
		}
	}
	if role == "" || role == "time" {
		out = append(out, MappingDescriptor{
			Branch: branch.ID, Role: "time", Kind: "unix-nanos",
			PhysicalPath: branch.Time.Path, SemanticPath: "timeNanos",
		})
	}
	for _, cond := range branch.Conditionals {
		if role == "" || cond.Role == role || role == "conditional" {
			out = append(out, MappingDescriptor{
				Branch: branch.ID, Role: cond.Role, Kind: "conditional-int",
				PhysicalPath: cond.Path, SemanticPath: "conditionals[" + cond.Role + "]",
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Role != out[j].Role {
			return out[i].Role < out[j].Role
		}
		return out[i].PhysicalPath < out[j].PhysicalPath
	})
	return out
}

func (t MappingTable) QueryAll() []MappingDescriptor {
	out := make([]MappingDescriptor, 0)
	for _, branch := range t.Branches {
		out = append(out, t.Query(branch.ID, "")...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Branch != out[j].Branch {
			return out[i].Branch < out[j].Branch
		}
		if out[i].Role != out[j].Role {
			return out[i].Role < out[j].Role
		}
		return out[i].PhysicalPath < out[j].PhysicalPath
	})
	return out
}

func semanticPath(role string) string {
	switch role {
	case "level":
		return "level"
	case "coins":
		return "coins"
	case "experience":
		return "experience"
	case "items":
		return "items"
	case "quests":
		return "quests"
	case "coordinate":
		return "coordinate"
	default:
		return role
	}
}

func (t MappingTable) MarshalCanonical() ([]byte, error) {
	return json.MarshalIndent(t, "", "  ")
}

func (b BranchSpec) Condition(id string) (ConditionRule, bool) {
	for _, condition := range b.Conditions {
		if condition.ID == id {
			return condition, true
		}
	}
	return ConditionRule{}, false
}

func (b BranchSpec) FlagBySemantic(name string) (FlagMapping, bool) {
	for _, flag := range b.Flags {
		if flag.Semantic == name {
			return flag, true
		}
	}
	return FlagMapping{}, false
}

func (b BranchSpec) Validate() error {
	seenRoles := make(map[string]bool)
	for _, field := range b.Fields {
		if seenRoles[field.Role] {
			return fmt.Errorf("branch %s duplicate field role %s", b.ID, field.Role)
		}
		seenRoles[field.Role] = true
	}
	if b.Time.TickNanos <= 0 {
		return fmt.Errorf("branch %s tickNanos must be positive", b.ID)
	}
	if b.Time.Rounding != RoundFloor && b.Time.Rounding != RoundTowardZero {
		return fmt.Errorf("branch %s has unsupported rounding %q", b.ID, b.Time.Rounding)
	}
	for _, cond := range b.Conditionals {
		if _, ok := b.Condition(cond.ConditionID); !ok {
			return fmt.Errorf("branch %s conditional %s references unknown condition %s", b.ID, cond.Path, cond.ConditionID)
		}
	}
	for _, condition := range b.Conditions {
		for _, ref := range condition.Refs {
			if _, ok := b.Condition(ref); !ok {
				return fmt.Errorf("branch %s condition %s references unknown condition %s", b.ID, condition.ID, ref)
			}
		}
	}
	seenConditions := make(map[string]bool)
	for _, condition := range b.Conditions {
		if seenConditions[condition.ID] {
			return fmt.Errorf("branch %s duplicate condition id %s", b.ID, condition.ID)
		}
		seenConditions[condition.ID] = true
	}
	if err := b.validateConditionCycles(); err != nil {
		return err
	}
	seenPriority := make(map[int]bool)
	for _, rule := range b.ConflictRules {
		if seenPriority[rule.Priority] {
			return fmt.Errorf("branch %s duplicate conflict priority %d", b.ID, rule.Priority)
		}
		seenPriority[rule.Priority] = true
		switch rule.Action {
		case "preserve", "default", "reject":
		default:
			return fmt.Errorf("branch %s has unsupported conflict action %q", b.ID, rule.Action)
		}
	}
	return nil
}

func (b BranchSpec) validateConditionCycles() error {
	ids := make([]string, 0, len(b.Conditions))
	for _, condition := range b.Conditions {
		ids = append(ids, condition.ID)
	}
	sort.Strings(ids)
	state := make(map[string]uint8, len(ids))
	var visit func(string, []string) error
	visit = func(id string, stack []string) error {
		switch state[id] {
		case 2:
			return nil
		case 1:
			for i, current := range stack {
				if current == id {
					chain := append(append([]string(nil), stack[i:]...), id)
					return &Error{
						Code: CodeCycle, Op: "validate-conditions",
						Message: "cyclic condition reference", Branch: b.ID,
						Conditional: id, Chain: chain, Window: -1,
					}
				}
			}
		}
		state[id] = 1
		condition, ok := b.Condition(id)
		if !ok {
			return nil
		}
		for _, ref := range condition.Refs {
			if err := visit(ref, append(stack, id)); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for _, id := range ids {
		if err := visit(id, nil); err != nil {
			return err
		}
	}
	return nil
}

// DemoMappingTable declares three release branches with the same container
// depth but different physical names and semantics.
func DemoMappingTable() MappingTable {
	return MappingTable{
		Version: DefaultRuleVersion,
		Branches: []BranchSpec{
			{
				ID:          "aurora",
				DisplayName: "Aurora PC",
				Fields: []FieldMapping{
					{Role: "level", Path: "progress.level"},
					{Role: "coins", Path: "stats.valueA"},
					{Role: "experience", Path: "stats.valueB"},
					{Role: "coordinate.x", Path: "world.pos.x"},
					{Role: "coordinate.y", Path: "world.pos.y"},
				},
				Lists: []ListMapping{
					{Role: "items", Path: "inventory.primary"},
					{Role: "quests", Path: "inventory.secondary"},
				},
				Time: TimeMapping{
					Path: "clock.ticks", EpochSeconds: 1_700_000_000,
					TickNanos: 1_000_000, Rounding: RoundFloor,
				},
				Flags: []FlagMapping{
					{Semantic: "event_enabled", Path: "flags.eventOn"},
				},
				Conditions: []ConditionRule{
					{ID: "event_on", Op: "flag", Flag: "event_enabled"},
				},
				Conditionals: []ConditionalMapping{
					{Path: "bonus.coins", Role: "coins", ConditionID: "event_on"},
					{Path: "bonus.exp", Role: "experience", ConditionID: "event_on"},
				},
				ConflictRules: defaultConflictRules(),
			},
			{
				ID:          "ember",
				DisplayName: "Ember Console",
				Fields: []FieldMapping{
					{Role: "level", Path: "progress.level"},
					{Role: "coins", Path: "stats.valueB"},
					{Role: "experience", Path: "stats.valueA"},
					{Role: "coordinate.x", Path: "world.pos.x"},
					{Role: "coordinate.y", Path: "world.pos.y"},
				},
				Lists: []ListMapping{
					{Role: "items", Path: "inventory.secondary"},
					{Role: "quests", Path: "inventory.primary"},
				},
				Time: TimeMapping{
					Path: "clock.ticks", EpochSeconds: 1_600_000_000,
					TickNanos: 1_000_000_000, Rounding: RoundFloor,
				},
				Flags: []FlagMapping{
					{Semantic: "event_enabled", Path: "flags.eventOn", Invert: true},
				},
				Conditions: []ConditionRule{
					{ID: "event_on", Op: "flag", Flag: "event_enabled"},
				},
				Conditionals: []ConditionalMapping{
					{Path: "bonus.coins", Role: "coins", ConditionID: "event_on"},
					{Path: "bonus.exp", Role: "experience", ConditionID: "event_on"},
				},
				ConflictRules: defaultConflictRules(),
			},
			{
				ID:          "nova",
				DisplayName: "Nova Handheld",
				Fields: []FieldMapping{
					{Role: "level", Path: "progress.level"},
					{Role: "coins", Path: "wallet.amountA"},
					{Role: "experience", Path: "wallet.amountB"},
					{Role: "coordinate.x", Path: "world.pos.x"},
					{Role: "coordinate.y", Path: "world.pos.y"},
				},
				Lists: []ListMapping{
					{Role: "items", Path: "bags.listA"},
					{Role: "quests", Path: "bags.listB"},
				},
				Time: TimeMapping{
					Path: "timestamp.raw", EpochSeconds: 946_684_800,
					TickNanos: 1_000, Rounding: RoundFloor,
				},
				Flags: []FlagMapping{
					{Semantic: "event_enabled", Path: "switches.eventState"},
					{Semantic: "legacy_enabled", Path: "switches.legacyOk"},
				},
				Conditions: []ConditionRule{
					{ID: "event_on", Op: "flag", Flag: "event_enabled"},
					{ID: "legacy_event_on", Op: "and", Refs: []string{"event_on", "legacy_ok"}},
					{ID: "legacy_ok", Op: "flag", Flag: "legacy_enabled"},
				},
				Conditionals: []ConditionalMapping{
					{Path: "modifiers.coinValue", Role: "coins", ConditionID: "legacy_event_on", Default: 7},
					{Path: "modifiers.expValue", Role: "experience", ConditionID: "event_on", Default: 3},
				},
				ConflictRules: defaultConflictRules(),
			},
		},
	}
}

func defaultConflictRules() []ConflictPolicy {
	return []ConflictPolicy{
		{Action: "preserve", Priority: 10},
		{Action: "default", Priority: 20},
		{Action: "reject", Priority: 30},
	}
}

// SortedConflictRules returns rules in declared priority order and never
// depends on the physical ordering in the source table.
func (b BranchSpec) SortedConflictRules() []ConflictPolicy {
	out := append([]ConflictPolicy(nil), b.ConflictRules...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].Action < out[j].Action
	})
	return out
}
