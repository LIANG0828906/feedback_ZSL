package hckernel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

type HealthState string

const (
	StateInitial   HealthState = "initial"
	StateHealthy   HealthState = "healthy"
	StateUnhealthy HealthState = "unhealthy"
	StateHalfOpen  HealthState = "half_open"
)

type ProbeResultKind string

const (
	ResultSuccess ProbeResultKind = "success"
	ResultFailure ProbeResultKind = "failure"
	ResultTimeout ProbeResultKind = "timeout"
)

type SlotPriority string

const (
	PriorityDeferredFirst SlotPriority = "deferred_first"
	PriorityPeriodicFirst SlotPriority = "periodic_first"
)

type ThresholdAction string

const (
	ThresholdReset ThresholdAction = "reset"
	ThresholdKeep  ThresholdAction = "keep"
)

// Instance is the schema-independent representation of one backend.
type Instance struct {
	ID               string      `json:"id"`
	NodeID           string      `json:"node_id"`
	RackID           string      `json:"rack_id"`
	Protocol         string      `json:"protocol"`
	Address          string      `json:"address,omitempty"`
	IntervalMs       int64       `json:"interval_ms"`
	TimeoutMs        int64       `json:"timeout_ms"`
	BackoffMs        int64       `json:"backoff_ms"`
	RetryLimit       int         `json:"retry_limit"`
	SuccessThreshold int         `json:"success_threshold"`
	FailureThreshold int         `json:"failure_threshold"`
	InitialState     HealthState `json:"initial_state"`
	WeightPPM        int64       `json:"weight_ppm"`
	ActiveProbe      bool        `json:"active_probe"`
}

type Rack struct {
	ID               string `json:"id"`
	AvailabilityZone string `json:"az,omitempty"`
	Concurrency      int    `json:"concurrency"`
}

type CountPolicy struct {
	SuccessReachedAction ThresholdAction `json:"success_reached_action"`
	FailureReachedAction ThresholdAction `json:"failure_reached_action"`
	ResetOppositeStreak  bool            `json:"reset_opposite_streak"`
	HalfOpenSuccessLimit int             `json:"half_open_success_limit"`
	HalfOpenFailureLimit int             `json:"half_open_failure_limit"`
	HalfOpenDelayMs      int64           `json:"half_open_delay_ms"`
}

// Project is the canonical engineering declaration shared by all schemas.
type Project struct {
	ProjectID         string       `json:"project_id"`
	ProjectVersion    string       `json:"project_version"`
	Seed              int64        `json:"seed"`
	StepMs            int64        `json:"step_ms"`
	Rounds            int          `json:"rounds"`
	MaxDeferMs        int64        `json:"max_defer_ms"`
	GlobalConcurrency int          `json:"global_concurrency"`
	Racks             []Rack       `json:"racks"`
	Workers           int          `json:"workers"`
	BufferCapacity    int          `json:"buffer_capacity"`
	SlotPriority      SlotPriority `json:"slot_priority"`
	CountPolicy       CountPolicy  `json:"count_policy"`
	Instances         []Instance   `json:"instances"`
	SourceSchema      string       `json:"-"`
}

func (p *Project) rackMap() map[string]Rack {
	out := make(map[string]Rack, len(p.Racks))
	for _, rack := range p.Racks {
		out[rack.ID] = rack
	}
	return out
}

func (p *Project) instanceMap() map[string]Instance {
	out := make(map[string]Instance, len(p.Instances))
	for _, inst := range p.Instances {
		out[inst.ID] = inst
	}
	return out
}

// Fingerprint hashes only canonical content. Source field names and source
// schema version are intentionally excluded.
func (p *Project) Fingerprint() (string, error) {
	clone := *p
	clone.Seed = 0
	data, err := json.Marshal(clone)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

type EventType string

const (
	EventProbeResult    EventType = "probe_result"
	EventInstanceAdd    EventType = "instance_added"
	EventInstanceRemove EventType = "instance_removed"
	EventRackMove       EventType = "rack_moved"
	EventConfigEdit     EventType = "config_edited"
)

// Event is a logical-time injection or topology/edit mutation.
type Event struct {
	ID         string          `json:"id"`
	AtMs       int64           `json:"at_ms"`
	Type       EventType       `json:"type"`
	InstanceID string          `json:"instance_id,omitempty"`
	ProbeID    string          `json:"probe_id,omitempty"`
	Result     ProbeResultKind `json:"result,omitempty"`
	LatencyMs  int64           `json:"latency_ms,omitempty"`
	ToRackID   string          `json:"to_rack_id,omitempty"`
	Instance   *Instance       `json:"instance,omitempty"`
	Edit       *Edit           `json:"edit,omitempty"`
}

type Edit struct {
	Field    string `json:"field"`
	NewValue any    `json:"new_value"`
}

type Probe struct {
	ID         string `json:"id"`
	Round      int    `json:"round"`
	Order      int    `json:"order"`
	InstanceID string `json:"instance_id"`
	RackID     string `json:"rack_id"`
	HalfOpen   bool   `json:"half_open"`
	WeightPPM  int64  `json:"weight_ppm"`
}

type Suppression struct {
	InstanceID   string    `json:"instance_id"`
	Code         ErrorCode `json:"code,omitempty"`
	Reason       string    `json:"reason"`
	ConstraintID string    `json:"constraint_id,omitempty"`
	QueueSeq     int64     `json:"queue_seq,omitempty"`
	Detail       string    `json:"detail,omitempty"`
}

type DeferredEntry struct {
	InstanceID    string `json:"instance_id"`
	QueueSeq      int64  `json:"queue_seq"`
	EnqueuedRound int    `json:"enqueued_round"`
	EnqueuedAtMs  int64  `json:"enqueued_at_ms"`
	LastRound     int    `json:"last_round"`
	RackID        string `json:"rack_id"`
	WeightPPM     int64  `json:"weight_ppm"`
	ConstraintID  string `json:"constraint_id"`
	Reason        string `json:"reason"`
}

type DroppedProbe struct {
	InstanceID string    `json:"instance_id"`
	QueueSeq   int64     `json:"queue_seq"`
	Round      int       `json:"round"`
	AtMs       int64     `json:"at_ms"`
	Code       ErrorCode `json:"code"`
	Reason     string    `json:"reason"`
}

type RescheduledProbe struct {
	InstanceID    string `json:"instance_id"`
	QueueSeq      int64  `json:"queue_seq"`
	Round         int    `json:"round"`
	OriginalRound int    `json:"original_round"`
}

type BudgetSnapshot struct {
	GlobalCapacity  int            `json:"global_capacity"`
	GlobalActive    int            `json:"global_active"`
	GlobalAvailable int            `json:"global_available"`
	RackCapacity    map[string]int `json:"rack_capacity"`
	RackActive      map[string]int `json:"rack_active"`
	RackAvailable   map[string]int `json:"rack_available"`
}

type Provenance struct {
	Kind               string   `json:"kind"`
	Source             string   `json:"source,omitempty"`
	InvalidationSource string   `json:"invalidation_source,omitempty"`
	Dependencies       []string `json:"dependencies,omitempty"`
	CacheHits          int      `json:"cache_hits,omitempty"`
	RecomputedRounds   []int    `json:"recomputed_rounds,omitempty"`
}

type RoundReport struct {
	Round       int                `json:"round"`
	NowMs       int64              `json:"now_ms"`
	Eligible    []string           `json:"eligible"`
	Dispatched  []Probe            `json:"dispatched"`
	Suppressed  []Suppression      `json:"suppressed"`
	Rejected    []Suppression      `json:"rejected,omitempty"`
	DeferredNow []DeferredEntry    `json:"deferred_now"`
	Queue       []DeferredEntry    `json:"queue"`
	Rescheduled []RescheduledProbe `json:"rescheduled"`
	Dropped     []DroppedProbe     `json:"dropped"`
	Budget      BudgetSnapshot     `json:"budget"`
	Topology    []string           `json:"topology_changes,omitempty"`
	Provenance  *Provenance        `json:"provenance,omitempty"`
}

type HealthObservation struct {
	ObservationID      string          `json:"observation_id"`
	EventID            string          `json:"event_id"`
	AtMs               int64           `json:"at_ms"`
	InstanceID         string          `json:"instance_id"`
	ProbeID            string          `json:"probe_id,omitempty"`
	Result             ProbeResultKind `json:"result,omitempty"`
	Trigger            string          `json:"trigger"`
	FromState          HealthState     `json:"from_state"`
	ToState            HealthState     `json:"to_state"`
	StateChanged       bool            `json:"state_changed"`
	SuccessStreak      int             `json:"success_streak"`
	FailureStreak      int             `json:"failure_streak"`
	SuccessThreshold   int             `json:"success_threshold"`
	FailureThreshold   int             `json:"failure_threshold"`
	Reason             string          `json:"reason,omitempty"`
	InvalidationSource string          `json:"invalidation_source,omitempty"`
}

type Result struct {
	ProjectID   string                 `json:"project_id"`
	Version     string                 `json:"version"`
	Fingerprint string                 `json:"fingerprint"`
	Rounds      []RoundReport          `json:"rounds"`
	HealthTrace []HealthObservation    `json:"health_trace"`
	FinalHealth map[string]HealthState `json:"final_health"`
	FinalCounts map[string][2]int      `json:"final_counts"`
}

// SortedInstanceIDs returns canonical declaration-independent order.
func SortedInstanceIDs(instances []Instance) []string {
	ids := make([]string, 0, len(instances))
	for _, inst := range instances {
		ids = append(ids, inst.ID)
	}
	sort.Strings(ids)
	return ids
}
