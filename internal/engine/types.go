package engine

import "grayrelease/internal/model"

type Config struct {
	WorkerBudget                int    `json:"worker_budget"`
	RouteBufferPerService       int    `json:"route_buffer_per_service"`
	MirrorConcurrencyPerService int    `json:"mirror_concurrency_per_service"`
	ShadowBufferPerService      int    `json:"shadow_buffer_per_service"`
	ShadowOnFull                string `json:"shadow_on_full"`
	ShadowLatencyTicks          int    `json:"shadow_latency_ticks"`
	ShadowTimeoutTicks          int    `json:"shadow_timeout_ticks"`
}

func DefaultConfig() Config {
	return Config{
		WorkerBudget:                4,
		RouteBufferPerService:       32,
		MirrorConcurrencyPerService: 2,
		ShadowBufferPerService:      8,
		ShadowOnFull:                "drop",
		ShadowLatencyTicks:          1,
		ShadowTimeoutTicks:          2,
	}
}

type Decision struct {
	Tick                int               `json:"tick"`
	RequestID           string            `json:"request_id"`
	Service             string            `json:"service"`
	Key                 string            `json:"key"`
	Labels              map[string]string `json:"labels,omitempty"`
	CandidateRuleIDs    []string          `json:"candidate_rule_ids,omitempty"`
	SelectedRuleID      string            `json:"selected_rule_id,omitempty"`
	SelectedRuleNo      int               `json:"selected_rule_no,omitempty"`
	AdjudicationBasis   string            `json:"adjudication_basis"`
	Bucket              int               `json:"bucket"`
	WeightBPS           int               `json:"weight_bps"`
	Side                string            `json:"side"`
	MainTarget          string            `json:"main_target"`
	MainAllowed         bool              `json:"main_allowed"`
	MainConclusion      string            `json:"main_conclusion"`
	MirrorPolicyPresent bool              `json:"mirror_policy_present"`
	MirrorSampled       bool              `json:"mirror_sampled"`
	MirrorSamplePresent bool              `json:"mirror_sample_present"`
	MirrorBucket        int               `json:"mirror_bucket,omitempty"`
	MirrorRatioBPS      int               `json:"mirror_ratio_bps,omitempty"`
	MirrorSelected      bool              `json:"mirror_selected"`
	MirrorRuleID        string            `json:"mirror_rule_id,omitempty"`
	MirrorRuleNo        int               `json:"mirror_rule_no,omitempty"`
	ShadowStatus        string            `json:"shadow_status,omitempty"`
	ShadowCode          string            `json:"shadow_code,omitempty"`
	Constraint          string            `json:"constraint,omitempty"`
	ShadowOutcome       string            `json:"shadow_outcome,omitempty"`
	CacheState          string            `json:"cache_state,omitempty"`
	InvalidationSources []string          `json:"invalidation_sources,omitempty"`
}

type ConstraintViolation struct {
	Tick       int    `json:"tick"`
	Service    string `json:"service"`
	RequestID  string `json:"request_id"`
	Code       string `json:"code"`
	Constraint string `json:"constraint"`
	Action     string `json:"action"`
}

type ServiceTickReport struct {
	Tick                int                   `json:"tick"`
	Service             string                `json:"service"`
	MainAllowed         int                   `json:"main_allowed"`
	BaselineCount       int                   `json:"baseline_count"`
	CanaryCount         int                   `json:"canary_count"`
	MirrorCandidates    int                   `json:"mirror_candidates"`
	MirrorSelected      int                   `json:"mirror_selected"`
	Queued              []string              `json:"queued,omitempty"`
	Dispatched          []string              `json:"dispatched,omitempty"`
	Completed           []string              `json:"completed,omitempty"`
	Dropped             []string              `json:"dropped,omitempty"`
	Deferred            []string              `json:"deferred,omitempty"`
	QueueOccupancy      int                   `json:"queue_occupancy"`
	ActiveOccupancy     int                   `json:"active_occupancy"`
	Violations          []ConstraintViolation `json:"violations,omitempty"`
	CacheState          string                `json:"cache_state,omitempty"`
	InvalidationSources []string              `json:"invalidation_sources,omitempty"`
}

type TickReport struct {
	Tick                int                          `json:"tick"`
	MainAllowed         int                          `json:"main_allowed"`
	BaselineCount       int                          `json:"baseline_count"`
	CanaryCount         int                          `json:"canary_count"`
	MirrorSelected      int                          `json:"mirror_selected"`
	QueueOccupancy      map[string]int               `json:"queue_occupancy"`
	ActiveOccupancy     map[string]int               `json:"active_occupancy"`
	DroppedSet          []string                     `json:"dropped_set,omitempty"`
	DeferredSet         []string                     `json:"deferred_set,omitempty"`
	Violations          []ConstraintViolation        `json:"violations,omitempty"`
	Services            map[string]ServiceTickReport `json:"services"`
	CacheState          string                       `json:"cache_state,omitempty"`
	InvalidationSources []string                     `json:"invalidation_sources,omitempty"`
}

type SimulationResult struct {
	Decisions []Decision   `json:"decisions"`
	Reports   []TickReport `json:"reports"`
	Config    Config       `json:"config"`
}

type Migration struct {
	Key           string `json:"key"`
	Service       string `json:"service"`
	RuleID        string `json:"rule_id"`
	OldSide       string `json:"old_side"`
	NewSide       string `json:"new_side"`
	OldBucket     int    `json:"old_bucket"`
	NewBucket     int    `json:"new_bucket"`
	OldWeightBPS  int    `json:"old_weight_bps"`
	NewWeightBPS  int    `json:"new_weight_bps"`
	AllowedReason string `json:"allowed_reason"`
}

var _ = model.CanonicalBasis
