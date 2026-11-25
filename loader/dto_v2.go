package loader

import "chaosdiag/model"

type v2Doc struct {
	APIVersion string `json:"apiVersion"`
	Metadata   struct {
		ProjectID string `json:"projectId"`
		Revision  string `json:"revision"`
		Seed      int64  `json:"seed"`
	} `json:"metadata"`
	Timing struct {
		TickDurationMS    int64 `json:"tickDurationMs"`
		RequestBudgetMS   int64 `json:"requestBudgetMs"`
		AdmissionBudgetMS int64 `json:"admissionBudgetMs"`
	} `json:"timing"`
	Guardrails struct {
		GlobalConcurrency       int            `json:"globalConcurrency"`
		ServiceConcurrency      int            `json:"serviceConcurrency"`
		ServiceConcurrencyBySvc map[string]int `json:"serviceConcurrencyByService"`
	} `json:"guardrails"`
	Services   []v2Service   `json:"services"`
	Edges      []v2Edge      `json:"edges"`
	Selectors  []v2Selector  `json:"selectorTable"`
	Rules      []v2Rule      `json:"rules"`
	Exemptions []v2Exemption `json:"circuitBreakerExemptions"`
	Requests   []v2Request   `json:"requests"`
}

type v2Service struct {
	Name            string `json:"name"`
	ProcessingMS    int64  `json:"processingMs"`
	OwnErrorCode    string `json:"ownErrorCode,omitempty"`
	OwnErrorMessage string `json:"ownErrorMessage,omitempty"`
}

type v2Edge struct {
	ID        string `json:"id"`
	Source    string `json:"source"`
	Target    string `json:"target"`
	LatencyMS int64  `json:"latencyMs"`
}

type v2Selector struct {
	ID           string `json:"id"`
	TargetType   string `json:"targetType"`
	ServiceName  string `json:"serviceName,omitempty"`
	EdgeID       string `json:"edgeId,omitempty"`
	Source       string `json:"source,omitempty"`
	Target       string `json:"target,omitempty"`
	PathContains string `json:"pathContains,omitempty"`
}

type v2Rule struct {
	ID                  string `json:"id"`
	SubmittedTick       int    `json:"submittedTick"`
	InjectionType       string `json:"injectionType"`
	Priority            int    `json:"priority"`
	SelectorRef         string `json:"selectorRef"`
	LatencyTicks        *int64 `json:"latencyTicks,omitempty"`
	LatencyMS           *int64 `json:"latencyMs,omitempty"`
	TimeoutTicks        *int64 `json:"timeoutTicks,omitempty"`
	TimeoutMS           *int64 `json:"timeoutMs,omitempty"`
	FailureCode         string `json:"failureCode,omitempty"`
	DuplicateCount      int    `json:"duplicateCount,omitempty"`
	DuplicateDelayTicks *int64 `json:"duplicateDelayTicks,omitempty"`
	ReorderDepth        int    `json:"reorderDepth,omitempty"`
	BudgetCostMS        int64  `json:"budgetCostMs,omitempty"`
	StartTick           *int   `json:"startTick,omitempty"`
	DurationTicks       *int   `json:"durationTicks,omitempty"`
	EndTick             *int   `json:"endTick,omitempty"`
}

type v2Exemption struct {
	ID         string     `json:"id"`
	BreakerRef string     `json:"breakerRef"`
	Reason     string     `json:"reason,omitempty"`
	RuleID     string     `json:"ruleId,omitempty"`
	Selector   v2Selector `json:"selector"`
}

type v2Request struct {
	ID          string        `json:"id"`
	AtTick      int           `json:"atTick"`
	Entry       string        `json:"entry"`
	Path        []string      `json:"path"`
	BudgetMS    int64         `json:"budgetMs,omitempty"`
	Expectation v2Expectation `json:"expectation"`
}

type v2Expectation struct {
	Outcome    string `json:"outcome"`
	Code       string `json:"code,omitempty"`
	MaxTotalMs int64  `json:"maxTotalMs,omitempty"`
	MaxRepeats int    `json:"maxRepeats,omitempty"`
}

func convertV2(doc *v2Doc) (*model.Project, error) {
	tickMS := doc.Timing.TickDurationMS
	if tickMS <= 0 {
		tickMS = 50
	}
	sels := map[string]model.Selector{}
	for _, raw := range doc.Selectors {
		sels[raw.ID] = model.Selector{
			ID: raw.ID, Kind: mapTargetType(raw.TargetType), Service: raw.ServiceName,
			EdgeID: raw.EdgeID, From: raw.Source, To: raw.Target, PathContains: raw.PathContains,
		}
	}
	rules := make([]model.Instruction, 0, len(doc.Rules))
	for i, raw := range doc.Rules {
		sel, err := resolveV2Selector(raw.SelectorRef, sels, i)
		if err != nil {
			return nil, err
		}
		start, end, err := normalizeWindow(raw.StartTick, raw.EndTick, nil, raw.DurationTicks, "rules["+itoa(i)+"].effectiveWindow")
		if err != nil {
			return nil, err
		}
		delay, err := normalizeMSOrTicks(raw.LatencyMS, raw.LatencyTicks, tickMS, "rules["+itoa(i)+"].latency")
		if err != nil {
			return nil, err
		}
		timeout, err := normalizeMSOrTicks(raw.TimeoutMS, raw.TimeoutTicks, tickMS, "rules["+itoa(i)+"].timeout")
		if err != nil {
			return nil, err
		}
		repeatDelay, err := normalizeMSOrTicks(nil, raw.DuplicateDelayTicks, tickMS, "rules["+itoa(i)+"].duplicateDelay")
		if err != nil {
			return nil, err
		}
		rules = append(rules, model.Instruction{
			ID: raw.ID, Sequence: i, SubmittedTick: raw.SubmittedTick, Kind: mapV2Kind(raw.InjectionType),
			Priority: raw.Priority, SelectorID: raw.SelectorRef, Selector: sel, StartTick: start, EndTick: end,
			DelayMS: delay, TimeoutMS: timeout, FailureCode: raw.FailureCode, RepeatCount: raw.DuplicateCount,
			RepeatDelayMS: repeatDelay, ReorderDepth: raw.ReorderDepth, BudgetCostMS: raw.BudgetCostMS,
			SourceAmplitude: "v2:ticks", SourceWindow: "v2:relative_duration",
		})
	}
	exemptions := make([]model.Exemption, 0, len(doc.Exemptions))
	for _, raw := range doc.Exemptions {
		exemptions = append(exemptions, model.Exemption{
			ID: raw.ID, Source: firstNonEmpty(raw.BreakerRef, raw.ID), Reason: raw.Reason, InstructionID: raw.RuleID,
			Selector: model.Selector{
				ID: raw.Selector.ID, Kind: mapTargetType(raw.Selector.TargetType), Service: raw.Selector.ServiceName,
				EdgeID: raw.Selector.EdgeID, From: raw.Selector.Source, To: raw.Selector.Target, PathContains: raw.Selector.PathContains,
			},
		})
	}
	reqs := make([]model.Request, 0, len(doc.Requests))
	for i, raw := range doc.Requests {
		reqs = append(reqs, model.Request{
			ID: raw.ID, Sequence: i, Tick: raw.AtTick, Entry: raw.Entry, Path: raw.Path, BudgetMS: raw.BudgetMS,
			Expected: model.ExpectedResult{Status: mapStatus(raw.Expectation.Outcome), Code: raw.Expectation.Code, MaxTotalMS: raw.Expectation.MaxTotalMs, MaxRepeats: raw.Expectation.MaxRepeats},
		})
	}
	return &model.Project{
		Schema: model.SchemaV2, ProjectID: doc.Metadata.ProjectID, ProjectVersion: doc.Metadata.Revision, Seed: doc.Metadata.Seed,
		TickMS: tickMS, RequestTimeoutBudgetMS: doc.Timing.RequestBudgetMS, AdmissionTimeoutBudgetMS: doc.Timing.AdmissionBudgetMS,
		Limits:   model.Limits{GlobalConcurrent: doc.Guardrails.GlobalConcurrency, ServiceConcurrentDefault: doc.Guardrails.ServiceConcurrency, ServiceConcurrent: doc.Guardrails.ServiceConcurrencyBySvc},
		Services: convertV2Services(doc.Services), Edges: convertV2Edges(doc.Edges), Selectors: selectorValues(sels),
		Instructions: rules, Exemptions: exemptions, Requests: reqs,
	}, nil
}

func resolveV2Selector(id string, table map[string]model.Selector, index int) (model.Selector, error) {
	if id == "" {
		return model.Selector{}, model.NewError(model.CodeFieldRequired, "rules["+itoa(index)+"].selectorRef", "selector ref is required")
	}
	sel, ok := table[id]
	if !ok {
		return model.Selector{}, model.NewError(model.CodeFieldInvalid, "rules["+itoa(index)+"].selectorRef", "selector %q not found", id)
	}
	return sel, nil
}

func convertV2Services(in []v2Service) []model.Service {
	out := make([]model.Service, 0, len(in))
	for _, s := range in {
		out = append(out, model.Service{Name: s.Name, LatencyMS: s.ProcessingMS, SelfErrorCode: s.OwnErrorCode, SelfErrorMessage: s.OwnErrorMessage})
	}
	return out
}

func convertV2Edges(in []v2Edge) []model.Edge {
	out := make([]model.Edge, 0, len(in))
	for _, e := range in {
		out = append(out, model.Edge{ID: e.ID, From: e.Source, To: e.Target, LatencyMS: e.LatencyMS})
	}
	return out
}

func mapV2Kind(v string) string { return mapV1Kind(v) }
