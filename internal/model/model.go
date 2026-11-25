// Package model contains the normalized replay timeline, settlement state,
// parameter/version declarations and deterministic hashing helpers.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

const (
	// FormatVersion is bumped when the normalized envelope/content changes.
	FormatVersion = "replay-format-1.0"
	// AlgoVersion is bumped when settlement/adjudication rules change.
	AlgoVersion = "settle-algo-1.0"
)

// Params are the declared, deterministic run parameters.
type Params struct {
	ParamsVersion string `json:"params_version"`
	// MsPerTurn is the integer conversion basis between turn numbers and ms.
	MsPerTurn int64 `json:"ms_per_turn"`
	// Anchor declares turn -> representative ms mapping: "start" only for v1.
	Anchor string `json:"anchor"`
	// Round declares the integer ms -> turn rounding direction: floor|ceil|nearest.
	Round string `json:"round"`
	// SegmentUnits is the fixed checkpoint segment width in internal ms.
	SegmentUnits int64 `json:"segment_units"`
	// Tolerance is the declared comparison tolerance in internal units.
	Tolerance int64 `json:"tolerance"`
	// MaxParallel bounds worker pool parallelism.
	MaxParallel int `json:"max_parallel"`
	// WorkerBuffer bounds each worker thread's pending event buffer.
	WorkerBuffer int `json:"worker_buffer"`
	// MaxRetries is the declared retry budget for failed events at barriers.
	MaxRetries int `json:"max_retries"`
}

// DefaultParams returns the declared v1 parameter set.
func DefaultParams() Params {
	return Params{
		ParamsVersion: "params-v1",
		MsPerTurn:     3000,
		Anchor:        "start",
		Round:         "floor",
		SegmentUnits:  6000,
		Tolerance:     0,
		MaxParallel:   2,
		WorkerBuffer:  4,
		MaxRetries:    1,
	}
}

// Hash returns a stable content hash of the parameters.
func (p Params) Hash() string { return HashOf(p) }

// EntitySpec declares one combat/economy entity.
type EntitySpec struct {
	ID        string `json:"id"`
	Side      string `json:"side"`
	MaxHP     int64  `json:"max_hp"`
	InitialHP int64  `json:"initial_hp,omitempty"`
	MaxMP     int64  `json:"max_mp"`
	InitialMP int64  `json:"initial_mp,omitempty"`
}

// Cond is a conditional event predicate: event participates iff flag == Want.
type Cond struct {
	Flag string `json:"flag"`
	Want bool   `json:"want"`
}

// Event is the normalized timeline event. All times are internal ms units.
type Event struct {
	ID            string         `json:"id"`
	Kind          string         `json:"kind"`
	At            int64          `json:"at"`
	Casters       []string       `json:"casters,omitempty"`
	Targets       []string       `json:"targets,omitempty"`
	Amount        int64          `json:"amount"`
	Skill         string         `json:"skill,omitempty"`
	CooldownUnits int64          `json:"cooldown_units,omitempty"`
	Cond          *Cond          `json:"cond,omitempty"`
	Priority      int            `json:"priority"`
	Mandatory     bool           `json:"mandatory,omitempty"`
	Fault         bool           `json:"fault,omitempty"`
	SourceFormats []int          `json:"source_formats,omitempty"`
	FieldSource   map[string]int `json:"field_source,omitempty"`
}

// Clone returns a deep copy of the event.
func (e *Event) Clone() *Event {
	if e == nil {
		return nil
	}
	cp := *e
	cp.Casters = append([]string(nil), e.Casters...)
	cp.Targets = append([]string(nil), e.Targets...)
	if e.Cond != nil {
		c := *e.Cond
		cp.Cond = &c
	}
	cp.SourceFormats = append([]int(nil), e.SourceFormats...)
	if e.FieldSource != nil {
		cp.FieldSource = make(map[string]int, len(e.FieldSource))
		for k, v := range e.FieldSource {
			cp.FieldSource[k] = v
		}
	}
	return &cp
}

// FlagDef is one (possibly priority-adjudicated) initial flag declaration.
type FlagDef struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Value    bool   `json:"value"`
	Priority int    `json:"priority"`
}

// MutexGroup declares mutually exclusive events.
type MutexGroup struct {
	ID       string   `json:"id"`
	EventIDs []string `json:"event_ids"`
}

// SyncPoint is a forced synchronization barrier at internal time At.
type SyncPoint struct {
	ID string `json:"id"`
	At int64  `json:"at"`
}

// Replay is the fully normalized multi-format replay.
type Replay struct {
	ReplayID string       `json:"replay_id"`
	Entities []EntitySpec `json:"entities"`
	Flags    []FlagDef    `json:"flags,omitempty"`
	Mutex    []MutexGroup `json:"mutex,omitempty"`
	Sync     []SyncPoint  `json:"sync,omitempty"`
	Events   []*Event     `json:"events"`
}

// Clone returns a deep copy of the replay.
func (r *Replay) Clone() *Replay {
	cp := &Replay{ReplayID: r.ReplayID}
	cp.Entities = append([]EntitySpec(nil), r.Entities...)
	cp.Flags = append([]FlagDef(nil), r.Flags...)
	cp.Mutex = append([]MutexGroup(nil), r.Mutex...)
	cp.Sync = append([]SyncPoint(nil), r.Sync...)
	cp.Events = make([]*Event, len(r.Events))
	for i, e := range r.Events {
		cp.Events[i] = e.Clone()
	}
	return cp
}

// EventIndex returns events keyed by stable identifier.
func (r *Replay) EventIndex() map[string]*Event {
	m := make(map[string]*Event, len(r.Events))
	for _, e := range r.Events {
		m[e.ID] = e
	}
	return m
}

// State is the cumulative settlement state.
type State struct {
	HP       map[string]int64 `json:"hp"`
	MP       map[string]int64 `json:"mp"`
	Cooldown map[string]int64 `json:"cooldown"`
	Winner   string           `json:"winner,omitempty"`
	Ended    bool             `json:"ended"`
}

// NewState builds the declared initial state from entity specs.
func NewState(entities []EntitySpec) State {
	s := State{
		HP:       make(map[string]int64, len(entities)),
		MP:       make(map[string]int64, len(entities)),
		Cooldown: make(map[string]int64),
	}
	for _, en := range entities {
		hp := en.MaxHP
		if en.InitialHP > 0 {
			hp = en.InitialHP
		}
		mp := int64(0)
		if en.InitialMP > 0 {
			mp = en.InitialMP
		}
		s.HP[en.ID] = hp
		s.MP[en.ID] = mp
	}
	return s
}

// Clone returns a deep copy of the state.
func (s State) Clone() State {
	cp := State{Winner: s.Winner, Ended: s.Ended}
	cp.HP = make(map[string]int64, len(s.HP))
	for k, v := range s.HP {
		cp.HP[k] = v
	}
	cp.MP = make(map[string]int64, len(s.MP))
	for k, v := range s.MP {
		cp.MP[k] = v
	}
	cp.Cooldown = make(map[string]int64, len(s.Cooldown))
	for k, v := range s.Cooldown {
		cp.Cooldown[k] = v
	}
	return cp
}

// Hash stable-hashes the state. Map keys are sorted by encoding/json.
func (s State) Hash() string { return HashOf(s) }

// Effect is one atomic state delta produced by an event outcome.
type Effect struct {
	Key    string `json:"key"`
	Op     string `json:"op"` // add|set
	Amount int64  `json:"amount"`
}

// Outcome is the cacheable, deterministic result of evaluating one event's
// rules before the ordered state fold.
type Outcome struct {
	Status  string   `json:"status"` // settled|skipped_cond|suppressed|failed
	Reason  string   `json:"reason,omitempty"`
	Effects []Effect `json:"effects,omitempty"`
}

// EventResult is a fully settled event on the canonical timeline.
type EventResult struct {
	EventID     string   `json:"event_id"`
	EffectiveAt int64    `json:"effective_at"`
	OrderIndex  int      `json:"order_index"`
	Segment     int      `json:"segment"`
	Status      string   `json:"status"`
	Reason      string   `json:"reason,omitempty"`
	Effects     []Effect `json:"effects,omitempty"`
	Attempt     int      `json:"attempt,omitempty"`
	StateAfter  State    `json:"state_after"`
	ResultHash  string   `json:"result_hash"`
}

// ConflictRecord is one explainable adjudication detail.
type ConflictRecord struct {
	Type           string   `json:"type"` // mutex|flag_contradiction|sync_pending
	PrimaryEventID string   `json:"primary_event_id"`
	RelatedIDs     []string `json:"related_ids,omitempty"`
	Decision       string   `json:"decision"`
	Detail         string   `json:"detail"`
}

// SegmentCommit is the deterministic commit descriptor of one time segment.
type SegmentCommit struct {
	Segment     int      `json:"segment"`
	CommitSeq   int      `json:"commit_seq"`
	EventIDs    []string `json:"event_ids"`
	SyncIDs     []string `json:"sync_ids,omitempty"`
	StateHash   string   `json:"state_hash"`
	SegmentHash string   `json:"segment_hash"`
}

// Report is the complete deterministic settlement output.
type Report struct {
	ReplayID       string           `json:"replay_id"`
	ContentHash    string           `json:"content_hash"`
	ParamsHash     string           `json:"params_hash"`
	FormatVersion  string           `json:"format_version"`
	AlgoVersion    string           `json:"algo_version"`
	EventResults   []*EventResult   `json:"event_results"`
	SyncPoints     []SyncPoint      `json:"sync_points"`
	Conflicts      []ConflictRecord `json:"conflicts,omitempty"`
	Terminal       State            `json:"terminal"`
	Winner         string           `json:"winner"`
	CommitSequence []SegmentCommit  `json:"commit_sequence"`
}

// ResultByID indexes event results.
func (r *Report) ResultByID() map[string]*EventResult {
	m := make(map[string]*EventResult, len(r.EventResults))
	for _, er := range r.EventResults {
		m[er.EventID] = er
	}
	return m
}

// Truth is the parser-constructed reference truth shipped by the generator.
type Truth struct {
	ReplayID    string            `json:"replay_id"`
	EffectiveAt map[string]int64  `json:"effective_at"`
	Status      map[string]string `json:"status,omitempty"`
	Terminal    State             `json:"terminal"`
	SyncPoints  []int64           `json:"sync_points"`
	Winner      string            `json:"winner"`
}

// HashOf returns the SHA-256 hex of canonical JSON of v.
func HashOf(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		// All hashed values are JSON-safe model types.
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// HashReplay returns the content hash covering all semantic replay fields.
// Provenance-only fields are excluded via an explicit canonical projection.
func HashReplay(r *Replay) string {
	type event struct {
		ID            string   `json:"id"`
		Kind          string   `json:"kind"`
		At            int64    `json:"at"`
		Casters       []string `json:"casters"`
		Targets       []string `json:"targets"`
		Amount        int64    `json:"amount"`
		Skill         string   `json:"skill"`
		CooldownUnits int64    `json:"cooldown_units"`
		Cond          *Cond    `json:"cond"`
		Priority      int      `json:"priority"`
		Mandatory     bool     `json:"mandatory"`
		Fault         bool     `json:"fault"`
	}
	events := make([]event, 0, len(r.Events))
	for _, e := range r.Events {
		events = append(events, event{
			ID: e.ID, Kind: e.Kind, At: e.At,
			Casters: append([]string(nil), e.Casters...),
			Targets: append([]string(nil), e.Targets...),
			Amount:  e.Amount, Skill: e.Skill, CooldownUnits: e.CooldownUnits,
			Cond: e.Cond, Priority: e.Priority, Mandatory: e.Mandatory, Fault: e.Fault,
		})
	}
	sort.Slice(events, func(i, j int) bool { return events[i].ID < events[j].ID })
	canonical := struct {
		ReplayID string       `json:"replay_id"`
		Entities []EntitySpec `json:"entities"`
		Flags    []FlagDef    `json:"flags"`
		Mutex    []MutexGroup `json:"mutex"`
		Sync     []SyncPoint  `json:"sync"`
		Events   []event      `json:"events"`
	}{r.ReplayID, r.Entities, r.Flags, r.Mutex, r.Sync, events}
	return HashOf(canonical)
}

// SortedSyncTimes returns the deterministic sync point time set.
func SortedSyncTimes(points []SyncPoint) []int64 {
	out := make([]int64, 0, len(points))
	for _, p := range points {
		out = append(out, p.At)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
