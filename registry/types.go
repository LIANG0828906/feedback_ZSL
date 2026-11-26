package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

const (
	EngineVersion = "serviceregistry/1"

	MaxDurationMS = int64(7 * 24 * time.Hour / time.Millisecond)
	MaxTickMS     = int64(time.Hour / time.Millisecond)
)

type SchemaVersion string

const (
	SchemaV1 SchemaVersion = "v1"
	SchemaV2 SchemaVersion = "v2"
	SchemaV3 SchemaVersion = "v3"
)

func (v SchemaVersion) Valid() bool {
	switch v {
	case SchemaV1, SchemaV2, SchemaV3:
		return true
	default:
		return false
	}
}

type WeightScale string

const (
	ScalePerMille WeightScale = "per_mille"
	ScalePer10K   WeightScale = "per_10k"
)

func (s WeightScale) Denominator() int64 {
	switch s {
	case ScalePerMille:
		return 1_000
	case ScalePer10K:
		return 10_000
	default:
		return 0
	}
}

type HealthState string

const (
	HealthInitial   HealthState = "initial"
	HealthHealthy   HealthState = "healthy"
	HealthUnhealthy HealthState = "unhealthy"
	HealthHalfOpen  HealthState = "half_open"
)

func (s HealthState) Valid() bool {
	switch s {
	case HealthInitial, HealthHealthy, HealthUnhealthy, HealthHalfOpen:
		return true
	default:
		return false
	}
}

type RegistrationPhase string

const (
	PhaseUnregistered RegistrationPhase = "unregistered"
	PhaseRegistered   RegistrationPhase = "registered"
	PhaseRemoval      RegistrationPhase = "removal_pending"
)

type HalfOpenPolicy string

const (
	HalfOpenExclude HalfOpenPolicy = "exclude"
	HalfOpenDemote  HalfOpenPolicy = "demote"
)

type EventKind string

const (
	EventRegister    EventKind = "register"
	EventDeregister  EventKind = "deregister"
	EventHeartbeat   EventKind = "heartbeat"
	EventProbeResult EventKind = "probe_result"
	EventQuery       EventKind = "query"
)

func (k EventKind) Valid() bool {
	switch k {
	case EventRegister, EventDeregister, EventHeartbeat, EventProbeResult, EventQuery:
		return true
	default:
		return false
	}
}

// Budgets are logical concurrency limits. Workers may execute in any order;
// the scheduler itself is deterministic and applies these limits by probe.
type Budgets struct {
	GlobalConcurrency int   `json:"global_concurrency"`
	NodeConcurrency   int   `json:"node_concurrency"`
	RackConcurrency   int   `json:"rack_concurrency"`
	InstanceBuffer    int   `json:"instance_buffer"`
	MaxDelayMS        int64 `json:"max_delay_ms"`
}

type Service struct {
	ID                     string         `json:"id"`
	Name                   string         `json:"name"`
	DefaultWeightScale     WeightScale    `json:"default_weight_scale"`
	HalfOpenPolicy         HalfOpenPolicy `json:"half_open_policy"`
	HalfOpenFactorPermille int64          `json:"half_open_factor_permille"`
	AdmitInitial           bool           `json:"admit_initial"`
	Order                  int            `json:"-"`
}

type Instance struct {
	ID               string      `json:"id"`
	ServiceID        string      `json:"service_id"`
	NodeID           string      `json:"node_id"`
	RackID           string      `json:"rack_id"`
	Address          string      `json:"address"`
	Weight           int64       `json:"weight"`
	WeightScale      WeightScale `json:"weight_scale"`
	InitialHealth    HealthState `json:"initial_health"`
	LeaseTTLMS       int64       `json:"lease_ttl_ms"`
	HeartbeatMS      int64       `json:"heartbeat_ms"`
	ProbePeriodMS    int64       `json:"probe_period_ms"`
	ProbeTimeoutMS   int64       `json:"probe_timeout_ms"`
	SuccessThreshold int         `json:"success_threshold"`
	FailureThreshold int         `json:"failure_threshold"`
	LeaseWatermarkMS *int64      `json:"lease_watermark_ms,omitempty"`
	Order            int         `json:"-"`
	SourcePath       string      `json:"-"`
}

func (i Instance) NormalizedWeightPPM() (int64, error) {
	den := i.WeightScale.Denominator()
	if den == 0 {
		return 0, NewError(CodeInvalidWeightScale, "unknown weight scale", ErrorContext{InstanceID: i.ID, ServiceID: i.ServiceID, Path: i.SourcePath})
	}
	if i.Weight < 0 || i.Weight > den {
		return 0, NewError(CodeInvalidWeight, "weight is outside scale domain", ErrorContext{InstanceID: i.ID, ServiceID: i.ServiceID, Path: i.SourcePath})
	}
	return i.Weight * 1_000_000 / den, nil
}

type Event struct {
	AtMS        int64       `json:"at_ms"`
	Kind        EventKind   `json:"kind"`
	ServiceID   string      `json:"service_id,omitempty"`
	InstanceID  string      `json:"instance_id,omitempty"`
	ProbeID     string      `json:"probe_id,omitempty"`
	Success     *bool       `json:"success,omitempty"`
	WeightScale WeightScale `json:"weight_scale,omitempty"`
	QueryID     string      `json:"query_id,omitempty"`
	Limit       int         `json:"limit,omitempty"`
	SourcePath  string      `json:"-"`
}

type ConfigOverride struct {
	InstanceID       string       `json:"instance_id"`
	FromTick         int64        `json:"from_tick"`
	LeaseTTLMS       *int64       `json:"lease_ttl_ms,omitempty"`
	HeartbeatMS      *int64       `json:"heartbeat_ms,omitempty"`
	ProbePeriodMS    *int64       `json:"probe_period_ms,omitempty"`
	ProbeTimeoutMS   *int64       `json:"probe_timeout_ms,omitempty"`
	SuccessThreshold *int         `json:"success_threshold,omitempty"`
	FailureThreshold *int         `json:"failure_threshold,omitempty"`
	Weight           *int64       `json:"weight,omitempty"`
	WeightScale      *WeightScale `json:"weight_scale,omitempty"`
}

type Project struct {
	SchemaVersion  SchemaVersion    `json:"schema_version"`
	ProjectVersion string           `json:"project_version"`
	Seed           int64            `json:"seed"`
	TickMS         int64            `json:"tick_ms"`
	Budgets        Budgets          `json:"budgets"`
	Services       []Service        `json:"services"`
	Instances      []Instance       `json:"instances"`
	Events         []Event          `json:"events"`
	Overrides      []ConfigOverride `json:"overrides,omitempty"`
}

func (p *Project) ServiceByID(id string) (Service, bool) {
	for _, service := range p.Services {
		if service.ID == id {
			return service, true
		}
	}
	return Service{}, false
}

func (p *Project) InstanceByID(id string) (Instance, bool) {
	for _, instance := range p.Instances {
		if instance.ID == id {
			return instance, true
		}
	}
	return Instance{}, false
}

func EffectiveInstance(project *Project, instance Instance, tick int64) Instance {
	if project == nil {
		return instance
	}
	for _, override := range project.Overrides {
		if override.InstanceID != instance.ID || override.FromTick > tick {
			continue
		}
		if override.LeaseTTLMS != nil {
			instance.LeaseTTLMS = *override.LeaseTTLMS
		}
		if override.HeartbeatMS != nil {
			instance.HeartbeatMS = *override.HeartbeatMS
		}
		if override.ProbePeriodMS != nil {
			instance.ProbePeriodMS = *override.ProbePeriodMS
		}
		if override.ProbeTimeoutMS != nil {
			instance.ProbeTimeoutMS = *override.ProbeTimeoutMS
		}
		if override.SuccessThreshold != nil {
			instance.SuccessThreshold = *override.SuccessThreshold
		}
		if override.FailureThreshold != nil {
			instance.FailureThreshold = *override.FailureThreshold
		}
		if override.WeightScale != nil {
			instance.WeightScale = *override.WeightScale
		}
		if override.Weight != nil {
			instance.Weight = *override.Weight
		}
	}
	return instance
}

func (p *Project) MaxEventTick() int64 {
	var max int64
	for _, event := range p.Events {
		tick := event.AtMS / p.TickMS
		if tick > max {
			max = tick
		}
	}
	return max
}

// CanonicalJSON deliberately excludes schema-specific spelling. It is used
// to prove that all three schemas loaded to the same domain project.
func (p *Project) CanonicalJSON() ([]byte, error) {
	type canonical struct {
		ProjectVersion string           `json:"project_version"`
		Seed           int64            `json:"seed"`
		TickMS         int64            `json:"tick_ms"`
		Budgets        Budgets          `json:"budgets"`
		Services       []Service        `json:"services"`
		Instances      []Instance       `json:"instances"`
		Events         []Event          `json:"events"`
		Overrides      []ConfigOverride `json:"overrides,omitempty"`
	}
	c := canonical{
		ProjectVersion: p.ProjectVersion,
		Seed:           p.Seed,
		TickMS:         p.TickMS,
		Budgets:        p.Budgets,
		Services:       p.Services,
		Instances:      p.Instances,
		Events:         p.Events,
		Overrides:      p.Overrides,
	}
	return json.Marshal(c)
}

func (p *Project) ModelHash() (string, error) {
	data, err := p.CanonicalJSON()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (p *Project) Clone() *Project {
	if p == nil {
		return nil
	}
	cloned := *p
	cloned.Services = append([]Service(nil), p.Services...)
	cloned.Instances = make([]Instance, len(p.Instances))
	for index, instance := range p.Instances {
		cloned.Instances[index] = instance
		if instance.LeaseWatermarkMS != nil {
			value := *instance.LeaseWatermarkMS
			cloned.Instances[index].LeaseWatermarkMS = &value
		}
	}
	cloned.Events = make([]Event, len(p.Events))
	for index, event := range p.Events {
		cloned.Events[index] = event
		if event.Success != nil {
			value := *event.Success
			cloned.Events[index].Success = &value
		}
	}
	cloned.Overrides = make([]ConfigOverride, len(p.Overrides))
	for index, override := range p.Overrides {
		cloned.Overrides[index] = override
		copyInt64 := func(value *int64) *int64 {
			if value == nil {
				return nil
			}
			copied := *value
			return &copied
		}
		copyInt := func(value *int) *int {
			if value == nil {
				return nil
			}
			copied := *value
			return &copied
		}
		cloned.Overrides[index].LeaseTTLMS = copyInt64(override.LeaseTTLMS)
		cloned.Overrides[index].HeartbeatMS = copyInt64(override.HeartbeatMS)
		cloned.Overrides[index].ProbePeriodMS = copyInt64(override.ProbePeriodMS)
		cloned.Overrides[index].ProbeTimeoutMS = copyInt64(override.ProbeTimeoutMS)
		cloned.Overrides[index].SuccessThreshold = copyInt(override.SuccessThreshold)
		cloned.Overrides[index].FailureThreshold = copyInt(override.FailureThreshold)
		cloned.Overrides[index].Weight = copyInt64(override.Weight)
		if override.WeightScale != nil {
			copied := *override.WeightScale
			cloned.Overrides[index].WeightScale = &copied
		}
	}
	return &cloned
}

type InstanceState struct {
	InstanceID        string            `json:"instance_id"`
	ServiceID         string            `json:"service_id"`
	Phase             RegistrationPhase `json:"phase"`
	Health            HealthState       `json:"health"`
	LeaseExpiresAtMS  int64             `json:"lease_expires_at_ms"`
	LeaseRemainingMS  int64             `json:"lease_remaining_ms"`
	RenewalCount      int64             `json:"renewal_count"`
	ConsecutiveOK     int               `json:"consecutive_success"`
	ConsecutiveFail   int               `json:"consecutive_failure"`
	NextProbeDueMS    int64             `json:"next_probe_due_ms"`
	InFlightProbeID   string            `json:"in_flight_probe_id,omitempty"`
	RemovalReason     string            `json:"removal_reason,omitempty"`
	LastTransitionAt  int64             `json:"last_transition_at_ms"`
	LastTransitionWhy string            `json:"last_transition_cause,omitempty"`
}

type ProbeRef struct {
	ProbeID      string   `json:"probe_id"`
	InstanceID   string   `json:"instance_id"`
	ServiceID    string   `json:"service_id"`
	DueAtMS      int64    `json:"due_at_ms"`
	DispatchedMS int64    `json:"dispatched_at_ms,omitempty"`
	DeadlineMS   int64    `json:"deadline_ms,omitempty"`
	QueueSeq     int64    `json:"queue_seq,omitempty"`
	Violations   []string `json:"violations,omitempty"`
}

type SuppressedProbe struct {
	ProbeID    string `json:"probe_id"`
	InstanceID string `json:"instance_id"`
	AtMS       int64  `json:"at_ms"`
	Reason     string `json:"reason"`
}

type DeferredProbe struct {
	ProbeID    string   `json:"probe_id"`
	InstanceID string   `json:"instance_id"`
	QueuedAtMS int64    `json:"queued_at_ms"`
	QueueSeq   int64    `json:"queue_seq"`
	Violations []string `json:"violations"`
}

type DroppedProbe struct {
	ProbeID    string   `json:"probe_id"`
	InstanceID string   `json:"instance_id"`
	DroppedMS  int64    `json:"dropped_at_ms"`
	QueuedAtMS int64    `json:"queued_at_ms"`
	QueueSeq   int64    `json:"queue_seq,omitempty"`
	Reason     string   `json:"reason"`
	Violations []string `json:"violations,omitempty"`
}

type ProbeCompletion struct {
	ProbeID    string `json:"probe_id"`
	InstanceID string `json:"instance_id"`
	AtMS       int64  `json:"at_ms"`
	Success    bool   `json:"success"`
	Reason     string `json:"reason"`
	TimedOut   bool   `json:"timed_out,omitempty"`
}

type HealthTransition struct {
	InstanceID        string      `json:"instance_id"`
	From              HealthState `json:"from"`
	To                HealthState `json:"to"`
	AtMS              int64       `json:"at_ms"`
	Cause             string      `json:"cause"`
	ConsecutiveOK     int         `json:"consecutive_success"`
	ConsecutiveFailed int         `json:"consecutive_failure"`
}

type EventOutcome struct {
	Index      int       `json:"index"`
	Kind       EventKind `json:"kind"`
	AtMS       int64     `json:"at_ms"`
	ServiceID  string    `json:"service_id,omitempty"`
	InstanceID string    `json:"instance_id,omitempty"`
	Accepted   bool      `json:"accepted"`
	Code       string    `json:"code,omitempty"`
}

type Candidate struct {
	InstanceID         string      `json:"instance_id"`
	Address            string      `json:"address"`
	Health             HealthState `json:"health"`
	WeightPPM          int64       `json:"weight_ppm"`
	EffectiveWeightPPM int64       `json:"effective_weight_ppm"`
	DisplayWeight      int64       `json:"display_weight"`
	DisplayScale       WeightScale `json:"display_scale"`
	Tier               int         `json:"tier"`
	Basis              []string    `json:"basis"`
}

type ExcludedInstance struct {
	InstanceID string      `json:"instance_id"`
	Health     HealthState `json:"health"`
	Reason     string      `json:"reason"`
}

type Catalog struct {
	ServiceID  string             `json:"service_id"`
	AtMS       int64              `json:"at_ms"`
	Candidates []Candidate        `json:"candidates"`
	Excluded   []ExcludedInstance `json:"excluded"`
}

type QueryResult struct {
	QueryID   string  `json:"query_id"`
	ServiceID string  `json:"service_id"`
	AtMS      int64   `json:"at_ms"`
	Catalog   Catalog `json:"catalog"`
}

type Invalidation struct {
	Source     string   `json:"source"`
	Reason     string   `json:"reason"`
	Closure    []string `json:"closure"`
	FromSeq    int      `json:"from_seq"`
	FromAtMS   int64    `json:"from_at_ms"`
	Reused     int      `json:"reused_steps"`
	Extended   int      `json:"extended_steps,omitempty"`
	Recomputed int      `json:"recomputed_steps"`
}

type StepResult struct {
	Seq              int                `json:"seq"`
	Tick             int64              `json:"tick"`
	AtMS             int64              `json:"at_ms"`
	Events           []EventOutcome     `json:"events,omitempty"`
	Instances        []InstanceState    `json:"instances"`
	DueProbes        []ProbeRef         `json:"due_probes,omitempty"`
	SuppressedProbes []SuppressedProbe  `json:"suppressed_probes,omitempty"`
	DeferredProbes   []DeferredProbe    `json:"deferred_probes,omitempty"`
	DroppedProbes    []DroppedProbe     `json:"dropped_probes,omitempty"`
	CompletedProbes  []ProbeCompletion  `json:"completed_probes,omitempty"`
	TimedOutProbes   []ProbeCompletion  `json:"timed_out_probes,omitempty"`
	Transitions      []HealthTransition `json:"transitions,omitempty"`
	Catalogs         []Catalog          `json:"catalogs"`
	Queries          []QueryResult      `json:"queries,omitempty"`
	Invalidation     *Invalidation      `json:"invalidation,omitempty"`
}

type RunResult struct {
	ProjectVersion string        `json:"project_version"`
	ModelHash      string        `json:"model_hash"`
	SchemaVersion  SchemaVersion `json:"schema_version"`
	Steps          []StepResult  `json:"steps"`
}

type ProbeTaskSnapshot struct {
	ProbeID      string   `json:"probe_id"`
	InstanceID   string   `json:"instance_id"`
	ServiceID    string   `json:"service_id"`
	NodeID       string   `json:"node_id"`
	RackID       string   `json:"rack_id"`
	DueAtMS      int64    `json:"due_at_ms"`
	DispatchedMS int64    `json:"dispatched_at_ms,omitempty"`
	DeadlineMS   int64    `json:"deadline_ms,omitempty"`
	QueueSeq     int64    `json:"queue_seq,omitempty"`
	Violations   []string `json:"violations,omitempty"`
	State        string   `json:"state"`
}

type RuntimeSnapshot struct {
	InstanceState
	InitialHealth HealthState `json:"initial_health"`
}

type Snapshot struct {
	EngineVersion  string              `json:"engine_version"`
	ProjectVersion string              `json:"project_version"`
	ModelHash      string              `json:"model_hash"`
	SchemaVersion  SchemaVersion       `json:"schema_version"`
	NextTick       int64               `json:"next_tick"`
	CommitSeq      int                 `json:"commit_seq"`
	LastAtMS       int64               `json:"last_at_ms"`
	NextQueueSeq   int64               `json:"next_queue_seq"`
	Instances      []RuntimeSnapshot   `json:"instances"`
	Queue          []ProbeTaskSnapshot `json:"queue"`
	InFlight       []ProbeTaskSnapshot `json:"in_flight"`
}

type TruthInstance struct {
	InstanceID       string            `json:"instance_id"`
	ServiceID        string            `json:"service_id"`
	Phase            RegistrationPhase `json:"phase"`
	Health           HealthState       `json:"health"`
	LeaseExpiresAtMS int64             `json:"lease_expires_at_ms"`
	LeaseRemainingMS int64             `json:"lease_remaining_ms"`
	RenewalCount     int64             `json:"renewal_count"`
}

type TruthStep struct {
	Seq              int                `json:"seq"`
	Tick             int64              `json:"tick"`
	AtMS             int64              `json:"at_ms"`
	Instances        []TruthInstance    `json:"instances"`
	DueProbes        []ProbeRef         `json:"due_probes,omitempty"`
	SuppressedProbes []SuppressedProbe  `json:"suppressed_probes,omitempty"`
	DeferredProbes   []DeferredProbe    `json:"deferred_probes,omitempty"`
	DroppedProbes    []DroppedProbe     `json:"dropped_probes,omitempty"`
	Transitions      []HealthTransition `json:"transitions,omitempty"`
	Catalogs         []Catalog          `json:"catalogs"`
	Queries          []QueryResult      `json:"queries,omitempty"`
}

type TruthReport struct {
	ProjectVersion string      `json:"project_version"`
	ModelHash      string      `json:"model_hash"`
	Steps          []TruthStep `json:"steps"`
}

func (r *RunResult) Truth() TruthReport {
	report := TruthReport{ProjectVersion: r.ProjectVersion, ModelHash: r.ModelHash}
	for _, step := range r.Steps {
		truth := TruthStep{
			Seq:              step.Seq,
			Tick:             step.Tick,
			AtMS:             step.AtMS,
			DueProbes:        step.DueProbes,
			SuppressedProbes: step.SuppressedProbes,
			DeferredProbes:   step.DeferredProbes,
			DroppedProbes:    step.DroppedProbes,
			Transitions:      step.Transitions,
			Catalogs:         step.Catalogs,
			Queries:          step.Queries,
		}
		for _, instance := range step.Instances {
			truth.Instances = append(truth.Instances, TruthInstance{
				InstanceID:       instance.InstanceID,
				ServiceID:        instance.ServiceID,
				Phase:            instance.Phase,
				Health:           instance.Health,
				LeaseExpiresAtMS: instance.LeaseExpiresAtMS,
				LeaseRemainingMS: instance.LeaseRemainingMS,
				RenewalCount:     instance.RenewalCount,
			})
		}
		report.Steps = append(report.Steps, truth)
	}
	return report
}
