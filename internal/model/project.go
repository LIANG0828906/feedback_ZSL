package model

const (
	SchemaV1            = "sampling.project/v1"
	SchemaV2            = "sampling.project/v2"
	SchemaV3            = "sampling.project/v3"
	SchemaV9Unsupported = "sampling.project/v9"

	ScalePermille    = "permille"
	ScalePermyriad   = "permyriad"
	OrphanQuarantine = "quarantine"
	OrphanAttach     = "attach_to_trace"

	GlobalExhaustReject              = "reject"
	GlobalExhaustAllowOvershoot      = "allow_overshoot"
	GlobalExhaustForcedOnlyOvershoot = "allow_forced_overshoot"
)

type Project struct {
	SchemaVersion    string       `json:"schema_version"`
	ProjectID        string       `json:"project_id"`
	Seed             int64        `json:"seed"`
	ClockOriginMS    int64        `json:"clock_origin_ms"`
	MillisPerSeqStep int64        `json:"millis_per_seq_step"`
	Budget           BudgetPolicy `json:"budget"`
	OrphanPolicy     string       `json:"orphan_policy"`
	Services         []Service    `json:"services"`
	Edges            []Edge       `json:"edges"`
	Allocations      []Allocation `json:"allocations"`
	TailRules        []TailRule   `json:"tail_rules"`
	Events           []Event      `json:"events"`
}

type BudgetPolicy struct {
	WindowMS             int64  `json:"window_ms"`
	GlobalLimitSpans     uint64 `json:"global_limit_spans"`
	GlobalExhaustionRule string `json:"global_exhaustion_rule"`
}

type Service struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Edge struct {
	No                int      `json:"no"`
	ID                string   `json:"id"`
	FromServiceID     string   `json:"from_service_id"`
	ToServiceID       string   `json:"to_service_id"`
	BaseRatePermyriad uint32   `json:"base_rate_permyriad"`
	ForcedRetention   bool     `json:"forced_retention"`
	TailRuleIDs       []string `json:"tail_rule_ids,omitempty"`
}

type Allocation struct {
	ServiceID       string `json:"service_id"`
	WeightPermyriad uint32 `json:"weight_permyriad"`
}

type TailRule struct {
	ID        string        `json:"id"`
	Priority  int           `json:"priority"`
	Order     int           `json:"order"`
	EdgeID    string        `json:"edge_id,omitempty"`
	Condition TailCondition `json:"condition"`
	DependsOn []string      `json:"depends_on,omitempty"`
}

type TailCondition struct {
	Kind  string `json:"kind"`
	Field string `json:"field,omitempty"`
	Op    string `json:"op,omitempty"`
	Value string `json:"value,omitempty"`
	Ref   string `json:"ref,omitempty"`
}

type EventKind string

const (
	EventRequestEnter EventKind = "request_enter"
	EventPropagate    EventKind = "propagate"
	EventSpanReport   EventKind = "span_report"
	EventRootSettle   EventKind = "root_settle"
)

type Event struct {
	Seq         int            `json:"seq"`
	Kind        EventKind      `json:"kind"`
	TimestampMS int64          `json:"timestamp_ms"`
	Enter       *EnterPayload  `json:"enter,omitempty"`
	Propagate   *EdgePayload   `json:"propagate,omitempty"`
	Span        *Span          `json:"span,omitempty"`
	Settle      *SettlePayload `json:"settle,omitempty"`
}

type EnterPayload struct {
	RequestID string `json:"request_id"`
	TraceID   string `json:"trace_id"`
	EdgeID    string `json:"edge_id"`
}

type EdgePayload struct {
	RequestID string `json:"request_id"`
	TraceID   string `json:"trace_id"`
	EdgeID    string `json:"edge_id"`
}

type SettlePayload struct {
	TraceID    string `json:"trace_id"`
	RootSpanID string `json:"root_span_id"`
	ServiceID  string `json:"service_id"`
}

type Span struct {
	SpanID       string            `json:"span_id"`
	TraceID      string            `json:"trace_id"`
	ParentSpanID string            `json:"parent_span_id,omitempty"`
	ServiceID    string            `json:"service_id"`
	EdgeID       string            `json:"edge_id,omitempty"`
	CausalOrder  int               `json:"causal_order"`
	Name         string            `json:"name"`
	Status       string            `json:"status"`
	LatencyMS    uint64            `json:"latency_ms"`
	Tags         map[string]string `json:"tags,omitempty"`
	ArrivalSeq   int               `json:"arrival_seq"`
}
