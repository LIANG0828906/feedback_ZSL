package loader

import (
	"encoding/json"
	"sort"

	"samplingkernel/internal/errs"
	"samplingkernel/internal/model"
)

func Render(p *model.Project, version string) ([]byte, error) {
	var doc any
	switch version {
	case model.SchemaV1:
		doc = renderV1(p)
	case model.SchemaV2:
		doc = renderV2(p)
	case model.SchemaV3:
		doc = renderV3(p)
	default:
		return nil, errs.New(errs.SchemaUnsupportedVersion, "cannot render unsupported version: "+version)
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, errs.Wrap(errs.SchemaInvalidValue, "render failed", err)
	}
	return data, nil
}

func renderV1(p *model.Project) v1Document {
	doc := v1Document{
		SchemaVersion: model.SchemaV1, ProjectID: p.ProjectID, Seed: p.Seed,
		ClockOriginEpochMS: p.ClockOriginMS, MillisPerSeqStep: p.MillisPerSeqStep,
		BudgetPolicy: v1Budget{WindowMS: p.Budget.WindowMS, GlobalLimitSpans: p.Budget.GlobalLimitSpans, OnGlobalExhausted: p.Budget.GlobalExhaustionRule},
		OrphanPolicy: p.OrphanPolicy,
	}
	for _, svc := range p.Services {
		doc.Services = append(doc.Services, v1Service{ServiceID: svc.ID, DisplayName: svc.Name})
	}
	for _, edge := range p.Edges {
		doc.CallEdges = append(doc.CallEdges, v1Edge{
			EdgeNo: edge.No, EdgeID: edge.ID, FromServiceID: edge.FromServiceID, ToServiceID: edge.ToServiceID,
			BaseRatePermille: edge.BaseRatePermyriad / 10, ForcedRetention: edge.ForcedRetention,
			TailRuleIDs: edge.TailRuleIDs,
		})
	}
	for _, alloc := range p.Allocations {
		doc.ServiceAllocations = append(doc.ServiceAllocations, v1Allocation{ServiceID: alloc.ServiceID, AllocationPermille: alloc.WeightPermyriad / 10})
	}
	for _, rule := range p.TailRules {
		doc.TailRules = append(doc.TailRules, v1TailRule{
			RuleID: rule.ID, Priority: rule.Priority, DeclarationOrder: rule.Order, EdgeID: rule.EdgeID,
			Condition: v1Condition{ConditionKind: rule.Condition.Kind, FieldName: rule.Condition.Field, Operator: rule.Condition.Op, ComparisonValue: rule.Condition.Value, ReferenceRule: rule.Condition.Ref},
			DependsOn: rule.DependsOn,
		})
	}
	for _, event := range p.Events {
		de := v1Event{Sequence: event.Seq, EventKind: string(event.Kind), OccurredAtEpochMS: event.TimestampMS}
		if event.Enter != nil {
			de.Enter = &v1Enter{RequestID: event.Enter.RequestID, TraceID: event.Enter.TraceID, EdgeID: event.Enter.EdgeID}
		}
		if event.Propagate != nil {
			de.Propagate = &v1Propagate{RequestID: event.Propagate.RequestID, TraceID: event.Propagate.TraceID, EdgeID: event.Propagate.EdgeID}
		}
		if event.Settle != nil {
			de.Settle = &v1Settle{TraceID: event.Settle.TraceID, RootSpanID: event.Settle.RootSpanID, ServiceID: event.Settle.ServiceID}
		}
		if event.Span != nil {
			de.Span = &v1Span{
				SpanID: event.Span.SpanID, TraceID: event.Span.TraceID, ParentSpanID: event.Span.ParentSpanID,
				ServiceID: event.Span.ServiceID, EdgeID: event.Span.EdgeID, CausalOrder: event.Span.CausalOrder,
				SpanName: event.Span.Name, Status: event.Span.Status, LatencyMS: event.Span.LatencyMS,
				Tags: event.Span.Tags, ArrivalSeq: event.Span.ArrivalSeq,
			}
		}
		doc.Events = append(doc.Events, de)
	}
	return doc
}

func renderV2(p *model.Project) v2Document {
	doc := v2Document{
		SchemaVersion: model.SchemaV2, ProjectID: p.ProjectID, Seed: p.Seed,
		ClockOriginEpochMs: p.ClockOriginMS, MillisPerSeqStep: p.MillisPerSeqStep,
		BudgetPolicy: v2Budget{WindowMs: p.Budget.WindowMS, GlobalLimitSpans: p.Budget.GlobalLimitSpans, OnGlobalExhausted: p.Budget.GlobalExhaustionRule},
		OrphanPolicy: p.OrphanPolicy,
	}
	for _, svc := range p.Services {
		doc.Services = append(doc.Services, v2Service{ServiceID: svc.ID, DisplayName: svc.Name})
	}
	for _, edge := range p.Edges {
		doc.CallEdges = append(doc.CallEdges, v2Edge{
			EdgeNo: edge.No, EdgeID: edge.ID, FromServiceID: edge.FromServiceID, ToServiceID: edge.ToServiceID,
			BaseRateBp: edge.BaseRatePermyriad, ForceKeep: edge.ForcedRetention,
			TailRuleIDs: edge.TailRuleIDs,
		})
	}
	doc.AllocationTree = renderAllocationTreeV2(p)
	for _, rule := range p.TailRules {
		doc.TailRules = append(doc.TailRules, v2TailRule{
			RuleID: rule.ID, Priority: rule.Priority, DeclarationOrder: rule.Order, EdgeID: rule.EdgeID,
			When:      v2Condition{Kind: rule.Condition.Kind, Field: rule.Condition.Field, Operator: rule.Condition.Op, Value: rule.Condition.Value, ReferenceRule: rule.Condition.Ref},
			DependsOn: rule.DependsOn,
		})
	}
	for _, event := range p.Events {
		de := v2Event{Seq: event.Seq, Kind: string(event.Kind), AtEventOffset: event.Seq - 1}
		if event.Enter != nil {
			de.Enter = &v2Enter{RequestID: event.Enter.RequestID, TraceID: event.Enter.TraceID, EdgeID: event.Enter.EdgeID}
		}
		if event.Propagate != nil {
			de.Propagate = &v2Propagate{RequestID: event.Propagate.RequestID, TraceID: event.Propagate.TraceID, EdgeID: event.Propagate.EdgeID}
		}
		if event.Settle != nil {
			de.Settle = &v2Settle{TraceID: event.Settle.TraceID, RootSpanID: event.Settle.RootSpanID, ServiceID: event.Settle.ServiceID}
		}
		if event.Span != nil {
			de.Span = &v2Span{
				SpanID: event.Span.SpanID, TraceID: event.Span.TraceID, ParentSpanID: event.Span.ParentSpanID,
				ServiceID: event.Span.ServiceID, EdgeID: event.Span.EdgeID, CausalOrder: event.Span.CausalOrder,
				SpanName: event.Span.Name, Status: event.Span.Status, LatencyMS: event.Span.LatencyMS,
				Tags: event.Span.Tags, ArrivalSeq: event.Span.ArrivalSeq,
			}
		}
		doc.Events = append(doc.Events, de)
	}
	return doc
}

func renderAllocationTreeV2(p *model.Project) []v2AllocationNode {
	weight := map[string]uint32{}
	for _, alloc := range p.Allocations {
		weight[alloc.ServiceID] = alloc.WeightPermyriad
	}
	children := map[string][]model.Edge{}
	hasIncoming := map[string]bool{}
	for _, edge := range p.Edges {
		seenChild := false
		for _, existing := range children[edge.FromServiceID] {
			if existing.ToServiceID == edge.ToServiceID {
				seenChild = true
				break
			}
		}
		if !seenChild {
			children[edge.FromServiceID] = append(children[edge.FromServiceID], edge)
		}
		hasIncoming[edge.ToServiceID] = true
	}
	for id := range children {
		sort.Slice(children[id], func(i, j int) bool { return children[id][i].No < children[id][j].No })
	}
	var roots []string
	for _, svc := range p.Services {
		if !hasIncoming[svc.ID] {
			roots = append(roots, svc.ID)
		}
	}
	sort.Strings(roots)
	var build func(serviceID string, seen map[string]bool) v2AllocationNode
	build = func(serviceID string, seen map[string]bool) v2AllocationNode {
		node := v2AllocationNode{ServiceID: serviceID, WeightBp: weight[serviceID]}
		if seen[serviceID] {
			return node
		}
		seen[serviceID] = true
		for _, edge := range children[serviceID] {
			node.Children = append(node.Children, build(edge.ToServiceID, seen))
		}
		return node
	}
	var out []v2AllocationNode
	for _, root := range roots {
		out = append(out, build(root, map[string]bool{}))
	}
	return out
}

func renderV3(p *model.Project) v3Document {
	doc := v3Document{
		SchemaVersion: model.SchemaV3, ProjectID: p.ProjectID, Seed: p.Seed,
		ClockOriginEpochMS: p.ClockOriginMS, MillisPerSeqStep: p.MillisPerSeqStep,
		BudgetPolicy: v3Budget{WindowMS: p.Budget.WindowMS, GlobalLimitSpans: p.Budget.GlobalLimitSpans, OnGlobalExhausted: p.Budget.GlobalExhaustionRule},
		OrphanPolicy: p.OrphanPolicy,
	}
	for _, svc := range p.Services {
		doc.Services = append(doc.Services, v3Service{ID: svc.ID, Name: svc.Name})
	}
	for _, edge := range p.Edges {
		doc.Edges = append(doc.Edges, v3Edge{
			No: edge.No, ID: edge.ID, From: edge.FromServiceID, To: edge.ToServiceID,
			BaseRatePermille: edge.BaseRatePermyriad / 10, Forced: edge.ForcedRetention,
			TailRuleIDs: edge.TailRuleIDs,
		})
	}
	for _, alloc := range p.Allocations {
		doc.ServiceShares = append(doc.ServiceShares, v3Allocation{ServiceID: alloc.ServiceID, SharePermille: alloc.WeightPermyriad / 10})
	}
	for _, rule := range p.TailRules {
		doc.TailRules = append(doc.TailRules, v3TailRule{
			ID: rule.ID, Priority: rule.Priority, Order: rule.Order, EdgeID: rule.EdgeID,
			Condition: v3Condition{Kind: rule.Condition.Kind, Field: rule.Condition.Field, Op: rule.Condition.Op, Value: rule.Condition.Value, Ref: rule.Condition.Ref},
			DependsOn: rule.DependsOn,
		})
	}
	for _, event := range p.Events {
		de := v3Event{Sequence: event.Seq, Type: string(event.Kind), EpochMS: event.TimestampMS}
		if event.Enter != nil {
			de.Enter = &v3Enter{RequestID: event.Enter.RequestID, TraceID: event.Enter.TraceID, EdgeID: event.Enter.EdgeID}
		}
		if event.Propagate != nil {
			de.Propagate = &v3Propagate{RequestID: event.Propagate.RequestID, TraceID: event.Propagate.TraceID, EdgeID: event.Propagate.EdgeID}
		}
		if event.Settle != nil {
			de.Settle = &v3Settle{TraceID: event.Settle.TraceID, RootSpanID: event.Settle.RootSpanID, ServiceID: event.Settle.ServiceID}
		}
		if event.Span != nil {
			de.Span = &v3Span{
				SpanID: event.Span.SpanID, TraceID: event.Span.TraceID, ParentSpanID: event.Span.ParentSpanID,
				ServiceID: event.Span.ServiceID, EdgeID: event.Span.EdgeID, CausalOrder: event.Span.CausalOrder,
				SpanName: event.Span.Name, Status: event.Span.Status, LatencyMS: event.Span.LatencyMS,
				Tags: event.Span.Tags, ArrivalSeq: event.Span.ArrivalSeq,
			}
		}
		doc.Events = append(doc.Events, de)
	}
	return doc
}
