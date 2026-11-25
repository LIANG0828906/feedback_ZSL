package loader

import "chaosdiag/model"

type v3Doc struct {
	APISchema string `json:"api_schema"`
	Spec      v3Spec `json:"spec"`
}

type v3Spec struct {
	ProjectID         string       `json:"projectId"`
	Revision          string       `json:"revision"`
	DeterministicSeed int64        `json:"deterministicSeed"`
	Timing            v3Timing     `json:"timing"`
	Guardrails        v3Guardrails `json:"guardrails"`
	Topology          v3Topology   `json:"topology"`
	ChaosRules        []v3Rule     `json:"chaosRules"`
	Resilience        v3Resilience `json:"resilience"`
	Traffic           []v3Request  `json:"traffic"`
}

type v3Timing struct {
	TickDurationMS    int64 `json:"tickDurationMs"`
	RequestBudgetMS   int64 `json:"requestBudgetMs"`
	AdmissionBudgetMS int64 `json:"admissionBudgetMs"`
}

type v3Guardrails struct {
	GlobalMaxActive     int            `json:"globalMaxActive"`
	ServiceMaxActive    int            `json:"serviceMaxActive"`
	ServiceMaxActiveMap map[string]int `json:"serviceMaxActiveByService"`
}

type v3Topology struct {
	Services []v3Service `json:"services"`
	Calls    []v3Call    `json:"calls"`
}

type v3Service struct {
	UniqueName string         `json:"uniqueName"`
	ProcessMS  int64          `json:"processMs"`
	Failure    *v3SelfFailure `json:"selfFailure,omitempty"`
}

type v3SelfFailure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type v3Call struct {
	CallID    string `json:"callId"`
	Caller    string `json:"caller"`
	Callee    string `json:"callee"`
	LatencyMS int64  `json:"latencyMs"`
}

type v3Rule struct {
	RuleID        string       `json:"ruleId"`
	SubmittedTick int          `json:"submittedTick"`
	FaultKind     string       `json:"faultKind"`
	PriorityRank  int          `json:"priorityRank"`
	MatchTree     v3MatchTree  `json:"matchTree"`
	Amplitude     *v3Amplitude `json:"amplitude,omitempty"`
	FailureCode   string       `json:"failureCode,omitempty"`
	Duplicates    int          `json:"duplicates,omitempty"`
	ReorderDepth  int          `json:"reorderDepth,omitempty"`
	BudgetCostMS  int64        `json:"budgetCostMs,omitempty"`
	Effective     v3Effective  `json:"effective"`
}

type v3MatchTree struct {
	Label    string        `json:"label,omitempty"`
	Service  string        `json:"service,omitempty"`
	EdgeID   string        `json:"edgeId,omitempty"`
	Caller   string        `json:"caller,omitempty"`
	Callee   string        `json:"callee,omitempty"`
	PathLike string        `json:"pathLike,omitempty"`
	Operator string        `json:"operator,omitempty"`
	All      []v3MatchTree `json:"all,omitempty"`
	Any      []v3MatchTree `json:"any,omitempty"`
}

type v3Amplitude struct {
	Mode             string       `json:"mode"`
	Selection        string       `json:"selection,omitempty"`
	AbsoluteMS       *int64       `json:"absoluteMs,omitempty"`
	TickValue        *int64       `json:"tickValue,omitempty"`
	Manifest         []v3Quantile `json:"manifest,omitempty"`
	DuplicateDelayMS int64        `json:"duplicateDelayMs,omitempty"`
}

type v3Quantile struct {
	Quantile string `json:"quantile"`
	ValueMS  int64  `json:"valueMs"`
}

type v3Effective struct {
	StartTick int `json:"startTick"`
	EndTick   int `json:"endTick"`
}

type v3Resilience struct {
	ExemptRules []v3Exemption `json:"exemptRules"`
}

type v3Exemption struct {
	ExemptionID string      `json:"exemptionId"`
	BreakerName string      `json:"breakerName"`
	Reason      string      `json:"reason,omitempty"`
	RuleID      string      `json:"ruleId,omitempty"`
	MatchTree   v3MatchTree `json:"matchTree"`
}

type v3Request struct {
	RequestID   string        `json:"requestId"`
	Moment      int           `json:"moment"`
	EntryPoint  string        `json:"entryPoint"`
	CallChain   []string      `json:"callChain"`
	BudgetMS    int64         `json:"budgetMs,omitempty"`
	Expectation v3Expectation `json:"expectation"`
}

type v3Expectation struct {
	Result       string `json:"result"`
	ErrorCode    string `json:"errorCode,omitempty"`
	MaxElapsedMS int64  `json:"maxElapsedMs,omitempty"`
	MaxCopies    int    `json:"maxCopies,omitempty"`
}

func convertV3(doc *v3Doc) (*model.Project, error) {
	spec := doc.Spec
	tickMS := spec.Timing.TickDurationMS
	if tickMS <= 0 {
		tickMS = 50
	}
	services := make([]model.Service, 0, len(spec.Topology.Services))
	for _, s := range spec.Topology.Services {
		svc := model.Service{Name: s.UniqueName, LatencyMS: s.ProcessMS}
		if s.Failure != nil {
			svc.SelfErrorCode = s.Failure.Code
			svc.SelfErrorMessage = s.Failure.Message
		}
		services = append(services, svc)
	}
	edges := make([]model.Edge, 0, len(spec.Topology.Calls))
	for _, e := range spec.Topology.Calls {
		edges = append(edges, model.Edge{ID: e.CallID, From: e.Caller, To: e.Callee, LatencyMS: e.LatencyMS})
	}

	selectors := []model.Selector{}
	instructions := make([]model.Instruction, 0, len(spec.ChaosRules))
	for i, raw := range spec.ChaosRules {
		sel, tree, err := selectorFromV3Tree(raw.MatchTree, "chaosRules["+itoa(i)+"].matchTree")
		if err != nil {
			return nil, err
		}
		sel.ID = firstNonEmpty(raw.RuleID+"-selector", sel.ID)
		sel.Tree = tree
		selectors = append(selectors, sel)
		amplitudeMS, repeatDelay, err := amplitudesFromV3(raw.Amplitude, tickMS, i)
		if err != nil {
			return nil, err
		}
		var delay, timeout int64
		if mapV3Kind(raw.FaultKind) == model.KindTimeout {
			timeout = amplitudeMS
		} else {
			delay = amplitudeMS
		}
		instructions = append(instructions, model.Instruction{
			ID: raw.RuleID, Sequence: i, SubmittedTick: raw.SubmittedTick, Kind: mapV3Kind(raw.FaultKind),
			Priority: raw.PriorityRank, SelectorID: sel.ID, Selector: sel,
			StartTick: raw.Effective.StartTick, EndTick: raw.Effective.EndTick,
			DelayMS: delay, TimeoutMS: timeout, FailureCode: raw.FailureCode,
			RepeatCount: raw.Duplicates, RepeatDelayMS: repeatDelay, ReorderDepth: raw.ReorderDepth,
			BudgetCostMS: raw.BudgetCostMS, SourceAmplitude: "v3:quantile_manifest", SourceWindow: "v3:absolute_range",
		})
	}

	exemptions := make([]model.Exemption, 0, len(spec.Resilience.ExemptRules))
	for i, raw := range spec.Resilience.ExemptRules {
		sel, tree, err := selectorFromV3Tree(raw.MatchTree, "resilience.exemptRules["+itoa(i)+"].matchTree")
		if err != nil {
			return nil, err
		}
		sel.Tree = tree
		selectors = append(selectors, sel)
		exemptions = append(exemptions, model.Exemption{
			ID: raw.ExemptionID, Source: firstNonEmpty(raw.BreakerName, raw.ExemptionID), Reason: raw.Reason,
			InstructionID: raw.RuleID, Selector: sel,
		})
	}

	reqs := make([]model.Request, 0, len(spec.Traffic))
	for i, raw := range spec.Traffic {
		reqs = append(reqs, model.Request{
			ID: raw.RequestID, Sequence: i, Tick: raw.Moment, Entry: raw.EntryPoint, Path: raw.CallChain,
			BudgetMS: raw.BudgetMS,
			Expected: model.ExpectedResult{Status: mapStatus(raw.Expectation.Result), Code: raw.Expectation.ErrorCode, MaxTotalMS: raw.Expectation.MaxElapsedMS, MaxRepeats: raw.Expectation.MaxCopies},
		})
	}

	return &model.Project{
		Schema: model.SchemaV3, ProjectID: spec.ProjectID, ProjectVersion: spec.Revision, Seed: spec.DeterministicSeed,
		TickMS: tickMS, RequestTimeoutBudgetMS: spec.Timing.RequestBudgetMS, AdmissionTimeoutBudgetMS: spec.Timing.AdmissionBudgetMS,
		Limits:   model.Limits{GlobalConcurrent: spec.Guardrails.GlobalMaxActive, ServiceConcurrentDefault: spec.Guardrails.ServiceMaxActive, ServiceConcurrent: spec.Guardrails.ServiceMaxActiveMap},
		Services: services, Edges: edges, Selectors: selectors, Instructions: instructions, Exemptions: exemptions, Requests: reqs,
	}, nil
}

func mapV3Kind(v string) string { return mapV1Kind(v) }

func amplitudesFromV3(a *v3Amplitude, tickMS int64, ruleIndex int) (amplitudeMS int64, repeatDelayMS int64, err error) {
	if a == nil {
		return 0, 0, nil
	}
	field := "chaosRules[" + itoa(ruleIndex) + "].amplitude"
	if len(a.Manifest) > 0 {
		points := make([]quantilePoint, 0, len(a.Manifest))
		for _, q := range a.Manifest {
			points = append(points, quantilePoint{Quantile: q.Quantile, ValueMS: q.ValueMS})
		}
		selectedMS, qerr := quantileValue(points, a.Selection, field)
		err = qerr
		if err != nil {
			return 0, 0, err
		}
		if a.AbsoluteMS != nil && *a.AbsoluteMS != selectedMS {
			return 0, 0, model.NewError(model.CodeFieldConflict, field, "absoluteMs=%d conflicts with selected quantile=%d", *a.AbsoluteMS, selectedMS)
		}
		amplitudeMS = selectedMS
	} else {
		amplitudeMS, err = normalizeMSOrTicks(a.AbsoluteMS, a.TickValue, tickMS, field)
		if err != nil {
			return 0, 0, err
		}
	}
	return amplitudeMS, a.DuplicateDelayMS, nil
}

func selectorFromV3Tree(node v3MatchTree, field string) (model.Selector, *model.MatchNode, error) {
	sel := model.Selector{PathContains: node.PathLike}
	tree := &model.MatchNode{Label: node.Label, Service: node.Service, EdgeID: node.EdgeID, From: node.Caller, To: node.Callee}

	merge := func(next model.Selector, child *model.MatchNode) error {
		if next.Service != "" {
			if sel.Service != "" && sel.Service != next.Service {
				return model.NewError(model.CodeFieldConflict, field+".service", "match tree ambiguously targets %q and %q", sel.Service, next.Service)
			}
			sel.Service = next.Service
		}
		if next.EdgeID != "" {
			if sel.EdgeID != "" && sel.EdgeID != next.EdgeID {
				return model.NewError(model.CodeFieldConflict, field+".edgeId", "match tree ambiguously targets edges %q and %q", sel.EdgeID, next.EdgeID)
			}
			sel.EdgeID = next.EdgeID
		}
		if next.From != "" {
			sel.From = next.From
		}
		if next.To != "" {
			sel.To = next.To
		}
		if next.PathContains != "" {
			sel.PathContains = next.PathContains
		}
		if child != nil {
			tree.Children = append(tree.Children, *child)
		}
		return nil
	}

	if node.Service != "" || node.EdgeID != "" || node.Caller != "" || node.Callee != "" {
		_ = merge(model.Selector{Service: node.Service, EdgeID: node.EdgeID, From: node.Caller, To: node.Callee, PathContains: node.PathLike}, nil)
	}
	for _, child := range node.All {
		childSel, childTree, err := selectorFromV3Tree(child, field+".all")
		if err != nil {
			return model.Selector{}, nil, err
		}
		childTree.Operator = "all"
		if err := merge(childSel, childTree); err != nil {
			return model.Selector{}, nil, err
		}
	}
	for _, child := range node.Any {
		childSel, childTree, err := selectorFromV3Tree(child, field+".any")
		if err != nil {
			return model.Selector{}, nil, err
		}
		childTree.Operator = "any"
		if err := merge(childSel, childTree); err != nil {
			return model.Selector{}, nil, err
		}
	}

	if sel.EdgeID != "" || sel.From != "" {
		sel.Kind = model.TargetEdge
	} else if sel.Service != "" {
		sel.Kind = model.TargetService
	} else {
		return model.Selector{}, nil, model.NewError(model.CodeFieldRequired, field, "match tree must target a service or edge")
	}
	tree.Kind = sel.Kind
	return sel, tree, nil
}
