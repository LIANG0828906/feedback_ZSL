package slikernel

import (
	"encoding/json"
)

// EncodeProject serializes a canonical project in one of the three supported
// schemas. Encoding always validates first so the three representations are
// semantically identical.
func EncodeProject(project Project, version int) ([]byte, error) {
	if err := ValidateProject(CloneProject(project)); err != nil {
		return nil, err
	}
	switch version {
	case 1:
		return encodeV1(project)
	case 2:
		return encodeV2(project)
	case 3:
		return encodeV3(project)
	default:
		return nil, biz(CodeSchema, "encode_project", nil, ctx("version", version))
	}
}

func encodeV1(project Project) ([]byte, error) {
	type threshold struct {
		ID                 string `json:"id"`
		BurnPPM            int64  `json:"burn_ppm"`
		ConsecutiveWindows int    `json:"consecutive_windows"`
	}
	type sli struct {
		ID                      string      `json:"id"`
		Kind                    SLIKind     `json:"kind"`
		Route                   string      `json:"route"`
		StatusCodes             []int       `json:"status_codes,omitempty"`
		WindowMs                int64       `json:"window_ms"`
		TargetPPM               int64       `json:"target_ppm"`
		RollingWindows          int         `json:"rolling_windows"`
		QuantilePPM             int64       `json:"quantile_ppm,omitempty"`
		LatencyThresholdUs      int64       `json:"latency_threshold_us,omitempty"`
		ExpectedEventsPerWindow int64       `json:"expected_events_per_window,omitempty"`
		Thresholds              []threshold `json:"thresholds,omitempty"`
		GapPolicy               GapPolicy   `json:"gap_policy"`
	}
	type service struct {
		Name string `json:"name"`
		SLIs []sli  `json:"slis"`
	}
	type observation struct {
		Seq              int64   `json:"seq"`
		Service          string  `json:"service"`
		Route            string  `json:"route"`
		StatusCode       int     `json:"status_code"`
		TimestampMs      int64   `json:"timestamp_ms"`
		Events           int64   `json:"events"`
		Errors           int64   `json:"errors"`
		LatencySamplesUs []int64 `json:"latency_samples_us,omitempty"`
		ThroughputEvents int64   `json:"throughput_events"`
	}
	document := struct {
		SchemaVersion     string        `json:"schema_version"`
		ProjectID         string        `json:"project_id"`
		TimeOriginMs      int64         `json:"time_origin_ms"`
		EvaluationStartMs int64         `json:"evaluation_start_ms"`
		EvaluationEndMs   int64         `json:"evaluation_end_ms"`
		Services          []service     `json:"services"`
		Observations      []observation `json:"observations"`
	}{
		SchemaVersion: "1", ProjectID: project.ProjectID, TimeOriginMs: project.TimeOriginMs,
		EvaluationStartMs: project.EvaluationStartMs, EvaluationEndMs: project.EvaluationEndMs,
	}
	for _, sourceService := range project.Services {
		targetService := service{Name: sourceService.Name}
		for _, sourceSLI := range sourceService.SLIs {
			targetSLI := sli{
				ID: sourceSLI.ID, Kind: sourceSLI.Kind, Route: sourceSLI.Route,
				StatusCodes: sourceSLI.StatusCodes, WindowMs: sourceSLI.WindowMs,
				TargetPPM: sourceSLI.TargetPPM, RollingWindows: sourceSLI.RollingWindows,
				QuantilePPM: sourceSLI.QuantilePPM, LatencyThresholdUs: sourceSLI.LatencyThresholdUs,
				ExpectedEventsPerWindow: sourceSLI.ExpectedEventsPerWindow,
				GapPolicy:               sourceSLI.GapPolicy,
			}
			for _, sourceThreshold := range sourceSLI.Thresholds {
				targetSLI.Thresholds = append(targetSLI.Thresholds, threshold{
					ID: sourceThreshold.ID, BurnPPM: sourceThreshold.BurnPPM,
					ConsecutiveWindows: sourceThreshold.ConsecutiveWindows,
				})
			}
			targetService.SLIs = append(targetService.SLIs, targetSLI)
		}
		document.Services = append(document.Services, targetService)
	}
	for _, source := range project.Observations {
		document.Observations = append(document.Observations, observation{
			Seq: source.Seq, Service: source.Service, Route: source.Route,
			StatusCode: source.StatusCode, TimestampMs: source.TimestampMs,
			Events: source.Events, Errors: source.Errors,
			LatencySamplesUs: source.LatencySamplesUs, ThroughputEvents: source.ThroughputEvents,
		})
	}
	return json.Marshal(document)
}

func encodeV2(project Project) ([]byte, error) {
	ordinalBaseMs := int64(60_000)
	for _, service := range project.Services {
		for _, sli := range service.SLIs {
			if sli.WindowMs < ordinalBaseMs {
				ordinalBaseMs = sli.WindowMs
			}
		}
	}
	if ordinalBaseMs <= 0 || (project.EvaluationStartMs-project.TimeOriginMs)%ordinalBaseMs != 0 {
		return nil, biz(CodeTimeBasis, "encode_project.v2", nil, ctx("ordinal_base_ms", ordinalBaseMs))
	}
	type alert struct {
		ID                 string `json:"threshold_id"`
		BurnBPS            int64  `json:"burn_bps"`
		ConsecutiveWindows int    `json:"consecutive_windows"`
	}
	type sli struct {
		ID                 string    `json:"sli_id"`
		Type               SLIKind   `json:"type"`
		Route              string    `json:"route_pattern"`
		Statuses           []int     `json:"statuses,omitempty"`
		WindowLength       int64     `json:"window_length"`
		WindowUnit         string    `json:"window_unit"`
		TargetBPS          int64     `json:"target_bps"`
		RollingWindowCount int       `json:"rolling_window_count"`
		QuantilePermille   *int64    `json:"quantile_permille,omitempty"`
		LatencyLimit       *float64  `json:"latency_limit,omitempty"`
		LatencyUnit        string    `json:"latency_unit,omitempty"`
		ExpectedEvents     int64     `json:"expected_events_per_window,omitempty"`
		Alerts             []alert   `json:"alerts,omitempty"`
		GapPolicy          GapPolicy `json:"gap_policy"`
	}
	type service struct {
		Name string `json:"service_name"`
		SLIs []sli  `json:"sli_defs"`
	}
	type observation struct {
		Seq              int64   `json:"observation_seq"`
		Service          string  `json:"service_name"`
		Route            string  `json:"route_pattern"`
		StatusCode       int     `json:"status_code"`
		WindowOrdinal    int64   `json:"window_ordinal"`
		OffsetMs         int64   `json:"offset_ms"`
		Events           int64   `json:"event_count"`
		Errors           int64   `json:"error_count"`
		LatencySamplesUs []int64 `json:"latency_samples_us,omitempty"`
		ThroughputEvents int64   `json:"throughput_count"`
	}
	document := struct {
		Schema  string `json:"schema"`
		Project struct {
			Name              string `json:"name"`
			TimeOriginMs      int64  `json:"time_origin_ms"`
			OrdinalBaseMs     int64  `json:"ordinal_base_ms"`
			EvaluationStartMs int64  `json:"evaluation_start_ms"`
			EvaluationEndMs   int64  `json:"evaluation_end_ms"`
		} `json:"project"`
		Services     []service     `json:"services"`
		Observations []observation `json:"observations"`
	}{Schema: "2"}
	document.Project.Name = project.ProjectID
	document.Project.TimeOriginMs = project.TimeOriginMs
	document.Project.OrdinalBaseMs = ordinalBaseMs
	document.Project.EvaluationStartMs = project.EvaluationStartMs
	document.Project.EvaluationEndMs = project.EvaluationEndMs
	for _, sourceService := range project.Services {
		targetService := service{Name: sourceService.Name}
		for _, sourceSLI := range sourceService.SLIs {
			if sourceSLI.TargetPPM%100 != 0 {
				return nil, biz(CodeRatioRange, "encode_project.v2.target", nil,
					ctx2("service", sourceService.Name, "sli", sourceSLI.ID))
			}
			if sourceSLI.WindowMs%1000 != 0 {
				return nil, biz(CodeWindowLength, "encode_project.v2.window", nil,
					ctx2("service", sourceService.Name, "sli", sourceSLI.ID))
			}
			targetSLI := sli{
				ID: sourceSLI.ID, Type: sourceSLI.Kind, Route: sourceSLI.Route,
				Statuses: sourceSLI.StatusCodes, WindowLength: sourceSLI.WindowMs / 1000,
				WindowUnit: "s", TargetBPS: sourceSLI.TargetPPM / 100,
				RollingWindowCount: sourceSLI.RollingWindows,
				ExpectedEvents:     sourceSLI.ExpectedEventsPerWindow, GapPolicy: sourceSLI.GapPolicy,
			}
			if sourceSLI.QuantilePPM != 0 {
				if sourceSLI.QuantilePPM%1000 != 0 {
					return nil, biz(CodeQuantileRange, "encode_project.v2.quantile", nil,
						ctx2("service", sourceService.Name, "sli", sourceSLI.ID))
				}
				quantile := sourceSLI.QuantilePPM / 1000
				targetSLI.QuantilePermille = &quantile
			}
			if sourceSLI.LatencyThresholdUs != 0 {
				if sourceSLI.LatencyThresholdUs%1000 != 0 {
					return nil, biz(CodeRule, "encode_project.v2.latency", nil,
						ctx2("service", sourceService.Name, "sli", sourceSLI.ID))
				}
				latency := float64(sourceSLI.LatencyThresholdUs / 1000)
				targetSLI.LatencyLimit = &latency
				targetSLI.LatencyUnit = "ms"
			}
			for _, sourceThreshold := range sourceSLI.Thresholds {
				if sourceThreshold.BurnPPM%100 != 0 {
					return nil, biz(CodeRatioRange, "encode_project.v2.threshold", nil,
						ctx2("service", sourceService.Name, "threshold", sourceThreshold.ID))
				}
				targetSLI.Alerts = append(targetSLI.Alerts, alert{
					ID: sourceThreshold.ID, BurnBPS: sourceThreshold.BurnPPM / 100,
					ConsecutiveWindows: sourceThreshold.ConsecutiveWindows,
				})
			}
			targetService.SLIs = append(targetService.SLIs, targetSLI)
		}
		document.Services = append(document.Services, targetService)
	}
	for _, source := range project.Observations {
		delta := source.TimestampMs - project.TimeOriginMs
		if delta < 0 || delta%ordinalBaseMs != 0 && delta%ordinalBaseMs >= ordinalBaseMs {
			return nil, biz(CodeTimeBasis, "encode_project.v2.observation", nil, ctx("seq", source.Seq))
		}
		ordinal := delta / ordinalBaseMs
		offset := delta % ordinalBaseMs
		document.Observations = append(document.Observations, observation{
			Seq: source.Seq, Service: source.Service, Route: source.Route,
			StatusCode: source.StatusCode, WindowOrdinal: ordinal, OffsetMs: offset,
			Events: source.Events, Errors: source.Errors,
			LatencySamplesUs: source.LatencySamplesUs, ThroughputEvents: source.ThroughputEvents,
		})
	}
	return json.Marshal(document)
}

func encodeV3(project Project) ([]byte, error) {
	type ratio struct {
		Value float64 `json:"value"`
		Unit  string  `json:"unit"`
	}
	type threshold struct {
		ID                 string `json:"id"`
		Burn               ratio  `json:"burn"`
		ConsecutiveWindows int    `json:"consecutive_windows"`
	}
	type sli struct {
		ID       string  `json:"id"`
		Type     SLIKind `json:"sli_type"`
		Route    string  `json:"route"`
		Statuses []int   `json:"status_codes,omitempty"`
		Window   struct {
			Value int64  `json:"value"`
			Unit  string `json:"unit"`
		} `json:"window"`
		Target         ratio `json:"target"`
		RollingWindows struct {
			Windows int `json:"windows"`
		} `json:"rolling"`
		Quantile *ratio `json:"quantile,omitempty"`
		Latency  struct {
			Value int64  `json:"value"`
			Unit  string `json:"unit"`
		} `json:"latency"`
		ExpectedEvents int64       `json:"expected_throughput,omitempty"`
		Thresholds     []threshold `json:"thresholds,omitempty"`
		GapPolicy      GapPolicy   `json:"gap_policy"`
	}
	type service struct {
		Name string `json:"name"`
		SLIs []sli  `json:"slis"`
	}
	type metrics struct {
		Events           int64   `json:"events"`
		Errors           int64   `json:"errors"`
		LatencySamplesUs []int64 `json:"latency_samples_us,omitempty"`
		ThroughputEvents int64   `json:"throughput_events"`
	}
	type observation struct {
		Seq             int64   `json:"id"`
		Service         string  `json:"service"`
		Route           string  `json:"route"`
		StatusCode      int     `json:"status_code"`
		EpochMs         int64   `json:"at_epoch_ms"`
		Metrics         metrics `json:"metrics"`
		BudgetExhausted bool    `json:"budget_exhausted"`
	}
	document := struct {
		SchemaVersion string `json:"schema_version"`
		ProjectID     string `json:"project_id"`
		Clock         struct {
			OriginEpochMs          int64 `json:"origin_epoch_ms"`
			OrdinalBaseMs          int64 `json:"ordinal_base_ms"`
			EvaluationStartEpochMs int64 `json:"evaluation_start_epoch_ms"`
			EvaluationEndEpochMs   int64 `json:"evaluation_end_epoch_ms"`
		} `json:"clock"`
		Services     []service     `json:"services"`
		Observations []observation `json:"observations"`
	}{SchemaVersion: "3", ProjectID: project.ProjectID}
	document.Clock.OriginEpochMs = project.TimeOriginMs
	document.Clock.OrdinalBaseMs = 60_000
	document.Clock.EvaluationStartEpochMs = project.EvaluationStartMs
	document.Clock.EvaluationEndEpochMs = project.EvaluationEndMs
	for _, sourceService := range project.Services {
		targetService := service{Name: sourceService.Name}
		for _, sourceSLI := range sourceService.SLIs {
			targetSLI := sli{
				ID: sourceSLI.ID, Type: sourceSLI.Kind, Route: sourceSLI.Route,
				Statuses: sourceSLI.StatusCodes, GapPolicy: sourceSLI.GapPolicy,
				Target:         ratio{Value: float64(sourceSLI.TargetPPM) / 10_000, Unit: "percent"},
				ExpectedEvents: sourceSLI.ExpectedEventsPerWindow,
			}
			targetSLI.Window.Value = sourceSLI.WindowMs
			targetSLI.Window.Unit = "ms"
			targetSLI.RollingWindows.Windows = sourceSLI.RollingWindows
			if sourceSLI.QuantilePPM != 0 {
				if sourceSLI.QuantilePPM%100 != 0 {
					return nil, biz(CodeQuantileRange, "encode_project.v3.quantile", nil,
						ctx2("service", sourceService.Name, "sli", sourceSLI.ID))
				}
				targetSLI.Quantile = &ratio{Value: float64(sourceSLI.QuantilePPM) / 100, Unit: "basis_points"}
			}
			if sourceSLI.LatencyThresholdUs != 0 {
				if sourceSLI.LatencyThresholdUs%1000 != 0 {
					return nil, biz(CodeRule, "encode_project.v3.latency", nil,
						ctx2("service", sourceService.Name, "sli", sourceSLI.ID))
				}
				targetSLI.Latency.Value = sourceSLI.LatencyThresholdUs / 1000
				targetSLI.Latency.Unit = "ms"
			}
			for _, sourceThreshold := range sourceSLI.Thresholds {
				targetSLI.Thresholds = append(targetSLI.Thresholds, threshold{
					ID:                 sourceThreshold.ID,
					Burn:               ratio{Value: float64(sourceThreshold.BurnPPM) / 10_000, Unit: "percent"},
					ConsecutiveWindows: sourceThreshold.ConsecutiveWindows,
				})
			}
			targetService.SLIs = append(targetService.SLIs, targetSLI)
		}
		document.Services = append(document.Services, targetService)
	}
	for _, source := range project.Observations {
		document.Observations = append(document.Observations, observation{
			Seq: source.Seq, Service: source.Service, Route: source.Route,
			StatusCode: source.StatusCode, EpochMs: source.TimestampMs,
			Metrics: metrics{
				Events: source.Events, Errors: source.Errors,
				LatencySamplesUs: source.LatencySamplesUs, ThroughputEvents: source.ThroughputEvents,
			},
			BudgetExhausted: false,
		})
	}
	return json.Marshal(document)
}
