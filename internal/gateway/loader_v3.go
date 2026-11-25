package gateway

type dtoV3 struct {
	APIVersion     string             `json:"apiVersion"`
	Project        dtoProjectV3       `json:"project"`
	Seed           int64              `json:"deterministicSeed"`
	Timeline       dtoTimelineV3      `json:"timeline"`
	Aggregation    dtoAggV3           `json:"aggregation"`
	ExportPolicy   dtoExportV2        `json:"exportPolicy"`
	RuntimeBudget  dtoBudgetV2        `json:"runtimeBudget"`
	Services       []dtoServiceRefV3  `json:"services"`
	RoutingRules   []dtoRouteV3       `json:"routingRules"`
	Observations   []dtoObservationV3 `json:"observations"`
	Watermark      *dtoWatermarkV3    `json:"watermark"`
	WatermarkMarks []WatermarkMark    `json:"watermarkMarks"`
	Metadata       map[string]string  `json:"metadata"`
}

type dtoProjectV3 struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
}

type dtoTimelineV3 struct {
	OriginEpochMS       int64 `json:"originEpochMs"`
	EventSequenceStepMS int64 `json:"eventSequenceStepMs"`
}

type dtoAggV3 struct {
	WindowWidthMS       int64   `json:"windowWidthMs"`
	WindowSlideMS       int64   `json:"windowSlideMs"`
	AllowedLatenessMS   int64   `json:"allowedLatenessMs"`
	BoundariesUS        []int64 `json:"histogramBoundariesMicroseconds"`
	DurationToleranceUS int64   `json:"durationToleranceMicroseconds"`
	CountTolerance      int64   `json:"countTolerance"`
	CommittedBoundaryMS *int64  `json:"committedWindowBoundaryMs"`
	FallbackRule        string  `json:"watermarkFallbackRule"`
}

type dtoServiceRefV3 struct {
	ID   string `json:"id"`
	Name string `json:"displayName"`
}

type dtoRouteV3 struct {
	ID            string         `json:"ruleId"`
	ServiceRef    string         `json:"serviceRef"`
	Path          string         `json:"path"`
	Prefix        string         `json:"matchPrefix"`
	Priority      int            `json:"priority"`
	DeadlineMS    int64          `json:"deadlineMs"`
	ErrorMappings []dtoErrorRule `json:"errorMappings"`
}

type dtoObservationV3 struct {
	ObservationID string `json:"observationId"`
	ArrivalIndex  int    `json:"arrivalIndex"`
	Request       struct {
		Path  string `json:"path"`
		Bytes int64  `json:"bytes"`
	} `json:"httpRequest"`
	Response struct {
		StatusCode int   `json:"statusCode"`
		Bytes      int64 `json:"bytes"`
	} `json:"httpResponse"`
	Routing struct {
		RouteRef   string `json:"routeRef"`
		ServiceRef string `json:"serviceRef"`
	} `json:"routing"`
	Timing struct {
		ObservedAtEpochMS any  `json:"observedAtEpochMs"`
		EventSequence     any  `json:"eventSequence"`
		DurationUS        any  `json:"durationMicroseconds"`
		DurationMS        any  `json:"durationMilliseconds"`
		TimedOut          bool `json:"timedOut"`
	} `json:"timing"`
	Classification struct {
		ErrorClass string `json:"errorClass"`
	} `json:"classification"`
}

type dtoWatermarkV3 struct {
	ObservedAtEpochMS int64 `json:"observedAtEpochMs"`
}

func loadV3(data []byte) (*Project, error) {
	if err := rejectDualRouteRepresentations(data, "routingRules", "route_list", "serviceTree"); err != nil {
		return nil, err
	}
	var dto dtoV3
	if err := decodeUseNumber(data, &dto); err != nil {
		return nil, NewError(CodeFieldType, "$", "invalid v3 JSON: %v", err)
	}
	services := make([]Service, 0, len(dto.Services))
	for _, s := range dto.Services {
		services = append(services, Service{ID: s.ID, Name: s.Name})
	}
	routes := make([]Route, 0, len(dto.RoutingRules))
	for i, r := range dto.RoutingRules {
		order := r.Priority
		if order == 0 {
			order = i
		}
		routes = append(routes, Route{
			ID: r.ID, ServiceID: r.ServiceRef, Path: r.Path, Prefix: r.Prefix, Order: order,
			TimeoutMS: r.DeadlineMS, ErrorRules: convertErrorRules(r.ErrorMappings),
		})
	}
	records := make([]Record, 0, len(dto.Observations))
	for i, r := range dto.Observations {
		path := "observations[" + itoa(i) + "]"
		eventTime, seq, err := resolveEventTime(r.Timing.ObservedAtEpochMS, r.Timing.EventSequence, dto.Timeline.OriginEpochMS, dto.Timeline.EventSequenceStepMS, path+".timing")
		if err != nil {
			return nil, err
		}
		durationFixed, raw, err := checkDurationAmbiguity(r.Timing.DurationUS, r.Timing.DurationMS, path+".timing")
		if err != nil {
			return nil, err
		}
		records = append(records, Record{
			ID: r.ObservationID, ArrivalSeq: r.ArrivalIndex, Path: r.Request.Path,
			RouteID: r.Routing.RouteRef, ServiceID: r.Routing.ServiceRef, EventTimeMS: eventTime,
			SourceEventSeq: seq, DurationUS: int64(durationFixed) / DurationScale,
			DurationFixed: durationFixed, DurationRaw: raw, DurationUnit: "us",
			StatusCode: r.Response.StatusCode, RequestBytes: r.Request.Bytes, ResponseBytes: r.Response.Bytes,
			TimedOut: r.Timing.TimedOut, ErrorClass: r.Classification.ErrorClass,
		})
	}
	var watermark *int64
	source := "missing"
	if dto.Watermark != nil {
		watermark = &dto.Watermark.ObservedAtEpochMS
		source = "explicit"
	}
	if len(dto.WatermarkMarks) > 0 {
		source = "explicit"
	}
	p := &Project{
		ProjectID: dto.Project.ID, ProjectVersion: dto.Project.Revision, Schema: SchemaV3, Seed: dto.Seed,
		TimeOriginMS: dto.Timeline.OriginEpochMS, SequenceStepMS: dto.Timeline.EventSequenceStepMS,
		Window:            WindowConfig{WidthMS: dto.Aggregation.WindowWidthMS, SlideMS: dto.Aggregation.WindowSlideMS},
		AllowedLatenessMS: dto.Aggregation.AllowedLatenessMS, HistogramBoundariesUS: dto.Aggregation.BoundariesUS,
		DurationToleranceUS: dto.Aggregation.DurationToleranceUS, CountTolerance: dto.Aggregation.CountTolerance,
		Export:   ExportConfig{MaxWindowsPerBatch: dto.ExportPolicy.MaxWindows, MaxRecordsPerBatch: dto.ExportPolicy.MaxRecords, PriorityRules: convertPriority(dto.ExportPolicy.Priorities)},
		Budget:   Budget{Workers: dto.RuntimeBudget.Workers, ServiceBuffer: dto.RuntimeBudget.ServiceBuffer, MaxWorkers: dto.RuntimeBudget.MaxWorkers, MaxServiceBuffer: dto.RuntimeBudget.MaxServiceBuffer},
		Services: services, Routes: routes, Records: records, WatermarkMS: watermark, WatermarkMarks: dto.WatermarkMarks,
		CommittedBoundaryMS: dto.Aggregation.CommittedBoundaryMS, DeclaredFallbackRule: dto.Aggregation.FallbackRule,
		Metadata: dto.Metadata, WatermarkSource: source,
	}
	return p, p.Validate()
}
