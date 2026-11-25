package engine

import "alertkernel/model"

const (
	StateInactive = "inactive"
	StatePending  = "pending"
	StateFiring   = "firing"

	ReasonSuppressed = "suppressed"
	ReasonSilenced   = "silenced"
)

type SourceRef struct {
	SeriesID    string       `json:"series_id"`
	Metric      string       `json:"metric"`
	Labels      model.Labels `json:"labels"`
	SampleIndex int          `json:"sample_index"`
	SampleAtMS  int64        `json:"sample_at_ms"`
	Raw         int64        `json:"raw"`
}

type Observation struct {
	RuleID       string       `json:"rule_id"`
	Order        int          `json:"order"`
	InstanceKey  string       `json:"instance_key"`
	Labels       model.Labels `json:"labels"`
	Matched      bool         `json:"matched"`
	HasData      bool         `json:"has_data"`
	NoDataReason string       `json:"no_data_reason,omitempty"`
	ValueRaw     *int64       `json:"value_raw,omitempty"`
	Overflow     bool         `json:"overflow_detected,omitempty"`
	Saturated    bool         `json:"saturated,omitempty"`
	Sources      []SourceRef  `json:"sources,omitempty"`
}

type State struct {
	RuleID         string       `json:"rule_id"`
	InstanceKey    string       `json:"instance_key"`
	Labels         model.Labels `json:"labels"`
	State          string       `json:"state"`
	EnteredT       int          `json:"entered_t"`
	EnteredAtMS    int64        `json:"entered_at_ms"`
	PendingSinceT  *int         `json:"pending_since_t,omitempty"`
	LastMatchT     *int         `json:"last_match_t,omitempty"`
	DurationMS     int64        `json:"duration_ms"`
	LastTransition string       `json:"last_transition,omitempty"`
	Changed        bool         `json:"changed"`
	BasisNo        int          `json:"basis_no"`
	BasisID        string       `json:"basis_id,omitempty"`
	FireBasisNo    int          `json:"fire_basis_no,omitempty"`
	FireBasisID    string       `json:"fire_basis_id,omitempty"`
	ResolveBasisNo int          `json:"resolve_basis_no,omitempty"`
	ResolveBasisID string       `json:"resolve_basis_id,omitempty"`
}

type SuppressionDecision struct {
	RuleID             string       `json:"rule_id"`
	InstanceKey        string       `json:"instance_key"`
	Labels             model.Labels `json:"labels"`
	PrimaryReason      string       `json:"primary_reason,omitempty"`
	InhibitionID       string       `json:"inhibition_id,omitempty"`
	SuppressionEventID string       `json:"suppression_event_id,omitempty"`
	SourceRuleID       string       `json:"source_rule_id,omitempty"`
	SourceInstanceKey  string       `json:"source_instance_key,omitempty"`
	SilenceID          string       `json:"silence_id,omitempty"`
	StartT             int          `json:"start_t"`
	EndT               int          `json:"end_t"`
	StartAtMS          int64        `json:"start_at_ms"`
	EndAtMS            int64        `json:"end_at_ms"`
	Comment            string       `json:"comment,omitempty"`
	Suppressed         bool         `json:"suppressed"`
	Silenced           bool         `json:"silenced"`
}

type VisibleAlert struct {
	RuleID      string       `json:"rule_id"`
	InstanceKey string       `json:"instance_key"`
	Labels      model.Labels `json:"labels"`
	State       string       `json:"state"`
	Priority    int          `json:"priority"`
}

type NotificationMember struct {
	RuleID      string       `json:"rule_id"`
	InstanceKey string       `json:"instance_key"`
	Labels      model.Labels `json:"labels"`
	BasisNo     int          `json:"basis_no"`
	BasisID     string       `json:"basis_id"`
}

type NotificationBatch struct {
	BatchKey     string               `json:"batch_key"`
	DedupKey     string               `json:"dedup_key"`
	Receiver     string               `json:"receiver"`
	RouteID      string               `json:"route_id"`
	RoutingBasis string               `json:"routing_basis"`
	T            int                  `json:"t"`
	AtMS         int64                `json:"at_ms"`
	Members      []NotificationMember `json:"members"`
}

type RuleScheduling struct {
	RuleID       string `json:"rule_id"`
	SeriesCount  int    `json:"series_count"`
	BufferChunks int    `json:"buffer_chunks"`
	Backpressure int    `json:"backpressure_events"`
	DurationNS   int64  `json:"duration_ns,omitempty"`
}

type SchedulingReport struct {
	Workers            int              `json:"workers"`
	PerRuleBuffer      int              `json:"per_rule_buffer"`
	RuleCount          int              `json:"rule_count"`
	BackpressureEvents int              `json:"backpressure_events"`
	Rules              []RuleScheduling `json:"rules"`
}

type TimeResult struct {
	T                   int                   `json:"t"`
	AtMS                int64                 `json:"at_ms"`
	Observations        []Observation         `json:"observations"`
	States              []State               `json:"states"`
	Visible             []VisibleAlert        `json:"visible"`
	SuppressionDetails  []SuppressionDecision `json:"suppression_details,omitempty"`
	Notifications       []NotificationBatch   `json:"notifications,omitempty"`
	BasisSeqAfter       int                   `json:"basis_seq_after"`
	Scheduling          SchedulingReport      `json:"scheduling"`
	InvalidationSources []string              `json:"invalidation_sources,omitempty"`
}

type RunReport struct {
	ProjectID   string       `json:"project_id"`
	RuleVersion string       `json:"rule_version"`
	Times       []TimeResult `json:"times"`
}
