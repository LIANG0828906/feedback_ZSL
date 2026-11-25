package loader

type v1Document struct {
	SchemaVersion      string         `json:"schema_version"`
	ProjectID          string         `json:"project_id"`
	Seed               int64          `json:"seed"`
	ClockOriginEpochMS int64          `json:"clock_origin_epoch_ms"`
	MillisPerSeqStep   int64          `json:"millis_per_seq_step"`
	BudgetPolicy       v1Budget       `json:"budget_policy"`
	OrphanPolicy       string         `json:"orphan_policy"`
	Services           []v1Service    `json:"services"`
	CallEdges          []v1Edge       `json:"call_edges"`
	ServiceAllocations []v1Allocation `json:"service_allocations"`
	TailRules          []v1TailRule   `json:"tail_rules"`
	Events             []v1Event      `json:"events"`
}

type v1Budget struct {
	WindowMS          int64  `json:"window_ms"`
	GlobalLimitSpans  uint64 `json:"global_limit_spans"`
	OnGlobalExhausted string `json:"on_global_exhausted"`
}

type v1Service struct {
	ServiceID   string `json:"service_id"`
	DisplayName string `json:"display_name"`
}

type v1Edge struct {
	EdgeNo           int      `json:"edge_no"`
	EdgeID           string   `json:"edge_id"`
	FromServiceID    string   `json:"from_service_id"`
	ToServiceID      string   `json:"to_service_id"`
	BaseRatePermille uint32   `json:"base_rate_permille"`
	ForcedRetention  bool     `json:"forced_retention"`
	TailRuleIDs      []string `json:"tail_rule_ids"`
}

type v1Allocation struct {
	ServiceID          string `json:"service_id"`
	AllocationPermille uint32 `json:"allocation_permille"`
}

type v1TailRule struct {
	RuleID           string      `json:"rule_id"`
	Priority         int         `json:"priority"`
	DeclarationOrder int         `json:"declaration_order"`
	EdgeID           string      `json:"edge_id"`
	Condition        v1Condition `json:"condition"`
	DependsOn        []string    `json:"depends_on"`
}

type v1Condition struct {
	ConditionKind   string `json:"condition_kind"`
	FieldName       string `json:"field_name"`
	Operator        string `json:"operator"`
	ComparisonValue string `json:"comparison_value"`
	ReferenceRule   string `json:"reference_rule"`
}

type v1Event struct {
	Sequence          int          `json:"sequence"`
	EventKind         string       `json:"event_kind"`
	OccurredAtEpochMS int64        `json:"occurred_at_epoch_ms"`
	Enter             *v1Enter     `json:"enter"`
	Propagate         *v1Propagate `json:"propagate"`
	Span              *v1Span      `json:"span"`
	Settle            *v1Settle    `json:"settle"`
}

type v1Enter struct {
	RequestID string `json:"request_id"`
	TraceID   string `json:"trace_id"`
	EdgeID    string `json:"edge_id"`
}

type v1Propagate struct {
	RequestID string `json:"request_id"`
	TraceID   string `json:"trace_id"`
	EdgeID    string `json:"edge_id"`
}

type v1Settle struct {
	TraceID    string `json:"trace_id"`
	RootSpanID string `json:"root_span_id"`
	ServiceID  string `json:"service_id"`
}

type v1Span struct {
	SpanID       string            `json:"span_id"`
	TraceID      string            `json:"trace_id"`
	ParentSpanID string            `json:"parent_span_id"`
	ServiceID    string            `json:"service_id"`
	EdgeID       string            `json:"edge_id"`
	CausalOrder  int               `json:"causal_order"`
	SpanName     string            `json:"span_name"`
	Status       string            `json:"status"`
	LatencyMS    uint64            `json:"latency_ms"`
	Tags         map[string]string `json:"tags"`
	ArrivalSeq   int               `json:"arrival_seq"`
}

type v2Document struct {
	SchemaVersion      string             `json:"schemaVersion"`
	ProjectID          string             `json:"projectId"`
	Seed               int64              `json:"seed"`
	ClockOriginEpochMs int64              `json:"clockOriginEpochMs"`
	MillisPerSeqStep   int64              `json:"millisPerSeqStep"`
	BudgetPolicy       v2Budget           `json:"budgetPolicy"`
	OrphanPolicy       string             `json:"orphanPolicy"`
	Services           []v2Service        `json:"services"`
	CallEdges          []v2Edge           `json:"callEdges"`
	AllocationTree     []v2AllocationNode `json:"allocationTree"`
	TailRules          []v2TailRule       `json:"tailRules"`
	Events             []v2Event          `json:"events"`
}

type v2Budget struct {
	WindowMs          int64  `json:"windowMs"`
	GlobalLimitSpans  uint64 `json:"globalLimitSpans"`
	OnGlobalExhausted string `json:"onGlobalExhausted"`
}

type v2Service struct {
	ServiceID   string `json:"serviceId"`
	DisplayName string `json:"displayName"`
}

type v2Edge struct {
	EdgeNo        int      `json:"edgeNo"`
	EdgeID        string   `json:"edgeId"`
	FromServiceID string   `json:"fromServiceId"`
	ToServiceID   string   `json:"toServiceId"`
	BaseRateBp    uint32   `json:"baseRateBp"`
	ForceKeep     bool     `json:"forceKeep,omitempty"`
	TailRuleIDs   []string `json:"tailRuleIds,omitempty"`
}

type v2AllocationNode struct {
	ServiceID string             `json:"serviceId"`
	WeightBp  uint32             `json:"weightBp"`
	Children  []v2AllocationNode `json:"children"`
}

type v2TailRule struct {
	RuleID           string      `json:"ruleId"`
	Priority         int         `json:"priority"`
	DeclarationOrder int         `json:"declarationOrder"`
	EdgeID           string      `json:"edgeId"`
	When             v2Condition `json:"when"`
	DependsOn        []string    `json:"dependsOn"`
}

type v2Condition struct {
	Kind          string `json:"kind"`
	Field         string `json:"field"`
	Operator      string `json:"operator"`
	Value         string `json:"value"`
	ReferenceRule string `json:"referenceRule"`
}

type v2Event struct {
	Seq           int          `json:"seq"`
	Kind          string       `json:"kind"`
	AtEventOffset int          `json:"atEventOffset"`
	Enter         *v2Enter     `json:"enter"`
	Propagate     *v2Propagate `json:"propagate"`
	Span          *v2Span      `json:"span"`
	Settle        *v2Settle    `json:"settle"`
}

type v2Enter struct {
	RequestID string `json:"requestId"`
	TraceID   string `json:"traceId"`
	EdgeID    string `json:"edgeId"`
}

type v2Propagate struct {
	RequestID string `json:"requestId"`
	TraceID   string `json:"traceId"`
	EdgeID    string `json:"edgeId"`
}

type v2Settle struct {
	TraceID    string `json:"traceId"`
	RootSpanID string `json:"rootSpanId"`
	ServiceID  string `json:"serviceId"`
}

type v2Span struct {
	SpanID       string            `json:"spanId"`
	TraceID      string            `json:"traceId"`
	ParentSpanID string            `json:"parentSpanId"`
	ServiceID    string            `json:"serviceId"`
	EdgeID       string            `json:"edgeId"`
	CausalOrder  int               `json:"causalOrder"`
	SpanName     string            `json:"spanName"`
	Status       string            `json:"status"`
	LatencyMS    uint64            `json:"latencyMs"`
	Tags         map[string]string `json:"tags"`
	ArrivalSeq   int               `json:"arrivalSeq"`
}

type v3Document struct {
	SchemaVersion      string         `json:"SchemaVersion"`
	ProjectID          string         `json:"ProjectID"`
	Seed               int64          `json:"Seed"`
	ClockOriginEpochMS int64          `json:"ClockOriginEpochMS"`
	MillisPerSeqStep   int64          `json:"MillisPerSeqStep"`
	BudgetPolicy       v3Budget       `json:"BudgetPolicy"`
	OrphanPolicy       string         `json:"OrphanPolicy"`
	Services           []v3Service    `json:"Services"`
	Edges              []v3Edge       `json:"Edges"`
	ServiceShares      []v3Allocation `json:"ServiceShares"`
	TailRules          []v3TailRule   `json:"TailRules"`
	Events             []v3Event      `json:"Events"`
}

type v3Budget struct {
	WindowMS          int64  `json:"WindowMS"`
	GlobalLimitSpans  uint64 `json:"GlobalLimitSpans"`
	OnGlobalExhausted string `json:"OnGlobalExhausted"`
}

type v3Service struct {
	ID   string `json:"ID"`
	Name string `json:"Name"`
}

type v3Edge struct {
	No               int      `json:"No"`
	ID               string   `json:"ID"`
	From             string   `json:"From"`
	To               string   `json:"To"`
	BaseRatePermille uint32   `json:"BaseRatePermille"`
	Forced           bool     `json:"Forced"`
	TailRuleIDs      []string `json:"TailRuleIDs"`
}

type v3Allocation struct {
	ServiceID     string `json:"ServiceID"`
	SharePermille uint32 `json:"SharePermille"`
}

type v3TailRule struct {
	ID        string      `json:"ID"`
	Priority  int         `json:"Priority"`
	Order     int         `json:"Order"`
	EdgeID    string      `json:"EdgeID"`
	Condition v3Condition `json:"Condition"`
	DependsOn []string    `json:"DependsOn"`
}

type v3Condition struct {
	Kind  string `json:"Kind"`
	Field string `json:"Field"`
	Op    string `json:"Op"`
	Value string `json:"Value"`
	Ref   string `json:"Ref"`
}

type v3Event struct {
	Sequence  int          `json:"Sequence"`
	Type      string       `json:"Type"`
	EpochMS   int64        `json:"EpochMS"`
	Enter     *v3Enter     `json:"Enter"`
	Propagate *v3Propagate `json:"Propagate"`
	Span      *v3Span      `json:"Span"`
	Settle    *v3Settle    `json:"Settle"`
}

type v3Enter struct {
	RequestID string `json:"RequestID"`
	TraceID   string `json:"TraceID"`
	EdgeID    string `json:"EdgeID"`
}

type v3Propagate struct {
	RequestID string `json:"RequestID"`
	TraceID   string `json:"TraceID"`
	EdgeID    string `json:"EdgeID"`
}

type v3Settle struct {
	TraceID    string `json:"TraceID"`
	RootSpanID string `json:"RootSpanID"`
	ServiceID  string `json:"ServiceID"`
}

type v3Span struct {
	SpanID       string            `json:"SpanID"`
	TraceID      string            `json:"TraceID"`
	ParentSpanID string            `json:"ParentSpanID"`
	ServiceID    string            `json:"ServiceID"`
	EdgeID       string            `json:"EdgeID"`
	CausalOrder  int               `json:"CausalOrder"`
	SpanName     string            `json:"SpanName"`
	Status       string            `json:"Status"`
	LatencyMS    uint64            `json:"LatencyMS"`
	Tags         map[string]string `json:"Tags"`
	ArrivalSeq   int               `json:"ArrivalSeq"`
}
