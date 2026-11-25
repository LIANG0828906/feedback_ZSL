package model

import (
	"sort"
)

const (
	CanonicalFormatVersion = "canonical-v1"
	AlgorithmVersion       = "queue-state-v1.4.0"
	TruthFingerprintVer    = "truth-fp-v1"
)

type SequenceItem struct {
	Round   int64  `json:"round"`
	UnitID  string `json:"unit_id"`
	SkillID string `json:"skill_id,omitempty"`
	Ref     string `json:"ref"`
}

type Unit struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	BaseWeight      int    `json:"base_weight"`
	TieOrdinal      int    `json:"tie_ordinal"`
	InitialCooldown int    `json:"initial_cooldown"`
	MaxResource     int    `json:"max_resource"`
	InitialResource int    `json:"initial_resource"`
	Regen           int    `json:"regen"`
}

type Skill struct {
	ID          string `json:"id"`
	UnitID      string `json:"unit_id"`
	Cost        int    `json:"cost"`
	Cooldown    int    `json:"cooldown"`
	Power       int    `json:"power"`
	Conditional bool   `json:"conditional"`
}

type PlannedAction struct {
	UnitID  string `json:"unit_id"`
	SkillID string `json:"skill_id"`
	Flag    *bool  `json:"flag,omitempty"`
}

type RoundSpec struct {
	Round       int64           `json:"round"`
	TimestampMs int64           `json:"timestamp_ms"`
	Actions     []PlannedAction `json:"actions"`
}

type ReorderInstruction struct {
	Offset          int    `json:"offset"`
	ID              string `json:"id"`
	UnitID          string `json:"unit_id"`
	AfterRound      int64  `json:"after_round"`
	NewTieOrdinal   int    `json:"new_tie_ordinal"`
	Reason          string `json:"reason"`
	ConsumedAtRound int64  `json:"consumed_at_round,omitempty"`
}

type WeightEdit struct {
	UnitID    string `json:"unit_id"`
	Weight    int    `json:"weight"`
	FromRound int64  `json:"from_round"`
	Reason    string `json:"reason"`
}

type UnitState struct {
	UnitID            string `json:"unit_id"`
	Resource          int    `json:"resource"`
	CooldownRemaining int    `json:"cooldown_remaining"`
	Alive             bool   `json:"alive"`
}

type QueueEntry struct {
	Position   int    `json:"position"`
	Round      int64  `json:"round"`
	UnitID     string `json:"unit_id"`
	SkillID    string `json:"skill_id"`
	Weight     int    `json:"weight"`
	TieOrdinal int    `json:"tie_ordinal"`
	Basis      string `json:"basis"`
}

type UnqueuedEntry struct {
	Round   int64  `json:"round"`
	UnitID  string `json:"unit_id"`
	SkillID string `json:"skill_id"`
	Reason  string `json:"reason"`
	Detail  string `json:"detail,omitempty"`
}

type ActionRecord struct {
	Position       int    `json:"position"`
	QueuePosition  int    `json:"queue_position"`
	Round          int64  `json:"round"`
	UnitID         string `json:"unit_id"`
	SkillID        string `json:"skill_id"`
	Decision       string `json:"decision"`
	Reason         string `json:"reason,omitempty"`
	Weight         int    `json:"weight"`
	TieOrdinal     int    `json:"tie_ordinal"`
	ResourceBefore int    `json:"resource_before"`
	ResourceAfter  int    `json:"resource_after"`
	CooldownBefore int    `json:"cooldown_before"`
	CooldownAfter  int    `json:"cooldown_after"`
	Power          int    `json:"power"`
	Damage         int    `json:"damage"`
}

type RoundResult struct {
	Round        int64           `json:"round"`
	TimestampMs  int64           `json:"timestamp_ms"`
	Queue        []QueueEntry    `json:"queue"`
	ActionOrder  []ActionRecord  `json:"action_order"`
	Settlements  []ActionRecord  `json:"settlements"`
	Unqueued     []UnqueuedEntry `json:"unqueued"`
	States       []UnitState     `json:"states"`
	TotalDamage  int             `json:"total_damage"`
	InputHash    string          `json:"input_hash"`
	ContentHash  string          `json:"content_hash"`
	Status       string          `json:"status"`
	ErrorCode    string          `json:"error_code,omitempty"`
	ErrorMessage string          `json:"error_message,omitempty"`
}

type RoundTruth struct {
	Round       int64           `json:"round"`
	TimestampMs int64           `json:"timestamp_ms"`
	Order       []QueueEntry    `json:"order"`
	Casts       []ActionRecord  `json:"casts"`
	Unqueued    []UnqueuedEntry `json:"unqueued"`
	States      []UnitState     `json:"states"`
	TotalDamage int             `json:"total_damage"`
}

type Truth struct {
	ProjectID   string       `json:"project_id"`
	Seed        int64        `json:"seed"`
	Rounds      []RoundTruth `json:"rounds"`
	FinalWinner string       `json:"final_winner"`
	EnemyHP     int          `json:"enemy_hp"`
	Fingerprint string       `json:"fingerprint,omitempty"`
	RoundHashes []string     `json:"round_hashes,omitempty"`
}

type Project struct {
	ID            string               `json:"id"`
	Seed          int64                `json:"seed"`
	FormatNo      int                  `json:"format_no"`
	SourceFormat  string               `json:"source_format"`
	SchemaVersion string               `json:"schema_version"`
	TickMs        int64                `json:"tick_ms"`
	EpochMs       int64                `json:"epoch_ms"`
	StartRound    int64                `json:"start_round"`
	TotalRounds   int                  `json:"total_rounds"`
	EnemyHP       int                  `json:"enemy_hp"`
	Units         []Unit               `json:"units"`
	Skills        []Skill              `json:"skills"`
	Rounds        []RoundSpec          `json:"rounds"`
	SkillSequence []SequenceItem       `json:"skill_sequence"`
	ActorSequence []SequenceItem       `json:"actor_sequence"`
	Reorders      []ReorderInstruction `json:"reorders"`
	WeightEdits   []WeightEdit         `json:"weight_edits,omitempty"`
	InitialStates []UnitState          `json:"initial_states"`
	Truth         *Truth               `json:"truth,omitempty"`
	Dependencies  []Dependency         `json:"dependencies,omitempty"`
}

type Dependency struct {
	Key          string   `json:"key"`
	Kind         string   `json:"kind"`
	Source       string   `json:"source"`
	AffectedKeys []string `json:"affected_keys"`
	Description  string   `json:"description"`
}

type Invalidation struct {
	Round  int64  `json:"round"`
	UnitID string `json:"unit_id"`
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

type WindowRecord struct {
	WindowIndex      int    `json:"window_index"`
	StartRound       int64  `json:"start_round"`
	EndRound         int64  `json:"end_round"`
	ContentHash      string `json:"content_hash"`
	FormatVersion    string `json:"format_version"`
	AlgorithmVersion string `json:"algorithm_version"`
	ReorderOffsets   []int  `json:"reorder_offsets"`
	RoundCount       int    `json:"round_count"`
}

func (p *Project) Unit(id string) (Unit, bool) {
	for _, unit := range p.Units {
		if unit.ID == id {
			return unit, true
		}
	}
	return Unit{}, false
}

func (p *Project) Skill(id string) (Skill, bool) {
	for _, skill := range p.Skills {
		if skill.ID == id {
			return skill, true
		}
	}
	return Skill{}, false
}

func (p *Project) SkillForUnit(unitID string) (Skill, bool) {
	for _, skill := range p.Skills {
		if skill.UnitID == unitID {
			return skill, true
		}
	}
	return Skill{}, false
}

func (p *Project) SortedUnits() []Unit {
	units := append([]Unit(nil), p.Units...)
	sort.Slice(units, func(i, j int) bool { return units[i].ID < units[j].ID })
	return units
}

func SortedStates(states []UnitState) []UnitState {
	out := append([]UnitState(nil), states...)
	sort.Slice(out, func(i, j int) bool { return out[i].UnitID < out[j].UnitID })
	return out
}

func CloneStates(states []UnitState) []UnitState {
	out := make([]UnitState, len(states))
	copy(out, states)
	return out
}

func StateMap(states []UnitState) map[string]UnitState {
	out := make(map[string]UnitState, len(states))
	for _, state := range states {
		out[state.UnitID] = state
	}
	return out
}
