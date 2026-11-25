package gen

import (
	"encoding/json"

	"chaosdiag/model"
)

func RenderAll(p *model.Project) (map[string][]byte, error) {
	v1, err := RenderV1(p)
	if err != nil {
		return nil, err
	}
	v2, err := RenderV2(p)
	if err != nil {
		return nil, err
	}
	v3, err := RenderV3(p)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{model.SchemaV1: v1, model.SchemaV2: v2, model.SchemaV3: v3}, nil
}

func RenderV1(p *model.Project) ([]byte, error) {
	services := []map[string]any{}
	for _, s := range p.Services {
		services = append(services, omitEmpty(map[string]any{
			"name": s.Name, "base_latency_ms": s.LatencyMS,
			"self_error_code": s.SelfErrorCode, "self_error_message": s.SelfErrorMessage,
		}))
	}
	edges := []map[string]any{}
	for _, e := range p.Edges {
		edges = append(edges, map[string]any{"edge_id": e.ID, "caller": e.From, "callee": e.To, "network_latency_ms": e.LatencyMS})
	}
	selectors := []map[string]any{}
	for _, s := range p.Selectors {
		selectors = append(selectors, omitEmpty(map[string]any{
			"selector_id": s.ID, "target_type": s.Kind, "target_service": s.Service,
			"edge_id": s.EdgeID, "caller": s.From, "callee": s.To, "path_contains": s.PathContains,
		}))
	}
	commands := []map[string]any{}
	for _, in := range p.Instructions {
		cmd := map[string]any{
			"command_id": in.ID, "submitted_at_logical_tick": in.SubmittedTick,
			"fault_type": v1Kind(in.Kind), "priority": in.Priority, "selector_id": in.SelectorID,
			"effective_from_tick": in.StartTick, "effective_until_tick": in.EndTick,
			"budget_cost_ms": in.BudgetCostMS,
		}
		if in.DelayMS != 0 {
			cmd["latency_ms"] = in.DelayMS
		}
		if in.TimeoutMS != 0 {
			cmd["deadline_ms"] = in.TimeoutMS
		}
		if in.FailureCode != "" {
			cmd["error_code"] = in.FailureCode
		}
		if in.RepeatCount != 0 {
			cmd["copies"] = in.RepeatCount
			cmd["extra_call_ms"] = in.RepeatDelayMS
		}
		if in.ReorderDepth != 0 {
			cmd["reorder_depth"] = in.ReorderDepth
		}
		commands = append(commands, cmd)
	}
	exemptions := []map[string]any{}
	for _, ex := range p.Exemptions {
		exemptions = append(exemptions, map[string]any{
			"exemption_id": ex.ID, "breaker_id": ex.Source, "reason": ex.Reason, "command_id": ex.InstructionID,
			"selector": omitEmpty(map[string]any{
				"selector_id": ex.Selector.ID, "target_type": ex.Selector.Kind, "target_service": ex.Selector.Service,
				"edge_id": ex.Selector.EdgeID, "caller": ex.Selector.From, "callee": ex.Selector.To,
			}),
		})
	}
	requests := []map[string]any{}
	for _, r := range p.Requests {
		requests = append(requests, omitEmptyMap(map[string]any{
			"request_id": r.ID, "submitted_at_logical_tick": r.Tick, "entry_service": r.Entry,
			"call_path": r.Path, "timeout_budget_ms": r.BudgetMS,
			"expected_result": omitEmpty(map[string]any{
				"terminal_state": v1Status(r.Expected.Status), "error_code": r.Expected.Code,
				"max_total_ms": r.Expected.MaxTotalMS, "max_copies": r.Expected.MaxRepeats,
			}),
		}))
	}
	doc := map[string]any{
		"schema_version": p.Schema, "project_name": p.ProjectID, "project_revision": p.ProjectVersion,
		"seed": p.Seed, "logical_tick_ms": p.TickMS, "global_timeout_budget_ms": p.RequestTimeoutBudgetMS,
		"admission_budget_ms": p.AdmissionTimeoutBudgetMS, "max_global_injections": p.Limits.GlobalConcurrent,
		"max_service_injections":           p.Limits.ServiceConcurrentDefault,
		"max_service_injections_by_target": p.Limits.ServiceConcurrent,
		"managed_services":                 services, "call_dependencies": edges, "flat_selectors": selectors,
		"fault_commands": commands, "breaker_exemptions": exemptions, "request_flow": requests,
	}
	return json.MarshalIndent(doc, "", "  ")
}

func RenderV2(p *model.Project) ([]byte, error) {
	services := []map[string]any{}
	for _, s := range p.Services {
		services = append(services, omitEmpty(map[string]any{
			"name": s.Name, "processingMs": s.LatencyMS, "ownErrorCode": s.SelfErrorCode, "ownErrorMessage": s.SelfErrorMessage,
		}))
	}
	edges := []map[string]any{}
	for _, e := range p.Edges {
		edges = append(edges, map[string]any{"id": e.ID, "source": e.From, "target": e.To, "latencyMs": e.LatencyMS})
	}
	selectors := []map[string]any{}
	for _, s := range p.Selectors {
		selectors = append(selectors, omitEmpty(map[string]any{
			"id": s.ID, "targetType": s.Kind, "serviceName": s.Service, "edgeId": s.EdgeID,
			"source": s.From, "target": s.To, "pathContains": s.PathContains,
		}))
	}
	rules := []map[string]any{}
	for _, in := range p.Instructions {
		rule := map[string]any{
			"id": in.ID, "submittedTick": in.SubmittedTick, "injectionType": in.Kind,
			"priority": in.Priority, "selectorRef": in.SelectorID,
			"startTick": in.StartTick, "durationTicks": in.EndTick - in.StartTick + 1,
			"budgetCostMs": in.BudgetCostMS,
		}
		if in.DelayMS != 0 {
			rule["latencyTicks"] = in.DelayMS / p.TickMS
		}
		if in.TimeoutMS != 0 {
			rule["timeoutTicks"] = in.TimeoutMS / p.TickMS
		}
		if in.FailureCode != "" {
			rule["failureCode"] = in.FailureCode
		}
		if in.RepeatCount != 0 {
			rule["duplicateCount"] = in.RepeatCount
			rule["duplicateDelayTicks"] = in.RepeatDelayMS / p.TickMS
		}
		if in.ReorderDepth != 0 {
			rule["reorderDepth"] = in.ReorderDepth
		}
		rules = append(rules, rule)
	}
	exemptions := []map[string]any{}
	for _, ex := range p.Exemptions {
		exemptions = append(exemptions, map[string]any{
			"id": ex.ID, "breakerRef": ex.Source, "reason": ex.Reason, "ruleId": ex.InstructionID,
			"selector": omitEmpty(map[string]any{
				"id": ex.Selector.ID, "targetType": ex.Selector.Kind, "serviceName": ex.Selector.Service,
				"edgeId": ex.Selector.EdgeID, "source": ex.Selector.From, "target": ex.Selector.To,
			}),
		})
	}
	requests := []map[string]any{}
	for _, r := range p.Requests {
		requests = append(requests, omitEmptyMap(map[string]any{
			"id": r.ID, "atTick": r.Tick, "entry": r.Entry, "path": r.Path, "budgetMs": r.BudgetMS,
			"expectation": omitEmpty(map[string]any{
				"outcome": r.Expected.Status, "code": r.Expected.Code,
				"maxTotalMs": r.Expected.MaxTotalMS, "maxRepeats": r.Expected.MaxRepeats,
			}),
		}))
	}
	doc := map[string]any{
		"apiVersion": model.SchemaV2,
		"metadata":   map[string]any{"projectId": p.ProjectID, "revision": p.ProjectVersion, "seed": p.Seed},
		"timing":     map[string]any{"tickDurationMs": p.TickMS, "requestBudgetMs": p.RequestTimeoutBudgetMS, "admissionBudgetMs": p.AdmissionTimeoutBudgetMS},
		"guardrails": map[string]any{
			"globalConcurrency": p.Limits.GlobalConcurrent, "serviceConcurrency": p.Limits.ServiceConcurrentDefault,
			"serviceConcurrencyByService": p.Limits.ServiceConcurrent,
		},
		"services": services, "edges": edges, "selectorTable": selectors, "rules": rules,
		"circuitBreakerExemptions": exemptions, "requests": requests,
	}
	return json.MarshalIndent(doc, "", "  ")
}

func RenderV3(p *model.Project) ([]byte, error) {
	services := []map[string]any{}
	for _, s := range p.Services {
		svc := map[string]any{"uniqueName": s.Name, "processMs": s.LatencyMS}
		if s.SelfErrorCode != "" {
			svc["selfFailure"] = map[string]any{"code": s.SelfErrorCode, "message": s.SelfErrorMessage}
		}
		services = append(services, svc)
	}
	calls := []map[string]any{}
	for _, e := range p.Edges {
		calls = append(calls, map[string]any{"callId": e.ID, "caller": e.From, "callee": e.To, "latencyMs": e.LatencyMS})
	}
	rules := []map[string]any{}
	for _, in := range p.Instructions {
		targetService := targetServiceForRender(p, in.Selector)
		tree := map[string]any{"label": "service:" + targetService, "service": targetService}
		if in.Selector.Kind == model.TargetEdge {
			tree["operator"] = "all"
			tree["all"] = []map[string]any{{"label": "edge:" + in.Selector.EdgeID, "edgeId": in.Selector.EdgeID}}
		}
		rule := map[string]any{
			"ruleId": in.ID, "submittedTick": in.SubmittedTick, "faultKind": in.Kind,
			"priorityRank": in.Priority, "matchTree": tree,
			"effective":    map[string]any{"startTick": in.StartTick, "endTick": in.EndTick},
			"budgetCostMs": in.BudgetCostMS,
		}
		amplitudeValue := in.DelayMS
		if in.Kind == model.KindTimeout {
			amplitudeValue = in.TimeoutMS
		}
		if amplitudeValue > 0 {
			rule["amplitude"] = map[string]any{
				"mode": "quantile_manifest", "selection": "P90",
				"manifest": []map[string]any{
					{"quantile": "P50", "valueMs": amplitudeValue / 2},
					{"quantile": "P90", "valueMs": amplitudeValue},
					{"quantile": "P99", "valueMs": amplitudeValue * 2},
				},
			}
		}
		if in.FailureCode != "" {
			rule["failureCode"] = in.FailureCode
		}
		if in.RepeatCount != 0 {
			rule["duplicates"] = in.RepeatCount
			if in.RepeatDelayMS != 0 {
				if amp, ok := rule["amplitude"].(map[string]any); ok {
					amp["duplicateDelayMs"] = in.RepeatDelayMS
				} else {
					rule["amplitude"] = map[string]any{"mode": "absolute", "duplicateDelayMs": in.RepeatDelayMS}
				}
			}
		}
		if in.ReorderDepth != 0 {
			rule["reorderDepth"] = in.ReorderDepth
		}
		rules = append(rules, rule)
	}
	exemptions := []map[string]any{}
	for _, ex := range p.Exemptions {
		tree := map[string]any{"label": "service:" + ex.Selector.Service, "service": ex.Selector.Service}
		exemptions = append(exemptions, map[string]any{
			"exemptionId": ex.ID, "breakerName": ex.Source, "reason": ex.Reason,
			"ruleId": ex.InstructionID, "matchTree": tree,
		})
	}
	traffic := []map[string]any{}
	for _, r := range p.Requests {
		traffic = append(traffic, omitEmptyMap(map[string]any{
			"requestId": r.ID, "moment": r.Tick, "entryPoint": r.Entry, "callChain": r.Path,
			"budgetMs": r.BudgetMS,
			"expectation": omitEmpty(map[string]any{
				"result": r.Expected.Status, "errorCode": r.Expected.Code,
				"maxElapsedMs": r.Expected.MaxTotalMS, "maxCopies": r.Expected.MaxRepeats,
			}),
		}))
	}
	doc := map[string]any{
		"api_schema": model.SchemaV3,
		"spec": map[string]any{
			"projectId": p.ProjectID, "revision": p.ProjectVersion, "deterministicSeed": p.Seed,
			"timing": map[string]any{"tickDurationMs": p.TickMS, "requestBudgetMs": p.RequestTimeoutBudgetMS, "admissionBudgetMs": p.AdmissionTimeoutBudgetMS},
			"guardrails": map[string]any{
				"globalMaxActive": p.Limits.GlobalConcurrent, "serviceMaxActive": p.Limits.ServiceConcurrentDefault,
				"serviceMaxActiveByService": p.Limits.ServiceConcurrent,
			},
			"topology":   map[string]any{"services": services, "calls": calls},
			"chaosRules": rules, "resilience": map[string]any{"exemptRules": exemptions}, "traffic": traffic,
		},
	}
	return json.MarshalIndent(doc, "", "  ")
}

func targetServiceForRender(p *model.Project, sel model.Selector) string {
	if sel.Kind == model.TargetService {
		return sel.Service
	}
	if edge, ok := p.EdgeByID(sel.EdgeID); ok {
		return edge.To
	}
	return sel.To
}

func v1Kind(kind string) string {
	switch kind {
	case model.KindDelay:
		return "latency"
	case model.KindTimeout:
		return "deadline"
	case model.KindFailure:
		return "error"
	case model.KindDuplicate:
		return "copies"
	default:
		return kind
	}
}

func v1Status(status string) string {
	if status == model.StatusTimeout {
		return "deadline_exceeded"
	}
	if status == model.StatusFailure {
		return "error"
	}
	return "success"
}

func omitEmpty(in map[string]any) map[string]any { return omitEmptyMap(in) }

func omitEmptyMap(in map[string]any) map[string]any {
	for k, v := range in {
		switch val := v.(type) {
		case string:
			if val == "" {
				delete(in, k)
			}
		case int:
			if val == 0 {
				delete(in, k)
			}
		case int64:
			if val == 0 {
				delete(in, k)
			}
		case map[string]any:
			if len(val) == 0 {
				delete(in, k)
			}
		}
	}
	return in
}
