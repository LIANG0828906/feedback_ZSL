package model

type HeadDecision struct {
	RequestID     string `json:"request_id"`
	TraceID       string `json:"trace_id"`
	EdgeID        string `json:"edge_id"`
	EdgeNo        int    `json:"edge_no"`
	Bucket        uint32 `json:"bucket"`
	RatePermyriad uint32 `json:"rate_permyriad"`
	Keep          bool   `json:"keep"`
	DecisionType  string `json:"decision_type"`
	Basis         string `json:"basis"`
}

type TailDecision struct {
	TraceID          string   `json:"trace_id"`
	Keep             bool     `json:"keep"`
	DecisionType     string   `json:"decision_type"`
	Forced           bool     `json:"forced"`
	ForcedEdgeIDs    []string `json:"forced_edge_ids,omitempty"`
	EvaluatedRuleIDs []string `json:"evaluated_rule_ids"`
	MatchedRuleIDs   []string `json:"matched_rule_ids"`
	SelectedRuleID   string   `json:"selected_rule_id,omitempty"`
	Path             []string `json:"path"`
	BasisSpanIDs     []string `json:"basis_span_ids"`
}

type ServiceBudgetSnapshot struct {
	ServiceID       string `json:"service_id"`
	LimitSpans      uint64 `json:"limit_spans"`
	ChargedSpans    uint64 `json:"charged_spans"`
	ForcedSpans     uint64 `json:"forced_spans"`
	RemainingSpans  int64  `json:"remaining_spans"`
	RejectedTraceID string `json:"rejected_trace_id,omitempty"`
	RejectReason    string `json:"reject_reason,omitempty"`
}

type BudgetDecision struct {
	EventSeq         int                     `json:"event_seq"`
	TraceID          string                  `json:"trace_id"`
	ServiceID        string                  `json:"service_id"`
	WindowStartMS    int64                   `json:"window_start_ms"`
	WindowEndMS      int64                   `json:"window_end_ms"`
	WindowBasis      string                  `json:"window_basis"`
	CostSpans        int                     `json:"cost_spans"`
	GlobalLimitSpans uint64                  `json:"global_limit_spans"`
	GlobalUsedSpans  uint64                  `json:"global_used_spans"`
	GlobalRemaining  int64                   `json:"global_remaining"`
	Accepted         bool                    `json:"accepted"`
	Charged          bool                    `json:"charged"`
	Overshot         bool                    `json:"overshot"`
	RejectCode       string                  `json:"reject_code,omitempty"`
	RejectReason     string                  `json:"reject_reason,omitempty"`
	Services         []ServiceBudgetSnapshot `json:"services"`
}

type MergedSpan struct {
	Span
	EffectiveParentSpanID string `json:"effective_parent_span_id,omitempty"`
	MergeState            string `json:"merge_state"`
	OrphanCode            string `json:"orphan_code,omitempty"`
	OrphanReason          string `json:"orphan_reason,omitempty"`
}

type UnmergedSpan struct {
	SpanID       string `json:"span_id"`
	TraceID      string `json:"trace_id"`
	ParentSpanID string `json:"parent_span_id"`
	Code         string `json:"code"`
	Reason       string `json:"reason"`
	Action       string `json:"action"`
}

type MergeResult struct {
	TraceID          string            `json:"trace_id"`
	Status           string            `json:"status"`
	OrderedSpanIDs   []string          `json:"ordered_span_ids"`
	ParentRefs       map[string]string `json:"parent_refs"`
	Spans            []MergedSpan      `json:"spans"`
	UnmergedSpans    []UnmergedSpan    `json:"unmerged_spans"`
	DuplicateSpanIDs []string          `json:"duplicate_span_ids"`
	Reasons          []string          `json:"reasons,omitempty"`
}

type EventView struct {
	Seq         int             `json:"seq"`
	Kind        EventKind       `json:"kind"`
	TimestampMS int64           `json:"timestamp_ms"`
	Label       string          `json:"label"`
	Head        *HeadDecision   `json:"head,omitempty"`
	Tail        *TailDecision   `json:"tail,omitempty"`
	Budget      *BudgetDecision `json:"budget,omitempty"`
	Merge       *MergeResult    `json:"merge,omitempty"`
}

type Report struct {
	ProjectID     string                  `json:"project_id"`
	SourceVersion string                  `json:"source_version,omitempty"`
	Head          []HeadDecision          `json:"head"`
	Tail          []TailDecision          `json:"tail"`
	Budget        []BudgetDecision        `json:"budget"`
	Merge         map[string]*MergeResult `json:"merge"`
	RetainedSpans map[string][]string     `json:"retained_spans"`
	Events        []EventView             `json:"events"`
}

type SeqRange struct {
	FromSeq int `json:"from_seq"`
	ToSeq   int `json:"to_seq"`
}

type InvalidationNode struct {
	NodeID          string `json:"node_id"`
	Kind            string `json:"kind"`
	Ref             string `json:"ref"`
	EventSeq        int    `json:"event_seq,omitempty"`
	RootSource      string `json:"root_source"`
	ImmediateSource string `json:"immediate_source"`
	Reason          string `json:"reason"`
}

type Closure struct {
	EditType    string             `json:"edit_type"`
	RootSource  string             `json:"root_source"`
	Nodes       []InvalidationNode `json:"nodes"`
	EventRanges []SeqRange         `json:"event_ranges"`
	Recomputed  []string           `json:"recomputed"`
	Reused      []string           `json:"reused"`
	Differences []string           `json:"differences,omitempty"`
}
