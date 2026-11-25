// Package formats implements the versioned distribution-format decoders and
// the multi-format merge loader.
package formats

import "replayaudit/internal/model"

// File names used inside a deterministic replay package directory.
const (
	File1      = "format_1.json"
	File2      = "format_2.json"
	File3      = "format_3.json"
	FileTruth  = "truth.json"
	FileParams = "params.json"
)

// Format1 is "RND-CAST": turn-serial time field `at`, and the `chain` list
// carries CASTER sequence. It encodes conditions and mutex groups but no
// forced sync points.
type Format1 struct {
	Format      int                `json:"format"`
	ReplayID    string             `json:"replay_id"`
	MsPerTurn   int64              `json:"ms_per_turn,omitempty"`
	Entities    []model.EntitySpec `json:"entities"`
	Flags       map[string]bool    `json:"flags,omitempty"`
	FlagDefs    []FlagDefDTO       `json:"flag_defs,omitempty"`
	MutexGroups []MutexDTO         `json:"mutex_groups,omitempty"`
	Events      []F1Event          `json:"events"`
}

// F1Event is one Format1 event record.
type F1Event struct {
	ID         string   `json:"id"`
	At         *int64   `json:"at"` // 1-based turn serial
	Kind       string   `json:"kind"`
	Chain      []string `json:"chain"` // CASTERS
	Targets    []string `json:"targets"`
	Amount     *int64   `json:"amount"`
	Skill      string   `json:"skill,omitempty"`
	CooldownMS *int64   `json:"cooldown_ms,omitempty"`
	CondFlag   string   `json:"cond_flag,omitempty"`
	CondWant   *bool    `json:"cond_want,omitempty"`
	Priority   *int     `json:"priority,omitempty"`
	Mandatory  bool     `json:"mandatory,omitempty"`
	Fault      bool     `json:"fault,omitempty"`
}

// Format2 is "MSQ-TARGET": millisecond timestamp field `at`, and the `chain`
// list carries TARGET sequence (semantic swap vs Format1). It encodes forced
// sync points but no conditions or mutex groups.
type Format2 struct {
	Format     int                `json:"format"`
	ReplayID   string             `json:"replay_id"`
	Entities   []model.EntitySpec `json:"entities"`
	SyncPoints []int64            `json:"sync_points,omitempty"` // ms
	Events     []F2Event          `json:"events"`
}

// F2Event is one Format2 event record.
type F2Event struct {
	ID         string   `json:"id"`
	At         *int64   `json:"at"` // ms timestamp
	Kind       string   `json:"kind"`
	Chain      []string `json:"chain"` // TARGETS
	Casters    []string `json:"casters"`
	Amount     *int64   `json:"amount"`
	Skill      string   `json:"skill,omitempty"`
	CooldownMS *int64   `json:"cooldown_ms,omitempty"`
	Priority   *int     `json:"priority,omitempty"`
	Mandatory  bool     `json:"mandatory,omitempty"`
	Fault      bool     `json:"fault,omitempty"`
}

// Format3 is "MSQ-FULL": explicit ms field `at_ms`, explicit caster/target
// sequences, conditions and sync points, but no mutex groups.
type Format3 struct {
	Format     int                `json:"format"`
	ReplayID   string             `json:"replay_id"`
	Entities   []model.EntitySpec `json:"entities"`
	Flags      map[string]bool    `json:"flags,omitempty"`
	FlagDefs   []FlagDefDTO       `json:"flag_defs,omitempty"`
	SyncPoints []int64            `json:"sync_points,omitempty"` // ms
	Events     []F3Event          `json:"events"`
}

// F3Event is one Format3 event record.
type F3Event struct {
	ID         string   `json:"id"`
	AtMS       *int64   `json:"at_ms"`
	Kind       string   `json:"kind"`
	CasterSeq  []string `json:"caster_seq"`
	TargetSeq  []string `json:"target_seq"`
	Amount     *int64   `json:"amount"`
	Skill      string   `json:"skill,omitempty"`
	CooldownMS *int64   `json:"cooldown_ms,omitempty"`
	CondFlag   string   `json:"cond_flag,omitempty"`
	CondWant   *bool    `json:"cond_want,omitempty"`
	Priority   *int     `json:"priority,omitempty"`
	Mandatory  bool     `json:"mandatory,omitempty"`
	Fault      bool     `json:"fault,omitempty"`
}

// FlagDefDTO is the on-wire priority flag declaration.
type FlagDefDTO struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Value    bool   `json:"value"`
	Priority int    `json:"priority"`
}

// MutexDTO declares one mutex group on the wire.
type MutexDTO struct {
	Group    string   `json:"group"`
	EventIDs []string `json:"event_ids"`
}

// PeekHeader only decodes the format discriminator.
type PeekHeader struct {
	Format *int `json:"format"`
}
