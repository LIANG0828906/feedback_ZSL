package gateway

type dtoV2 struct {
	Schema                string            `json:"schema"`
	ProjectID             string            `json:"projectId"`
	ProjectVersion        string            `json:"projectVersion"`
	Seed                  int64             `json:"seed"`
	OriginEpochMS         int64             `json:"originEpochMs"`
	SequenceIntervalMS    int64             `json:"sequenceIntervalMs"`
	Window                dtoWindowV2       `json:"windowing"`
	AllowedLatenessMS     int64             `json:"allowedLatenessMs"`
	HistogramBoundariesUS []int64           `json:"histogramBoundariesUs"`
	DurationToleranceUS   int64             `json:"durationToleranceUs"`
	CountTolerance        int64             `json:"countTolerance"`
	Export                dtoExportV2       `json:"exportPolicy"`
	Budget                dtoBudgetV2       `json:"parallelBudget"`
	Services              []dtoServiceV2    `json:"serviceTree"`
	Records               []dtoRecordV2     `json:"accessLogs"`
	CommittedBoundaryMS   *int64            `json:"committedBoundaryMs"`
	FallbackRule          string            `json:"watermarkFallbackRule"`
	Metadata              map[string]string `json:"metadata"`
}

type dtoWindowV2 struct {
	WidthMS int64 `json:"widthMs"`
	SlideMS int64 `json:"slideMs"`
}

type dtoExportV2 struct {
	MaxWindows int           `json:"maxWindowsPerBatch"`
	MaxRecords int           `json:"maxRecordsPerBatch"`
	Priorities []dtoPriority `json:"priorityRules"`
}

type dtoBudgetV2 struct {
	Workers          int `json:"workers"`
	ServiceBuffer    int `json:"perServiceBuffer"`
	MaxWorkers       int `json:"maxWorkers"`
	MaxServiceBuffer int `json:"maxPerServiceBuffer"`
}

type dtoServiceV2 struct {
	ServiceID   string       `json:"serviceId"`
	ServiceName string       `json:"serviceName"`
	Routes      []dtoRouteV2 `json:"routes"`
}

type dtoRouteV2 struct {
	RouteID    string         `json:"routeId"`
	Path       string         `json:"path"`
	Prefix     string         `json:"prefix"`
	Order      int            `json:"order"`
	TimeoutMS  int64          `json:"timeoutMs"`
	ErrorRules []dtoErrorRule `json:"errorRules"`
}

type dtoRecordV2 struct {
	LogID         string `json:"logId"`
	ArrivalSeq    int    `json:"arrivalSeq"`
	Path          string `json:"path"`
	RouteID       string `json:"routeId"`
	ServiceID     string `json:"serviceId"`
	EventEpochMS  any    `json:"eventEpochMs"`
	EventSeq      any    `json:"eventSeq"`
	LatencyMS     any    `json:"latencyMs"`
	LatencyUS     any    `json:"latencyUs"`
	Status        int    `json:"httpStatus"`
	RequestBytes  int64  `json:"reqBytes"`
	ResponseBytes int64  `json:"respBytes"`
	TimedOut      bool   `json:"timedOut"`
	ErrorClass    string `json:"errorClass"`
}

func loadV2(data []byte) (*Project, error) {
	if err := rejectDualRouteRepresentations(data, "serviceTree", "route_list", "routingRules"); err != nil {
		return nil, err
	}
	var dto dtoV2
	if err := decodeUseNumber(data, &dto); err != nil {
		return nil, NewError(CodeFieldType, "$", "invalid v2 JSON: %v", err)
	}
	services := make([]Service, 0, len(dto.Services))
	routes := make([]Route, 0)
	for serviceIndex, service := range dto.Services {
		services = append(services, Service{ID: service.ServiceID, Name: service.ServiceName})
		for routeIndex, route := range service.Routes {
			order := route.Order
			if order == 0 {
				order = routeIndex
			}
			routes = append(routes, Route{
				ID: route.RouteID, ServiceID: service.ServiceID, Path: route.Path, Prefix: route.Prefix,
				Order: order, TimeoutMS: route.TimeoutMS, ErrorRules: convertErrorRules(route.ErrorRules),
			})
		}
		_ = serviceIndex
	}
	if len(routes) == 0 {
		return nil, NewError(CodeFieldRequired, "serviceTree[].routes", "v2 requires nested route tree")
	}

	records := make([]Record, 0, len(dto.Records))
	for i, r := range dto.Records {
		path := "accessLogs[" + itoa(i) + "]"
		eventTime, seq, err := resolveEventTime(r.EventEpochMS, r.EventSeq, dto.OriginEpochMS, dto.SequenceIntervalMS, path)
		if err != nil {
			return nil, err
		}
		durationFixed, raw, err := checkDurationAmbiguity(r.LatencyUS, r.LatencyMS, path)
		if err != nil {
			return nil, err
		}
		records = append(records, Record{
			ID: r.LogID, ArrivalSeq: r.ArrivalSeq, Path: r.Path, RouteID: r.RouteID, ServiceID: r.ServiceID,
			EventTimeMS: eventTime, SourceEventSeq: seq, DurationUS: int64(durationFixed) / DurationScale,
			DurationFixed: durationFixed, DurationRaw: raw,
			DurationUnit: "ms", StatusCode: r.Status, RequestBytes: r.RequestBytes, ResponseBytes: r.ResponseBytes,
			TimedOut: r.TimedOut, ErrorClass: r.ErrorClass,
		})
	}

	p := &Project{
		ProjectID: dto.ProjectID, ProjectVersion: dto.ProjectVersion, Schema: SchemaV2, Seed: dto.Seed,
		TimeOriginMS: dto.OriginEpochMS, SequenceStepMS: dto.SequenceIntervalMS,
		Window:            WindowConfig{WidthMS: dto.Window.WidthMS, SlideMS: dto.Window.SlideMS},
		AllowedLatenessMS: dto.AllowedLatenessMS, HistogramBoundariesUS: dto.HistogramBoundariesUS,
		DurationToleranceUS: dto.DurationToleranceUS, CountTolerance: dto.CountTolerance,
		Export:   ExportConfig{MaxWindowsPerBatch: dto.Export.MaxWindows, MaxRecordsPerBatch: dto.Export.MaxRecords, PriorityRules: convertPriority(dto.Export.Priorities)},
		Budget:   Budget{Workers: dto.Budget.Workers, ServiceBuffer: dto.Budget.ServiceBuffer, MaxWorkers: dto.Budget.MaxWorkers, MaxServiceBuffer: dto.Budget.MaxServiceBuffer},
		Services: services, Routes: routes, Records: records, CommittedBoundaryMS: dto.CommittedBoundaryMS,
		DeclaredFallbackRule: dto.FallbackRule, Metadata: dto.Metadata, WatermarkSource: "missing",
	}
	return p, p.Validate()
}
