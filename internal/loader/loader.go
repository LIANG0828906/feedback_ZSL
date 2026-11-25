package loader

import (
	"encoding/json"

	"samplingkernel/internal/errs"
	"samplingkernel/internal/model"
	"samplingkernel/internal/projectvalidate"
)

func Load(data []byte) (*model.Project, error) {
	var envelope map[string]any
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, errs.Wrap(errs.SchemaInvalidValue, "invalid JSON envelope", err).AtPath("$")
	}
	version, _ := envelope["schema_version"].(string)
	if version == "" {
		version, _ = envelope["schemaVersion"].(string)
	}
	if version == "" {
		version, _ = envelope["SchemaVersion"].(string)
	}
	switch version {
	case model.SchemaV1:
		return LoadVersion(model.SchemaV1, data)
	case model.SchemaV2:
		return LoadVersion(model.SchemaV2, data)
	case model.SchemaV3:
		return LoadVersion(model.SchemaV3, data)
	default:
		return nil, errs.New(errs.SchemaUnsupportedVersion, "unsupported schema version").
			WithVersion(version).AtPath("$.schema_version").
			WithDetail("supported=" + model.SchemaV1 + "," + model.SchemaV2 + "," + model.SchemaV3)
	}
}

func LoadVersion(version string, data []byte) (*model.Project, error) {
	var project *model.Project
	var err error
	switch version {
	case model.SchemaV1:
		var doc v1Document
		if err = unmarshalStrict(data, &doc, version); err != nil {
			return nil, err
		}
		project = convertV1(&doc)
	case model.SchemaV2:
		var doc v2Document
		if err = unmarshalStrict(data, &doc, version); err != nil {
			return nil, err
		}
		project, err = convertV2(&doc)
	case model.SchemaV3:
		var doc v3Document
		if err = unmarshalStrict(data, &doc, version); err != nil {
			return nil, err
		}
		project = convertV3(&doc)
	default:
		return nil, errs.New(errs.SchemaUnsupportedVersion, "unsupported schema version: "+version).
			WithVersion(version).AtPath("$.schema_version")
	}
	if err != nil {
		return nil, err
	}
	if err := projectvalidate.Validate(project); err != nil {
		return nil, err
	}
	return project, nil
}

func baseProject(version, id string, seed int64, origin, step int64, budget model.BudgetPolicy, orphan string) model.Project {
	return model.Project{
		SchemaVersion:    version,
		ProjectID:        id,
		Seed:             seed,
		ClockOriginMS:    origin,
		MillisPerSeqStep: step,
		Budget:           budget,
		OrphanPolicy:     orphan,
	}
}

func convertV1(doc *v1Document) *model.Project {
	p := baseProject(model.SchemaV1, doc.ProjectID, doc.Seed, doc.ClockOriginEpochMS, doc.MillisPerSeqStep, model.BudgetPolicy{
		WindowMS:             doc.BudgetPolicy.WindowMS,
		GlobalLimitSpans:     doc.BudgetPolicy.GlobalLimitSpans,
		GlobalExhaustionRule: doc.BudgetPolicy.OnGlobalExhausted,
	}, doc.OrphanPolicy)
	for _, service := range doc.Services {
		p.Services = append(p.Services, model.Service{ID: service.ServiceID, Name: service.DisplayName})
	}
	for _, edge := range doc.CallEdges {
		p.Edges = append(p.Edges, model.Edge{
			No:                edge.EdgeNo,
			ID:                edge.EdgeID,
			FromServiceID:     edge.FromServiceID,
			ToServiceID:       edge.ToServiceID,
			BaseRatePermyriad: edge.BaseRatePermille * 10,
			ForcedRetention:   edge.ForcedRetention,
			TailRuleIDs:       edge.TailRuleIDs,
		})
	}
	for _, allocation := range doc.ServiceAllocations {
		p.Allocations = append(p.Allocations, model.Allocation{ServiceID: allocation.ServiceID, WeightPermyriad: allocation.AllocationPermille * 10})
	}
	for _, rule := range doc.TailRules {
		p.TailRules = append(p.TailRules, model.TailRule{
			ID: rule.RuleID, Priority: rule.Priority, Order: rule.DeclarationOrder, EdgeID: rule.EdgeID,
			Condition: model.TailCondition{
				Kind: rule.Condition.ConditionKind, Field: rule.Condition.FieldName,
				Op: rule.Condition.Operator, Value: rule.Condition.ComparisonValue, Ref: rule.Condition.ReferenceRule,
			},
			DependsOn: rule.DependsOn,
		})
	}
	for i, event := range doc.Events {
		ce := model.Event{Seq: event.Sequence, Kind: model.EventKind(event.EventKind), TimestampMS: event.OccurredAtEpochMS}
		if ce.Seq == 0 {
			ce.Seq = i + 1
		}
		if event.Enter != nil {
			ce.Enter = &model.EnterPayload{RequestID: event.Enter.RequestID, TraceID: event.Enter.TraceID, EdgeID: event.Enter.EdgeID}
		}
		if event.Propagate != nil {
			ce.Propagate = &model.EdgePayload{RequestID: event.Propagate.RequestID, TraceID: event.Propagate.TraceID, EdgeID: event.Propagate.EdgeID}
		}
		if event.Settle != nil {
			ce.Settle = &model.SettlePayload{TraceID: event.Settle.TraceID, RootSpanID: event.Settle.RootSpanID, ServiceID: event.Settle.ServiceID}
		}
		if event.Span != nil {
			ce.Span = convertV1Span(event.Span)
		}
		p.Events = append(p.Events, ce)
	}
	return &p
}

func convertV1Span(sp *v1Span) *model.Span {
	return &model.Span{
		SpanID: sp.SpanID, TraceID: sp.TraceID, ParentSpanID: sp.ParentSpanID, ServiceID: sp.ServiceID,
		EdgeID: sp.EdgeID, CausalOrder: sp.CausalOrder, Name: sp.SpanName, Status: sp.Status,
		LatencyMS: sp.LatencyMS, Tags: sp.Tags, ArrivalSeq: sp.ArrivalSeq,
	}
}

func convertV2(doc *v2Document) (*model.Project, error) {
	p := baseProject(model.SchemaV2, doc.ProjectID, doc.Seed, doc.ClockOriginEpochMs, doc.MillisPerSeqStep, model.BudgetPolicy{
		WindowMS:             doc.BudgetPolicy.WindowMs,
		GlobalLimitSpans:     doc.BudgetPolicy.GlobalLimitSpans,
		GlobalExhaustionRule: doc.BudgetPolicy.OnGlobalExhausted,
	}, doc.OrphanPolicy)
	for _, service := range doc.Services {
		p.Services = append(p.Services, model.Service{ID: service.ServiceID, Name: service.DisplayName})
	}
	for _, edge := range doc.CallEdges {
		p.Edges = append(p.Edges, model.Edge{
			No: edge.EdgeNo, ID: edge.EdgeID, FromServiceID: edge.FromServiceID, ToServiceID: edge.ToServiceID,
			BaseRatePermyriad: edge.BaseRateBp, ForcedRetention: edge.ForceKeep,
			TailRuleIDs: edge.TailRuleIDs,
		})
	}
	var flattenAllocations func(nodes []v2AllocationNode)
	flattenAllocations = func(nodes []v2AllocationNode) {
		for _, node := range nodes {
			p.Allocations = append(p.Allocations, model.Allocation{ServiceID: node.ServiceID, WeightPermyriad: node.WeightBp})
			flattenAllocations(node.Children)
		}
	}
	flattenAllocations(doc.AllocationTree)
	for _, rule := range doc.TailRules {
		p.TailRules = append(p.TailRules, model.TailRule{
			ID: rule.RuleID, Priority: rule.Priority, Order: rule.DeclarationOrder, EdgeID: rule.EdgeID,
			Condition: model.TailCondition{
				Kind: rule.When.Kind, Field: rule.When.Field, Op: rule.When.Operator,
				Value: rule.When.Value, Ref: rule.When.ReferenceRule,
			},
			DependsOn: rule.DependsOn,
		})
	}
	for i, event := range doc.Events {
		seq := event.Seq
		if seq == 0 {
			seq = i + 1
		}
		timestamp := doc.ClockOriginEpochMs + int64(event.AtEventOffset)*doc.MillisPerSeqStep
		ce := model.Event{Seq: seq, Kind: model.EventKind(event.Kind), TimestampMS: timestamp}
		if event.Enter != nil {
			ce.Enter = &model.EnterPayload{RequestID: event.Enter.RequestID, TraceID: event.Enter.TraceID, EdgeID: event.Enter.EdgeID}
		}
		if event.Propagate != nil {
			ce.Propagate = &model.EdgePayload{RequestID: event.Propagate.RequestID, TraceID: event.Propagate.TraceID, EdgeID: event.Propagate.EdgeID}
		}
		if event.Settle != nil {
			ce.Settle = &model.SettlePayload{TraceID: event.Settle.TraceID, RootSpanID: event.Settle.RootSpanID, ServiceID: event.Settle.ServiceID}
		}
		if event.Span != nil {
			ce.Span = convertV2Span(event.Span)
		}
		p.Events = append(p.Events, ce)
	}
	return &p, nil
}

func convertV2Span(sp *v2Span) *model.Span {
	return &model.Span{
		SpanID: sp.SpanID, TraceID: sp.TraceID, ParentSpanID: sp.ParentSpanID, ServiceID: sp.ServiceID,
		EdgeID: sp.EdgeID, CausalOrder: sp.CausalOrder, Name: sp.SpanName, Status: sp.Status,
		LatencyMS: sp.LatencyMS, Tags: sp.Tags, ArrivalSeq: sp.ArrivalSeq,
	}
}

func convertV3(doc *v3Document) *model.Project {
	p := baseProject(model.SchemaV3, doc.ProjectID, doc.Seed, doc.ClockOriginEpochMS, doc.MillisPerSeqStep, model.BudgetPolicy{
		WindowMS:             doc.BudgetPolicy.WindowMS,
		GlobalLimitSpans:     doc.BudgetPolicy.GlobalLimitSpans,
		GlobalExhaustionRule: doc.BudgetPolicy.OnGlobalExhausted,
	}, doc.OrphanPolicy)
	for _, service := range doc.Services {
		p.Services = append(p.Services, model.Service{ID: service.ID, Name: service.Name})
	}
	for _, edge := range doc.Edges {
		p.Edges = append(p.Edges, model.Edge{
			No: edge.No, ID: edge.ID, FromServiceID: edge.From, ToServiceID: edge.To,
			BaseRatePermyriad: edge.BaseRatePermille * 10, ForcedRetention: edge.Forced,
			TailRuleIDs: edge.TailRuleIDs,
		})
	}
	for _, allocation := range doc.ServiceShares {
		p.Allocations = append(p.Allocations, model.Allocation{ServiceID: allocation.ServiceID, WeightPermyriad: allocation.SharePermille * 10})
	}
	for _, rule := range doc.TailRules {
		p.TailRules = append(p.TailRules, model.TailRule{
			ID: rule.ID, Priority: rule.Priority, Order: rule.Order, EdgeID: rule.EdgeID,
			Condition: model.TailCondition{Kind: rule.Condition.Kind, Field: rule.Condition.Field, Op: rule.Condition.Op, Value: rule.Condition.Value, Ref: rule.Condition.Ref},
			DependsOn: rule.DependsOn,
		})
	}
	for _, event := range doc.Events {
		ce := model.Event{Seq: event.Sequence, Kind: model.EventKind(event.Type), TimestampMS: event.EpochMS}
		if event.Enter != nil {
			ce.Enter = &model.EnterPayload{RequestID: event.Enter.RequestID, TraceID: event.Enter.TraceID, EdgeID: event.Enter.EdgeID}
		}
		if event.Propagate != nil {
			ce.Propagate = &model.EdgePayload{RequestID: event.Propagate.RequestID, TraceID: event.Propagate.TraceID, EdgeID: event.Propagate.EdgeID}
		}
		if event.Settle != nil {
			ce.Settle = &model.SettlePayload{TraceID: event.Settle.TraceID, RootSpanID: event.Settle.RootSpanID, ServiceID: event.Settle.ServiceID}
		}
		if event.Span != nil {
			ce.Span = convertV3Span(event.Span)
		}
		p.Events = append(p.Events, ce)
	}
	return &p
}

func convertV3Span(sp *v3Span) *model.Span {
	return &model.Span{
		SpanID: sp.SpanID, TraceID: sp.TraceID, ParentSpanID: sp.ParentSpanID, ServiceID: sp.ServiceID,
		EdgeID: sp.EdgeID, CausalOrder: sp.CausalOrder, Name: sp.SpanName, Status: sp.Status,
		LatencyMS: sp.LatencyMS, Tags: sp.Tags, ArrivalSeq: sp.ArrivalSeq,
	}
}
