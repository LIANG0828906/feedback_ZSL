package gateway

type dtoV1 struct {
	Schema                string            `json:"schema"`
	ProjectID             string            `json:"project_id"`
	ProjectVersion        string            `json:"project_version"`
	Seed                  int64             `json:"seed"`
	TimeOriginMS          int64             `json:"time_origin_ms"`
	SequenceStepMS        int64             `json:"sequence_step_ms"`
	Window                WindowConfig      `json:"window"`
	AllowedLatenessMS     int64             `json:"allowed_lateness_ms"`
	HistogramBoundariesUS []int64           `json:"histogram_boundaries_us"`
	DurationToleranceUS   int64             `json:"duration_tolerance_us"`
	CountTolerance        int64             `json:"count_tolerance"`
	Export                dtoExport         `json:"export"`
	Budget                dtoBudget         `json:"budget"`
	Services              []Service         `json:"services"`
	Routes                []dtoRouteV1      `json:"route_list"`
	Records               []dtoRecordV1     `json:"access_records"`
	WatermarkMS           *int64            `json:"watermark_ms"`
	WatermarkMarks        []WatermarkMark   `json:"watermark_marks"`
	CommittedBoundaryMS   *int64            `json:"committed_window_boundary_ms"`
	FallbackRule          string            `json:"watermark_fallback_rule"`
	Metadata              map[string]string `json:"metadata"`
}

type dtoRouteV1 struct {
	ID         string         `json:"route_id"`
	ServiceID  string         `json:"service_id"`
	Path       string         `json:"path"`
	Prefix     string         `json:"match_prefix"`
	Order      int            `json:"match_order"`
	TimeoutMS  int64          `json:"timeout_ms"`
	ErrorRules []dtoErrorRule `json:"error_classification"`
}

type dtoRecordV1 struct {
	ID            string `json:"record_id"`
	ArrivalSeq    int    `json:"arrival_sequence"`
	Path          string `json:"request_path"`
	RouteID       string `json:"route_id"`
	ServiceID     string `json:"service_id"`
	EventTimeMS   any    `json:"event_time_ms"`
	EventSeq      any    `json:"event_sequence"`
	LatencyUS     any    `json:"latency_us"`
	LatencyMS     any    `json:"latency_ms"`
	StatusCode    int    `json:"status_code"`
	RequestBytes  int64  `json:"request_bytes"`
	ResponseBytes int64  `json:"response_bytes"`
	TimedOut      bool   `json:"timed_out"`
	ErrorClass    string `json:"error_class"`
}

func loadV1(data []byte) (*Project, error) {
	if err := rejectDualRouteRepresentations(data, "route_list", "serviceTree", "routingRules"); err != nil {
		return nil, err
	}
	var dto dtoV1
	if err := decodeUseNumber(data, &dto); err != nil {
		return nil, NewError(CodeFieldType, "$", "invalid v1 JSON: %v", err)
	}
	if len(dto.Routes) == 0 {
		return nil, NewError(CodeFieldRequired, "route_list", "v1 requires flat route_list")
	}
	routes := make([]Route, 0, len(dto.Routes))
	for i, r := range dto.Routes {
		if r.Order == 0 {
			r.Order = i
		}
		routes = append(routes, Route{
			ID: r.ID, ServiceID: r.ServiceID, Path: r.Path, Prefix: r.Prefix, Order: r.Order,
			TimeoutMS: r.TimeoutMS, ErrorRules: convertErrorRules(r.ErrorRules),
		})
	}
	records := make([]Record, 0, len(dto.Records))
	for i, r := range dto.Records {
		path := "access_records[" + itoa(i) + "]"
		eventTime, seq, err := resolveEventTime(r.EventTimeMS, r.EventSeq, dto.TimeOriginMS, dto.SequenceStepMS, path)
		if err != nil {
			return nil, err
		}
		durationFixed, raw, err := checkDurationAmbiguity(r.LatencyUS, r.LatencyMS, path)
		if err != nil {
			return nil, err
		}
		records = append(records, Record{
			ID: r.ID, ArrivalSeq: r.ArrivalSeq, Path: r.Path, RouteID: r.RouteID, ServiceID: r.ServiceID,
			EventTimeMS: eventTime, SourceEventSeq: seq, DurationUS: int64(durationFixed) / DurationScale,
			DurationFixed: durationFixed, DurationRaw: raw,
			DurationUnit: "us", StatusCode: r.StatusCode, RequestBytes: r.RequestBytes,
			ResponseBytes: r.ResponseBytes, TimedOut: r.TimedOut, ErrorClass: r.ErrorClass,
		})
	}
	p := &Project{
		ProjectID: dto.ProjectID, ProjectVersion: dto.ProjectVersion, Schema: SchemaV1, Seed: dto.Seed,
		TimeOriginMS: dto.TimeOriginMS, SequenceStepMS: dto.SequenceStepMS, Window: dto.Window,
		AllowedLatenessMS: dto.AllowedLatenessMS, HistogramBoundariesUS: dto.HistogramBoundariesUS,
		DurationToleranceUS: dto.DurationToleranceUS, CountTolerance: dto.CountTolerance,
		Export: ExportConfig{MaxWindowsPerBatch: dto.Export.MaxWindows, MaxRecordsPerBatch: dto.Export.MaxRecords, PriorityRules: convertPriority(dto.Export.Priorities)},
		Budget: Budget(dto.Budget), Services: dto.Services, Routes: routes, Records: records,
		WatermarkMS: dto.WatermarkMS, WatermarkMarks: dto.WatermarkMarks, CommittedBoundaryMS: dto.CommittedBoundaryMS,
		DeclaredFallbackRule: dto.FallbackRule, Metadata: dto.Metadata,
	}
	if dto.WatermarkMS != nil || len(dto.WatermarkMarks) > 0 {
		p.WatermarkSource = "explicit"
	}
	return p, p.Validate()
}
