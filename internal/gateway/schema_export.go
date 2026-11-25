package gateway

import (
	"encoding/json"
	"fmt"
)

func EncodeSchema(p *Project, version SchemaName) ([]byte, error) {
	switch version {
	case SchemaV1:
		return json.MarshalIndent(toV1DTO(p), "", "  ")
	case SchemaV2:
		return json.MarshalIndent(toV2DTO(p), "", "  ")
	case SchemaV3:
		return json.MarshalIndent(toV3DTO(p), "", "  ")
	default:
		return nil, NewError(CodeSchemaUnsupported, "schema", "cannot encode unsupported version").WithContext("version", version)
	}
}

func toV1DTO(p *Project) dtoV1 {
	routes := make([]dtoRouteV1, 0, len(p.Routes))
	for _, route := range p.Routes {
		routes = append(routes, dtoRouteV1{
			ID: route.ID, ServiceID: route.ServiceID, Path: route.Path, Prefix: route.Prefix,
			Order: route.Order, TimeoutMS: route.TimeoutMS, ErrorRules: toDTOErrorRules(route.ErrorRules),
		})
	}
	records := make([]dtoRecordV1, 0, len(p.Records))
	for _, record := range p.Records {
		records = append(records, dtoRecordV1{
			ID: record.ID, ArrivalSeq: record.ArrivalSeq, Path: record.Path, RouteID: record.RouteID,
			ServiceID: record.ServiceID, EventTimeMS: record.EventTimeMS, EventSeq: record.SourceEventSeq,
			LatencyUS: record.DurationUS, StatusCode: record.StatusCode, RequestBytes: record.RequestBytes,
			ResponseBytes: record.ResponseBytes, TimedOut: record.TimedOut, ErrorClass: record.ErrorClass,
		})
	}
	return dtoV1{
		Schema: string(SchemaV1), ProjectID: p.ProjectID, ProjectVersion: p.ProjectVersion, Seed: p.Seed,
		TimeOriginMS: p.TimeOriginMS, SequenceStepMS: p.SequenceStepMS, Window: p.Window,
		AllowedLatenessMS: p.AllowedLatenessMS, HistogramBoundariesUS: p.HistogramBoundariesUS,
		DurationToleranceUS: p.DurationToleranceUS, CountTolerance: p.CountTolerance,
		Export: dtoExport{MaxWindows: p.Export.MaxWindowsPerBatch, MaxRecords: p.Export.MaxRecordsPerBatch, Priorities: toDTOPriorities(p.Export.PriorityRules)},
		Budget: dtoBudget(p.Budget), Services: p.Services, Routes: routes, Records: records,
		WatermarkMS: p.WatermarkMS, WatermarkMarks: p.WatermarkMarks, CommittedBoundaryMS: p.CommittedBoundaryMS,
		FallbackRule: p.DeclaredFallbackRule, Metadata: p.Metadata,
	}
}

func toV2DTO(p *Project) dtoV2 {
	serviceByID := map[string]dtoServiceV2{}
	order := []string{}
	for _, service := range p.Services {
		serviceByID[service.ID] = dtoServiceV2{ServiceID: service.ID, ServiceName: service.Name}
		order = append(order, service.ID)
	}
	for _, route := range p.Routes {
		service := serviceByID[route.ServiceID]
		service.Routes = append(service.Routes, dtoRouteV2{
			RouteID: route.ID, Path: route.Path, Prefix: route.Prefix, Order: route.Order,
			TimeoutMS: route.TimeoutMS, ErrorRules: toDTOErrorRules(route.ErrorRules),
		})
		serviceByID[route.ServiceID] = service
	}
	services := make([]dtoServiceV2, 0, len(order))
	for _, id := range order {
		services = append(services, serviceByID[id])
	}
	records := make([]dtoRecordV2, 0, len(p.Records))
	for _, record := range p.Records {
		seq := record.SourceEventSeq
		if seq == nil {
			value := (record.EventTimeMS - p.TimeOriginMS) / p.SequenceStepMS
			seq = &value
		}
		records = append(records, dtoRecordV2{
			LogID: record.ID, ArrivalSeq: record.ArrivalSeq, Path: record.Path, RouteID: record.RouteID,
			ServiceID: record.ServiceID, EventSeq: *seq, LatencyMS: msJSONNumber(record.DurationUS),
			Status: record.StatusCode, RequestBytes: record.RequestBytes, ResponseBytes: record.ResponseBytes,
			TimedOut: record.TimedOut, ErrorClass: record.ErrorClass,
		})
	}
	return dtoV2{
		Schema: string(SchemaV2), ProjectID: p.ProjectID, ProjectVersion: p.ProjectVersion, Seed: p.Seed,
		OriginEpochMS: p.TimeOriginMS, SequenceIntervalMS: p.SequenceStepMS,
		Window:            dtoWindowV2{WidthMS: p.Window.WidthMS, SlideMS: p.Window.SlideMS},
		AllowedLatenessMS: p.AllowedLatenessMS, HistogramBoundariesUS: p.HistogramBoundariesUS,
		DurationToleranceUS: p.DurationToleranceUS, CountTolerance: p.CountTolerance,
		Export:   dtoExportV2{MaxWindows: p.Export.MaxWindowsPerBatch, MaxRecords: p.Export.MaxRecordsPerBatch, Priorities: toDTOPriorities(p.Export.PriorityRules)},
		Budget:   dtoBudgetV2{Workers: p.Budget.Workers, ServiceBuffer: p.Budget.ServiceBuffer, MaxWorkers: p.Budget.MaxWorkers, MaxServiceBuffer: p.Budget.MaxServiceBuffer},
		Services: services, Records: records, CommittedBoundaryMS: p.CommittedBoundaryMS,
		FallbackRule: p.DeclaredFallbackRule, Metadata: p.Metadata,
	}
}

func toV3DTO(p *Project) dtoV3 {
	services := make([]dtoServiceRefV3, 0, len(p.Services))
	for _, service := range p.Services {
		services = append(services, dtoServiceRefV3{ID: service.ID, Name: service.Name})
	}
	routes := make([]dtoRouteV3, 0, len(p.Routes))
	for _, route := range p.Routes {
		routes = append(routes, dtoRouteV3{
			ID: route.ID, ServiceRef: route.ServiceID, Path: route.Path, Prefix: route.Prefix,
			Priority: route.Order, DeadlineMS: route.TimeoutMS, ErrorMappings: toDTOErrorRules(route.ErrorRules),
		})
	}
	observations := make([]dtoObservationV3, 0, len(p.Records))
	for _, record := range p.Records {
		obs := dtoObservationV3{ObservationID: record.ID, ArrivalIndex: record.ArrivalSeq}
		obs.Request.Path = record.Path
		obs.Request.Bytes = record.RequestBytes
		obs.Response.StatusCode = record.StatusCode
		obs.Response.Bytes = record.ResponseBytes
		obs.Routing.RouteRef = record.RouteID
		obs.Routing.ServiceRef = record.ServiceID
		obs.Timing.ObservedAtEpochMS = record.EventTimeMS
		obs.Timing.EventSequence = record.SourceEventSeq
		obs.Timing.DurationUS = record.DurationUS
		obs.Timing.TimedOut = record.TimedOut
		obs.Classification.ErrorClass = record.ErrorClass
		observations = append(observations, obs)
	}
	dto := dtoV3{
		APIVersion: string(SchemaV3), Project: dtoProjectV3{ID: p.ProjectID, Revision: p.ProjectVersion},
		Seed: p.Seed, Timeline: dtoTimelineV3{OriginEpochMS: p.TimeOriginMS, EventSequenceStepMS: p.SequenceStepMS},
		Aggregation: dtoAggV3{
			WindowWidthMS: p.Window.WidthMS, WindowSlideMS: p.Window.SlideMS,
			AllowedLatenessMS: p.AllowedLatenessMS, BoundariesUS: p.HistogramBoundariesUS,
			DurationToleranceUS: p.DurationToleranceUS, CountTolerance: p.CountTolerance,
			CommittedBoundaryMS: p.CommittedBoundaryMS, FallbackRule: p.DeclaredFallbackRule,
		},
		ExportPolicy:  dtoExportV2{MaxWindows: p.Export.MaxWindowsPerBatch, MaxRecords: p.Export.MaxRecordsPerBatch, Priorities: toDTOPriorities(p.Export.PriorityRules)},
		RuntimeBudget: dtoBudgetV2{Workers: p.Budget.Workers, ServiceBuffer: p.Budget.ServiceBuffer, MaxWorkers: p.Budget.MaxWorkers, MaxServiceBuffer: p.Budget.MaxServiceBuffer},
		Services:      services, RoutingRules: routes, Observations: observations,
		WatermarkMarks: p.WatermarkMarks, Metadata: p.Metadata,
	}
	if p.WatermarkMS != nil {
		dto.Watermark = &dtoWatermarkV3{ObservedAtEpochMS: *p.WatermarkMS}
	}
	return dto
}

func toDTOErrorRules(rules []ErrorRule) []dtoErrorRule {
	out := make([]dtoErrorRule, 0, len(rules))
	for _, rule := range rules {
		out = append(out, dtoErrorRule(rule))
	}
	return out
}

func toDTOPriorities(rules []PriorityRule) []dtoPriority {
	out := make([]dtoPriority, 0, len(rules))
	for _, rule := range rules {
		out = append(out, dtoPriority(rule))
	}
	return out
}

func msJSONNumber(durationUS int64) json.Number {
	wholeMS := durationUS / 1000
	remainderUS := durationUS % 1000
	if remainderUS == 0 {
		return json.Number(fmt.Sprintf("%d", wholeMS))
	}
	return json.Number(fmt.Sprintf("%d.%03d", wholeMS, remainderUS))
}
