package projectvalidate

import (
	"fmt"
	"sort"

	"samplingkernel/internal/errs"
	"samplingkernel/internal/model"
)

func Validate(p *model.Project) error {
	if p == nil {
		return errs.New(errs.SchemaInvalidValue, "project is nil").AtPath("$")
	}
	if p.ProjectID == "" {
		return errs.New(errs.SchemaInvalidValue, "project_id is required").WithVersion(p.SchemaVersion).AtPath("$.project_id")
	}
	if p.Budget.WindowMS <= 0 {
		return errs.New(errs.SchemaInvalidValue, "budget.window_ms must be positive").WithVersion(p.SchemaVersion).AtPath("$.budget.window_ms")
	}
	if p.Budget.GlobalLimitSpans == 0 {
		return errs.New(errs.SchemaInvalidValue, "budget.global_limit_spans must be positive").WithVersion(p.SchemaVersion).AtPath("$.budget.global_limit_spans")
	}
	if p.OrphanPolicy != model.OrphanAttach && p.OrphanPolicy != model.OrphanQuarantine {
		return errs.New(errs.SchemaInvalidValue, "unsupported orphan policy").WithVersion(p.SchemaVersion).AtPath("$.orphan_policy")
	}

	serviceIDs := map[string]struct{}{}
	for i, service := range p.Services {
		if service.ID == "" {
			return errs.New(errs.SchemaInvalidValue, "service id is required").WithVersion(p.SchemaVersion).AtPath(fmt.Sprintf("$.services[%d].id", i))
		}
		if _, dup := serviceIDs[service.ID]; dup {
			return errs.New(errs.SchemaInvalidValue, "duplicate service id: "+service.ID).WithVersion(p.SchemaVersion).AtPath(fmt.Sprintf("$.services[%d].id", i))
		}
		serviceIDs[service.ID] = struct{}{}
	}
	tailRuleIDs := map[string]struct{}{}
	for _, rule := range p.TailRules {
		tailRuleIDs[rule.ID] = struct{}{}
	}

	for i, edge := range p.Edges {
		path := fmt.Sprintf("$.edges[%d]", i)
		if edge.ID == "" {
			return errs.New(errs.SchemaInvalidValue, "edge id is required").WithVersion(p.SchemaVersion).AtPath(path + ".id")
		}
		if edge.BaseRatePermyriad > 10000 {
			return errs.New(errs.RateOutOfRange, "base sampling rate must be in [0,10000] permyriad").
				WithVersion(p.SchemaVersion).AtPath(path + ".base_rate").WithDetail(fmt.Sprintf("edge=%s rate=%d", edge.ID, edge.BaseRatePermyriad))
		}
		if _, ok := serviceIDs[edge.FromServiceID]; !ok {
			return errs.New(errs.EdgeServiceMissing, "edge references missing from_service").
				WithVersion(p.SchemaVersion).AtPath(path + ".from_service_id").
				WithDetail(fmt.Sprintf("edge=%s service=%s", edge.ID, edge.FromServiceID))
		}
		if _, ok := serviceIDs[edge.ToServiceID]; !ok {
			return errs.New(errs.EdgeServiceMissing, "edge references missing to_service").
				WithVersion(p.SchemaVersion).AtPath(path + ".to_service_id").
				WithDetail(fmt.Sprintf("edge=%s service=%s", edge.ID, edge.ToServiceID))
		}
		for j, ruleID := range edge.TailRuleIDs {
			if _, ok := tailRuleIDs[ruleID]; !ok {
				return errs.New(errs.SchemaInvalidValue, "edge tail rule reference does not exist: "+ruleID).
					WithVersion(p.SchemaVersion).AtPath(fmt.Sprintf("%s.tail_rule_ids[%d]", path, j))
			}
		}
	}

	var allocationSum uint64
	for i, allocation := range p.Allocations {
		path := fmt.Sprintf("$.allocations[%d]", i)
		if _, ok := serviceIDs[allocation.ServiceID]; !ok {
			return errs.New(errs.SchemaInvalidValue, "allocation references missing service").
				WithVersion(p.SchemaVersion).AtPath(path + ".service_id").WithDetail("service=" + allocation.ServiceID)
		}
		if allocation.WeightPermyriad > 10000 {
			return errs.New(errs.AllocationSumExceeded, "allocation weight exceeds full scale").
				WithVersion(p.SchemaVersion).AtPath(path + ".weight_permyriad").
				WithDetail(fmt.Sprintf("service=%s weight=%d", allocation.ServiceID, allocation.WeightPermyriad))
		}
		allocationSum += uint64(allocation.WeightPermyriad)
	}
	if allocationSum > 10000 {
		return errs.New(errs.AllocationSumExceeded, "service allocation sum exceeds full scale").
			WithVersion(p.SchemaVersion).AtPath("$.allocations").
			WithDetail(fmt.Sprintf("sum_permyriad=%d max=10000", allocationSum))
	}
	if err := validateTailRules(p); err != nil {
		return err
	}
	if err := validateEvents(p); err != nil {
		return err
	}
	return nil
}

func validateTailRules(p *model.Project) error {
	ruleIDs := map[string]struct{}{}
	deps := map[string][]string{}
	for _, rule := range p.TailRules {
		if rule.ID == "" {
			return errs.New(errs.SchemaInvalidValue, "tail rule id is required").WithVersion(p.SchemaVersion).AtPath("$.tail_rules[].id")
		}
		ruleIDs[rule.ID] = struct{}{}
		deps[rule.ID] = append([]string{}, rule.DependsOn...)
		if rule.Condition.Kind == "ref" && rule.Condition.Ref != "" {
			deps[rule.ID] = append(deps[rule.ID], rule.Condition.Ref)
		}
	}
	for id := range deps {
		for _, dep := range deps[id] {
			if _, ok := ruleIDs[dep]; !ok {
				return errs.New(errs.SchemaInvalidValue, "tail rule dependency does not exist: "+dep).
					WithVersion(p.SchemaVersion).AtPath("$.tail_rules." + id + ".depends_on")
			}
		}
	}

	state := map[string]int{}
	var stack []string
	var visit func(string) error
	visit = func(id string) error {
		switch state[id] {
		case 1:
			cycle := append(append([]string{}, stack...), id)
			return errs.New(errs.TailConditionCycle, "tail condition dependency cycle").
				WithVersion(p.SchemaVersion).
				AtPath("$.tail_rules").
				WithDetail("cycle=" + joinCycle(cycle))
		case 2:
			return nil
		}
		state[id] = 1
		stack = append(stack, id)
		ordered := append([]string{}, deps[id]...)
		sort.Strings(ordered)
		for _, dep := range ordered {
			if err := visit(dep); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = 2
		return nil
	}
	ids := make([]string, 0, len(deps))
	for id := range deps {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func joinCycle(parts []string) string {
	out := ""
	for i, part := range parts {
		if i > 0 {
			out += " -> "
		}
		out += part
	}
	return out
}

func validateEvents(p *model.Project) error {
	edgeIDs := map[string]struct{}{}
	for _, edge := range p.Edges {
		edgeIDs[edge.ID] = struct{}{}
	}
	for i, event := range p.Events {
		path := fmt.Sprintf("$.events[%d]", i)
		if event.Seq != i+1 {
			return errs.New(errs.SchemaInvalidValue, "event seq must be dense and start at 1").
				WithVersion(p.SchemaVersion).AtPath(path + ".seq").
				WithDetail(fmt.Sprintf("expected=%d actual=%d", i+1, event.Seq))
		}
		switch event.Kind {
		case model.EventRequestEnter:
			if event.Enter == nil || event.Enter.RequestID == "" || event.Enter.TraceID == "" || event.Enter.EdgeID == "" {
				return errs.New(errs.SchemaInvalidValue, "request_enter requires request_id, trace_id and edge_id").WithVersion(p.SchemaVersion).AtPath(path + ".enter")
			}
			if _, ok := edgeIDs[event.Enter.EdgeID]; !ok {
				return errs.New(errs.EdgeServiceMissing, "enter event references missing edge").WithVersion(p.SchemaVersion).AtPath(path + ".enter.edge_id")
			}
		case model.EventPropagate:
			if event.Propagate == nil || event.Propagate.TraceID == "" || event.Propagate.EdgeID == "" {
				return errs.New(errs.SchemaInvalidValue, "propagate requires trace_id and edge_id").WithVersion(p.SchemaVersion).AtPath(path + ".propagate")
			}
			if _, ok := edgeIDs[event.Propagate.EdgeID]; !ok {
				return errs.New(errs.EdgeServiceMissing, "propagate event references missing edge").WithVersion(p.SchemaVersion).AtPath(path + ".propagate.edge_id")
			}
		case model.EventSpanReport:
			if event.Span == nil || event.Span.SpanID == "" || event.Span.TraceID == "" || event.Span.ServiceID == "" {
				return errs.New(errs.SchemaInvalidValue, "span_report requires span_id, trace_id and service_id").WithVersion(p.SchemaVersion).AtPath(path + ".span")
			}
			if _, ok := serviceIDSet(p)[event.Span.ServiceID]; !ok {
				return errs.New(errs.EdgeServiceMissing, "span references missing service").WithVersion(p.SchemaVersion).AtPath(path + ".span.service_id")
			}
		case model.EventRootSettle:
			if event.Settle == nil || event.Settle.TraceID == "" || event.Settle.RootSpanID == "" {
				return errs.New(errs.SchemaInvalidValue, "root_settle requires trace_id and root_span_id").WithVersion(p.SchemaVersion).AtPath(path + ".settle")
			}
		default:
			return errs.New(errs.SchemaInvalidValue, "unsupported event kind: "+string(event.Kind)).WithVersion(p.SchemaVersion).AtPath(path + ".kind")
		}
	}
	return nil
}

func serviceIDSet(p *model.Project) map[string]struct{} {
	out := map[string]struct{}{}
	for _, service := range p.Services {
		out[service.ID] = struct{}{}
	}
	return out
}
