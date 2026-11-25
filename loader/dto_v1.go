package loader

import "chaosdiag/model"

type v1Doc struct {
	SchemaVersion         string         `json:"schema_version"`
	ProjectName           string         `json:"project_name"`
	ProjectRevision       string         `json:"project_revision"`
	Seed                  int64          `json:"seed"`
	LogicalTickMS         int64          `json:"logical_tick_ms"`
	GlobalTimeoutBudgetMS int64          `json:"global_timeout_budget_ms"`
	AdmissionBudgetMS     int64          `json:"admission_budget_ms"`
	MaxGlobalInjections   int            `json:"max_global_injections"`
	MaxServiceInjections  int            `json:"max_service_injections"`
	MaxServiceByTarget    map[string]int `json:"max_service_injections_by_target"`
	ManagedServices       []v1Service    `json:"managed_services"`
	CallDependencies      []v1Edge       `json:"call_dependencies"`
	FlatSelectors         []v1Selector   `json:"flat_selectors"`
	FaultCommands         []v1Command    `json:"fault_commands"`
	BreakerExemptions     []v1Exemption  `json:"breaker_exemptions"`
	RequestFlow           []v1Request    `json:"request_flow"`
}

type v1Service struct {
	Name             string `json:"name"`
	BaseLatencyMS    int64  `json:"base_latency_ms"`
	SelfErrorCode    string `json:"self_error_code,omitempty"`
	SelfErrorMessage string `json:"self_error_message,omitempty"`
}

type v1Edge struct {
	EdgeID           string `json:"edge_id"`
	Caller           string `json:"caller"`
	Callee           string `json:"callee"`
	NetworkLatencyMS int64  `json:"network_latency_ms"`
}

type v1Selector struct {
	SelectorID    string `json:"selector_id"`
	TargetType    string `json:"target_type"`
	TargetService string `json:"target_service,omitempty"`
	EdgeID        string `json:"edge_id,omitempty"`
	Caller        string `json:"caller,omitempty"`
	Callee        string `json:"callee,omitempty"`
	PathContains  string `json:"path_contains,omitempty"`
}

type v1Command struct {
	CommandID         string `json:"command_id"`
	SubmittedAtTick   int    `json:"submitted_at_logical_tick"`
	FaultType         string `json:"fault_type"`
	Priority          int    `json:"priority"`
	SelectorID        string `json:"selector_id"`
	LatencyMS         *int64 `json:"latency_ms,omitempty"`
	LatencyTicks      *int64 `json:"latency_ticks,omitempty"`
	DeadlineMS        *int64 `json:"deadline_ms,omitempty"`
	ErrorCode         string `json:"error_code,omitempty"`
	Copies            int    `json:"copies,omitempty"`
	ExtraCallMS       int64  `json:"extra_call_ms,omitempty"`
	ReorderDepth      int    `json:"reorder_depth,omitempty"`
	BudgetCostMS      int64  `json:"budget_cost_ms,omitempty"`
	EffectiveFrom     *int   `json:"effective_from_tick,omitempty"`
	EffectiveUntil    *int   `json:"effective_until_tick,omitempty"`
	EffectiveStart    *int   `json:"effective_start_tick,omitempty"`
	EffectiveDuration *int   `json:"effective_duration_ticks,omitempty"`
}

type v1Exemption struct {
	ExemptionID string     `json:"exemption_id"`
	BreakerID   string     `json:"breaker_id"`
	Reason      string     `json:"reason,omitempty"`
	CommandID   string     `json:"command_id,omitempty"`
	Selector    v1Selector `json:"selector"`
}

type v1Request struct {
	RequestID       string           `json:"request_id"`
	SubmittedAtTick int              `json:"submitted_at_logical_tick"`
	EntryService    string           `json:"entry_service"`
	CallPath        []string         `json:"call_path"`
	BudgetMS        int64            `json:"timeout_budget_ms,omitempty"`
	Expected        v1ExpectedResult `json:"expected_result"`
}

type v1ExpectedResult struct {
	TerminalState string `json:"terminal_state"`
	ErrorCode     string `json:"error_code,omitempty"`
	MaxTotalMS    int64  `json:"max_total_ms,omitempty"`
	MaxCopies     int    `json:"max_copies,omitempty"`
}

func convertV1(doc *v1Doc) (*model.Project, error) {
	tickMS := doc.LogicalTickMS
	if tickMS <= 0 {
		tickMS = 50
	}
	selectors := map[string]model.Selector{}
	for _, raw := range doc.FlatSelectors {
		sel := model.Selector{
			ID: raw.SelectorID, Kind: mapTargetType(raw.TargetType), Service: raw.TargetService,
			EdgeID: raw.EdgeID, From: raw.Caller, To: raw.Callee, PathContains: raw.PathContains,
		}
		selectors[sel.ID] = sel
	}

	instructions := make([]model.Instruction, 0, len(doc.FaultCommands))
	for i, raw := range doc.FaultCommands {
		sel, err := resolveV1Selector(raw.SelectorID, selectors, i)
		if err != nil {
			return nil, err
		}
		start, end, err := normalizeWindow(raw.EffectiveFrom, raw.EffectiveUntil, raw.EffectiveStart, raw.EffectiveDuration, "fault_commands")
		if err != nil {
			return nil, err
		}
		delay, err := normalizeMSOrTicks(raw.LatencyMS, raw.LatencyTicks, tickMS, "fault_commands["+itoa(i)+"].latency")
		if err != nil {
			return nil, err
		}
		timeout, err := normalizeMSOrTicks(raw.DeadlineMS, nil, tickMS, "fault_commands["+itoa(i)+"].deadline")
		if err != nil {
			return nil, err
		}
		kind := mapV1Kind(raw.FaultType)
		instructions = append(instructions, model.Instruction{
			ID: raw.CommandID, Sequence: i, SubmittedTick: raw.SubmittedAtTick, Kind: kind,
			Priority: raw.Priority, SelectorID: raw.SelectorID, Selector: sel,
			StartTick: start, EndTick: end, DelayMS: delay, TimeoutMS: timeout,
			FailureCode: raw.ErrorCode, RepeatCount: raw.Copies, RepeatDelayMS: raw.ExtraCallMS,
			ReorderDepth: raw.ReorderDepth, BudgetCostMS: raw.BudgetCostMS,
			SourceAmplitude: "v1:absolute_ms", SourceWindow: "v1:absolute_range",
		})
	}

	exemptions := make([]model.Exemption, 0, len(doc.BreakerExemptions))
	for _, raw := range doc.BreakerExemptions {
		exemptions = append(exemptions, model.Exemption{
			ID: raw.ExemptionID, Source: firstNonEmpty(raw.BreakerID, raw.ExemptionID),
			Reason: raw.Reason, InstructionID: raw.CommandID,
			Selector: model.Selector{
				ID: raw.Selector.SelectorID, Kind: mapTargetType(raw.Selector.TargetType),
				Service: raw.Selector.TargetService, EdgeID: raw.Selector.EdgeID,
				From: raw.Selector.Caller, To: raw.Selector.Callee, PathContains: raw.Selector.PathContains,
			},
		})
	}

	reqs := make([]model.Request, 0, len(doc.RequestFlow))
	for i, raw := range doc.RequestFlow {
		reqs = append(reqs, model.Request{
			ID: raw.RequestID, Sequence: i, Tick: raw.SubmittedAtTick, Entry: raw.EntryService,
			Path: raw.CallPath, BudgetMS: raw.BudgetMS,
			Expected: model.ExpectedResult{
				Status: mapStatus(raw.Expected.TerminalState), Code: raw.Expected.ErrorCode,
				MaxTotalMS: raw.Expected.MaxTotalMS, MaxRepeats: raw.Expected.MaxCopies,
			},
		})
	}

	return &model.Project{
		Schema: model.SchemaV1, ProjectID: doc.ProjectName, ProjectVersion: doc.ProjectRevision,
		Seed: doc.Seed, TickMS: tickMS, RequestTimeoutBudgetMS: doc.GlobalTimeoutBudgetMS,
		AdmissionTimeoutBudgetMS: doc.AdmissionBudgetMS,
		Limits:                   model.Limits{GlobalConcurrent: doc.MaxGlobalInjections, ServiceConcurrentDefault: doc.MaxServiceInjections, ServiceConcurrent: doc.MaxServiceByTarget},
		Services:                 convertV1Services(doc.ManagedServices), Edges: convertV1Edges(doc.CallDependencies),
		Selectors: selectorValues(selectors), Instructions: instructions, Exemptions: exemptions, Requests: reqs,
	}, nil
}

func resolveV1Selector(id string, table map[string]model.Selector, index int) (model.Selector, error) {
	if id == "" {
		return model.Selector{}, model.NewError(model.CodeFieldRequired, "fault_commands["+itoa(index)+"].selector_id", "selector id is required")
	}
	sel, ok := table[id]
	if !ok {
		err := model.NewError(model.CodeFieldInvalid, "fault_commands["+itoa(index)+"].selector_id", "selector %q not found in flat table", id)
		return model.Selector{}, model.WithContext(err, "selector_id", id)
	}
	return sel, nil
}

func convertV1Services(in []v1Service) []model.Service {
	out := make([]model.Service, 0, len(in))
	for _, s := range in {
		out = append(out, model.Service{Name: s.Name, LatencyMS: s.BaseLatencyMS, SelfErrorCode: s.SelfErrorCode, SelfErrorMessage: s.SelfErrorMessage})
	}
	return out
}

func convertV1Edges(in []v1Edge) []model.Edge {
	out := make([]model.Edge, 0, len(in))
	for _, e := range in {
		out = append(out, model.Edge{ID: e.EdgeID, From: e.Caller, To: e.Callee, LatencyMS: e.NetworkLatencyMS})
	}
	return out
}

func selectorValues(m map[string]model.Selector) []model.Selector {
	out := make([]model.Selector, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func mapTargetType(v string) string {
	switch v {
	case "service", "SERVICE", "managed_service":
		return model.TargetService
	case "edge", "call", "dependency", "EDGE":
		return model.TargetEdge
	default:
		return v
	}
}

func mapV1Kind(v string) string {
	switch v {
	case "latency", "delay":
		return model.KindDelay
	case "deadline", "timeout":
		return model.KindTimeout
	case "error", "failure", "fail":
		return model.KindFailure
	case "copies", "duplicate", "repeat":
		return model.KindDuplicate
	case "reorder", "out_of_order":
		return model.KindReorder
	default:
		return v
	}
}

func mapStatus(v string) string {
	switch v {
	case "ok", "success", "succeeded", model.StatusCompleted:
		return model.StatusCompleted
	case "deadline_exceeded", model.StatusTimeout:
		return model.StatusTimeout
	case "error", model.StatusFailure:
		return model.StatusFailure
	default:
		return v
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
