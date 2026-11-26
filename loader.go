package slikernel

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// LoadProject detects one of the three supported source schemas and returns a
// canonical deep copy. The input byte slice is never modified.
func LoadProject(data []byte) (Project, error) {
	version, err := sniffSchemaVersion(data)
	if err != nil {
		return Project{}, err
	}
	var project Project
	switch version {
	case 1:
		project, err = loadV1(data)
	case 2:
		project, err = loadV2(data)
	case 3:
		project, err = loadV3(data)
	default:
		return Project{}, biz(CodeSchema, "load_project", nil, ctx("version", version))
	}
	if err != nil {
		return Project{}, err
	}
	project.SourceVersion = version
	if err := ValidateProject(project); err != nil {
		return Project{}, err
	}
	return CloneProject(project), nil
}

func sniffSchemaVersion(data []byte) (int, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return 0, decodeError("load_project.sniff_version", err)
	}
	keys := []string{"schema_version", "schema", "version"}
	var found []int
	for _, key := range keys {
		raw, ok := top[key]
		if !ok || len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			continue
		}
		version, err := parseSchemaVersion(raw)
		if err != nil {
			return 0, biz(CodeSchema, "load_project.sniff_version", err, ctx("field", key))
		}
		found = append(found, version)
	}
	if len(found) == 0 {
		return 0, biz(CodeSchema, "load_project.sniff_version", nil, ctx("field", "schema_version"))
	}
	for _, version := range found[1:] {
		if version != found[0] {
			return 0, biz(CodeAmbiguousField, "load_project.sniff_version", nil,
				ctx("field", "schema_version,schema,version"))
		}
	}
	return found[0], nil
}

func parseSchemaVersion(raw json.RawMessage) (int, error) {
	text := strings.TrimSpace(string(raw))
	if len(text) >= 2 && text[0] == '"' {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return 0, err
		}
		value = strings.TrimSpace(strings.ToLower(value))
		value = strings.TrimPrefix(value, "v")
		version, err := strconv.Atoi(value)
		if err != nil {
			return 0, err
		}
		return version, nil
	}
	version, err := strconv.Atoi(text)
	if err != nil {
		return 0, err
	}
	return version, nil
}

func decodeError(op string, err error) error {
	context := map[string]any{}
	var syntax *json.SyntaxError
	if ok := asSyntaxError(err, &syntax); ok {
		context["offset"] = syntax.Offset
	}
	var typeError *json.UnmarshalTypeError
	if ok := asTypeError(err, &typeError); ok {
		context["field"] = typeError.Field
		context["offset"] = typeError.Offset
	}
	return biz(CodeJSON, op, err, context)
}

func asSyntaxError(err error, target **json.SyntaxError) bool {
	for err != nil {
		if typed, ok := err.(*json.SyntaxError); ok {
			*target = typed
			return true
		}
		type unwrapper interface{ Unwrap() error }
		wrapped, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = wrapped.Unwrap()
	}
	return false
}

func asTypeError(err error, target **json.UnmarshalTypeError) bool {
	for err != nil {
		if typed, ok := err.(*json.UnmarshalTypeError); ok {
			*target = typed
			return true
		}
		type unwrapper interface{ Unwrap() error }
		wrapped, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = wrapped.Unwrap()
	}
	return false
}

func durationToMs(value int64, unit string, location map[string]any) (int64, error) {
	if value < 0 {
		return 0, biz(CodeWindowLength, "convert_duration", nil, withField(location, "unit", unit))
	}
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "", "ms", "millisecond", "milliseconds":
		return value, nil
	case "s", "sec", "second", "seconds":
		if value > math.MaxInt64/1000 {
			return 0, biz(CodeWindowLength, "convert_duration", nil, withField(location, "unit", unit))
		}
		return value * 1000, nil
	case "us", "microsecond", "microseconds":
		if value%1000 != 0 {
			return 0, biz(CodeUnit, "convert_duration", nil, withField(location, "unit", unit))
		}
		return value / 1000, nil
	default:
		return 0, biz(CodeUnit, "convert_duration", nil, withField(location, "unit", unit))
	}
}

func latencyToUs(value int64, unit string, location map[string]any) (int64, error) {
	if value < 0 {
		return 0, biz(CodeRule, "convert_latency", nil, withField(location, "unit", unit))
	}
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "", "us", "microsecond", "microseconds":
		return value, nil
	case "ms", "millisecond", "milliseconds":
		if value > math.MaxInt64/1000 {
			return 0, biz(CodeRule, "convert_latency", nil, withField(location, "unit", unit))
		}
		return value * 1000, nil
	case "s", "sec", "second", "seconds":
		if value > math.MaxInt64/1_000_000 {
			return 0, biz(CodeRule, "convert_latency", nil, withField(location, "unit", unit))
		}
		return value * 1_000_000, nil
	default:
		return 0, biz(CodeUnit, "convert_latency", nil, withField(location, "unit", unit))
	}
}

func ratioToPPM(value float64, unit string, location map[string]any) (int64, error) {
	return ratioToPPMWithMax(value, unit, PPM, location)
}

func burnToPPM(value float64, unit string, location map[string]any) (int64, error) {
	return ratioToPPMWithMax(value, unit, PPM*100, location)
}

func ratioToPPMWithMax(value float64, unit string, maxPPM int64, location map[string]any) (int64, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, biz(CodeRatioRange, "convert_ratio", nil, withField(location, "unit", unit))
	}
	var scaled float64
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "ppm":
		scaled = value
	case "bps", "basis_point", "basis_points", "basis-points":
		scaled = value * 100
	case "percent", "pct", "%":
		scaled = value * 10_000
	case "fraction", "ratio":
		scaled = value * float64(PPM)
	default:
		return 0, biz(CodeUnit, "convert_ratio", nil, withField(location, "unit", unit))
	}
	if scaled < 0 || scaled > float64(maxPPM)+0.5 {
		return 0, biz(CodeRatioRange, "convert_ratio", nil, withField(location, "unit", unit))
	}
	return int64(math.Round(scaled)), nil
}

func quantileToScale(value float64, unit string, location map[string]any) (int64, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, biz(CodeQuantileRange, "convert_quantile", nil, withField(location, "unit", unit))
	}
	var scaled float64
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "ppm":
		scaled = value
	case "permille", "per_mille", "per-mille", "‰":
		scaled = value * 1000
	case "basis_point", "basis_points", "basis-points", "bps", "ten_thousand":
		scaled = value * 100
	case "percent", "pct", "%":
		scaled = value * 10_000
	default:
		return 0, biz(CodeUnit, "convert_quantile", nil, withField(location, "unit", unit))
	}
	if scaled < 0 || scaled > float64(PPM)+0.5 {
		return 0, biz(CodeQuantileRange, "convert_quantile", nil, withField(location, "unit", unit))
	}
	return int64(math.Round(scaled)), nil
}

func withField(location map[string]any, key string, value any) map[string]any {
	copied := make(map[string]any, len(location)+1)
	for existingKey, existingValue := range location {
		copied[existingKey] = existingValue
	}
	copied[key] = value
	return copied
}

// Schema version 1: compact snake_case, epoch milliseconds, ppm ratios,
// ppm quantiles and millisecond durations.

type rawV1Project struct {
	ProjectID         string             `json:"project_id"`
	TimeOriginMs      int64              `json:"time_origin_ms"`
	EvaluationStartMs int64              `json:"evaluation_start_ms"`
	EvaluationEndMs   int64              `json:"evaluation_end_ms"`
	Services          []rawV1Service     `json:"services"`
	Observations      []rawV1Observation `json:"observations"`
}

type rawV1Service struct {
	Name string     `json:"name"`
	SLIs []rawV1SLI `json:"slis"`
}

type rawV1SLI struct {
	ID                      string           `json:"id"`
	Kind                    string           `json:"kind"`
	Route                   string           `json:"route"`
	StatusCodes             []int            `json:"status_codes"`
	WindowMs                int64            `json:"window_ms"`
	TargetPPM               int64            `json:"target_ppm"`
	RollingWindows          int              `json:"rolling_windows"`
	QuantilePPM             int64            `json:"quantile_ppm"`
	LatencyThresholdUs      int64            `json:"latency_threshold_us"`
	ExpectedEventsPerWindow int64            `json:"expected_events_per_window"`
	Thresholds              []rawV1Threshold `json:"thresholds"`
	GapPolicy               string           `json:"gap_policy"`
	BudgetExhausted         bool             `json:"budget_exhausted"`
}

type rawV1Threshold struct {
	ID                 string `json:"id"`
	BurnPPM            int64  `json:"burn_ppm"`
	ConsecutiveWindows int    `json:"consecutive_windows"`
}

type rawV1Observation struct {
	Seq              int64   `json:"seq"`
	Service          string  `json:"service"`
	Route            string  `json:"route"`
	StatusCode       int     `json:"status_code"`
	TimestampMs      int64   `json:"timestamp_ms"`
	Events           int64   `json:"events"`
	Errors           int64   `json:"errors"`
	LatencySamplesUs []int64 `json:"latency_samples_us"`
	ThroughputEvents int64   `json:"throughput_events"`
	BudgetExhausted  bool    `json:"budget_exhausted"`
}

func loadV1(data []byte) (Project, error) {
	var raw rawV1Project
	if err := json.Unmarshal(data, &raw); err != nil {
		return Project{}, decodeError("load_project.v1", err)
	}
	project := Project{
		ProjectID: raw.ProjectID, TimeOriginMs: raw.TimeOriginMs,
		EvaluationStartMs: raw.EvaluationStartMs, EvaluationEndMs: raw.EvaluationEndMs,
	}
	project.Services = make([]Service, len(raw.Services))
	for serviceIndex, rawService := range raw.Services {
		service := Service{Name: rawService.Name, SLIs: make([]SLI, len(rawService.SLIs))}
		for sliIndex, rawSLI := range rawService.SLIs {
			location := map[string]any{"service": rawService.Name, "sli": rawSLI.ID, "service_index": serviceIndex, "sli_index": sliIndex}
			thresholds, err := convertV1Thresholds(rawSLI.Thresholds, location)
			if err != nil {
				return Project{}, err
			}
			gapPolicy := GapPolicy(rawSLI.GapPolicy)
			if gapPolicy == "" {
				gapPolicy = GapIncludeEmpty
			}
			service.SLIs[sliIndex] = SLI{
				ID: rawSLI.ID, Kind: SLIKind(rawSLI.Kind), Route: rawSLI.Route,
				StatusCodes: NormalizeStatusCodes(rawSLI.StatusCodes), WindowMs: rawSLI.WindowMs,
				TargetPPM: rawSLI.TargetPPM, RollingWindows: rawSLI.RollingWindows,
				QuantilePPM: rawSLI.QuantilePPM, LatencyThresholdUs: rawSLI.LatencyThresholdUs,
				ExpectedEventsPerWindow: rawSLI.ExpectedEventsPerWindow,
				Thresholds:              thresholds, GapPolicy: gapPolicy,
			}
		}
		project.Services[serviceIndex] = service
	}
	project.Observations = make([]Observation, len(raw.Observations))
	for index, rawObservation := range raw.Observations {
		project.Observations[index] = Observation{
			Seq: rawObservation.Seq, Service: rawObservation.Service, Route: rawObservation.Route,
			StatusCode: rawObservation.StatusCode, TimestampMs: rawObservation.TimestampMs,
			Events: rawObservation.Events, Errors: rawObservation.Errors,
			LatencySamplesUs: append([]int64(nil), rawObservation.LatencySamplesUs...),
			ThroughputEvents: rawObservation.ThroughputEvents,
		}
	}
	return project, nil
}

func convertV1Thresholds(raw []rawV1Threshold, location map[string]any) ([]Threshold, error) {
	thresholds := make([]Threshold, len(raw))
	for index, source := range raw {
		if source.BurnPPM < 0 || source.BurnPPM > PPM*100 {
			return nil, biz(CodeRatioRange, "load_project.v1.threshold", nil,
				withField(location, "threshold_index", index))
		}
		thresholds[index] = Threshold{ID: source.ID, BurnPPM: source.BurnPPM, ConsecutiveWindows: source.ConsecutiveWindows}
	}
	return thresholds, nil
}

// Schema version 2: nested project metadata, relative ordinal time base,
// seconds for durations, basis points for ratios and per-mille for quantiles.

type rawV2Project struct {
	Project      rawV2ProjectInfo   `json:"project"`
	Services     []rawV2Service     `json:"services"`
	Observations []rawV2Observation `json:"observations"`
}

type rawV2ProjectInfo struct {
	Name              string `json:"name"`
	TimeOriginMs      int64  `json:"time_origin_ms"`
	OrdinalBaseMs     int64  `json:"ordinal_base_ms"`
	EvaluationStartMs int64  `json:"evaluation_start_ms"`
	EvaluationEndMs   int64  `json:"evaluation_end_ms"`
}

type rawV2Service struct {
	Name string     `json:"service_name"`
	SLIs []rawV2SLI `json:"sli_defs"`
}

type rawV2SLI struct {
	ID                 string           `json:"sli_id"`
	Type               string           `json:"type"`
	Route              string           `json:"route_pattern"`
	Statuses           []int            `json:"statuses"`
	WindowLength       int64            `json:"window_length"`
	WindowUnit         string           `json:"window_unit"`
	TargetPPM          *int64           `json:"target_ppm"`
	TargetBPS          *int64           `json:"target_bps"`
	TargetPercent      *float64         `json:"target_percent"`
	RollingWindowCount int              `json:"rolling_window_count"`
	QuantilePermille   *int64           `json:"quantile_permille"`
	QuantileBasisPoint *int64           `json:"quantile_basis_points"`
	Quantile           *float64         `json:"quantile"`
	QuantileUnit       string           `json:"quantile_unit"`
	LatencyLimit       *float64         `json:"latency_limit"`
	LatencyUnit        string           `json:"latency_unit"`
	ExpectedEvents     int64            `json:"expected_events_per_window"`
	Alerts             []rawV2Threshold `json:"alerts"`
	GapPolicy          string           `json:"gap_policy"`
	BudgetExhausted    bool             `json:"budget_exhausted"`
}

type rawV2Threshold struct {
	ID                 string `json:"threshold_id"`
	BurnPPM            *int64 `json:"burn_ppm"`
	BurnBPS            *int64 `json:"burn_bps"`
	ConsecutiveWindows int    `json:"consecutive_windows"`
}

type rawV2Observation struct {
	Seq              int64   `json:"observation_seq"`
	Service          string  `json:"service_name"`
	Route            string  `json:"route_pattern"`
	StatusCode       int     `json:"status_code"`
	TimestampMs      *int64  `json:"timestamp_ms"`
	WindowOrdinal    *int64  `json:"window_ordinal"`
	OffsetMs         *int64  `json:"offset_ms"`
	Events           int64   `json:"event_count"`
	Errors           int64   `json:"error_count"`
	LatencySamplesUs []int64 `json:"latency_samples_us"`
	ThroughputEvents int64   `json:"throughput_count"`
	BudgetExhausted  bool    `json:"exhausted"`
}

func loadV2(data []byte) (Project, error) {
	var raw rawV2Project
	if err := json.Unmarshal(data, &raw); err != nil {
		return Project{}, decodeError("load_project.v2", err)
	}
	if raw.Project.OrdinalBaseMs < 0 {
		return Project{}, biz(CodeTimeBasis, "load_project.v2", nil, ctx("field", "ordinal_base_ms"))
	}
	project := Project{
		ProjectID: raw.Project.Name, TimeOriginMs: raw.Project.TimeOriginMs,
		EvaluationStartMs: raw.Project.EvaluationStartMs,
		EvaluationEndMs:   raw.Project.EvaluationEndMs,
	}
	project.Services = make([]Service, len(raw.Services))
	for serviceIndex, rawService := range raw.Services {
		service := Service{Name: rawService.Name, SLIs: make([]SLI, len(rawService.SLIs))}
		for sliIndex, rawSLI := range rawService.SLIs {
			location := map[string]any{"service": rawService.Name, "sli": rawSLI.ID, "service_index": serviceIndex, "sli_index": sliIndex}
			target, err := convertV2Target(rawSLI, location)
			if err != nil {
				return Project{}, err
			}
			quantile, err := convertV2Quantile(rawSLI, location)
			if err != nil {
				return Project{}, err
			}
			windowMs, err := durationToMs(rawSLI.WindowLength, rawSLI.WindowUnit, location)
			if err != nil {
				return Project{}, err
			}
			latencyUs, err := convertV2Latency(rawSLI, location)
			if err != nil {
				return Project{}, err
			}
			thresholds, err := convertV2Thresholds(rawSLI.Alerts, location)
			if err != nil {
				return Project{}, err
			}
			gapPolicy := GapPolicy(rawSLI.GapPolicy)
			if gapPolicy == "" {
				gapPolicy = GapIncludeEmpty
			}
			service.SLIs[sliIndex] = SLI{
				ID: rawSLI.ID, Kind: SLIKind(rawSLI.Type), Route: rawSLI.Route,
				StatusCodes: NormalizeStatusCodes(rawSLI.Statuses), WindowMs: windowMs,
				TargetPPM: target, RollingWindows: rawSLI.RollingWindowCount,
				QuantilePPM: quantile, LatencyThresholdUs: latencyUs,
				ExpectedEventsPerWindow: rawSLI.ExpectedEvents,
				Thresholds:              thresholds, GapPolicy: gapPolicy,
			}
		}
		project.Services[serviceIndex] = service
	}
	project.Observations = make([]Observation, len(raw.Observations))
	for index, rawObservation := range raw.Observations {
		timestamp, err := resolveObservationTime(
			rawObservation.TimestampMs, rawObservation.WindowOrdinal, rawObservation.OffsetMs,
			project.TimeOriginMs, raw.Project.OrdinalBaseMs,
			map[string]any{"observation_index": index, "seq": rawObservation.Seq},
		)
		if err != nil {
			return Project{}, err
		}
		project.Observations[index] = Observation{
			Seq: rawObservation.Seq, Service: rawObservation.Service, Route: rawObservation.Route,
			StatusCode: rawObservation.StatusCode, TimestampMs: timestamp,
			Events: rawObservation.Events, Errors: rawObservation.Errors,
			LatencySamplesUs: append([]int64(nil), rawObservation.LatencySamplesUs...),
			ThroughputEvents: rawObservation.ThroughputEvents,
		}
	}
	return project, nil
}

func convertV2Target(raw rawV2SLI, location map[string]any) (int64, error) {
	count := 0
	if raw.TargetPPM != nil {
		count++
	}
	if raw.TargetBPS != nil {
		count++
	}
	if raw.TargetPercent != nil {
		count++
	}
	if count == 0 {
		return 0, biz(CodeInvalidInput, "load_project.v2.target", nil, withField(location, "field", "target"))
	}
	if count > 1 {
		return 0, biz(CodeAmbiguousField, "load_project.v2.target", nil, withField(location, "field", "target"))
	}
	if raw.TargetPPM != nil {
		if *raw.TargetPPM < 0 || *raw.TargetPPM > PPM {
			return 0, biz(CodeRatioRange, "load_project.v2.target", nil, location)
		}
		return *raw.TargetPPM, nil
	}
	if raw.TargetBPS != nil {
		return ratioToPPM(float64(*raw.TargetBPS), "bps", location)
	}
	return ratioToPPM(*raw.TargetPercent, "percent", location)
}

func convertV2Quantile(raw rawV2SLI, location map[string]any) (int64, error) {
	count := 0
	if raw.QuantilePermille != nil {
		count++
	}
	if raw.QuantileBasisPoint != nil {
		count++
	}
	if raw.Quantile != nil {
		count++
	}
	if count == 0 {
		return 0, nil
	}
	if count > 1 {
		return 0, biz(CodeAmbiguousField, "load_project.v2.quantile", nil, withField(location, "field", "quantile"))
	}
	if raw.QuantilePermille != nil {
		return quantileToScale(float64(*raw.QuantilePermille), "permille", location)
	}
	if raw.QuantileBasisPoint != nil {
		return quantileToScale(float64(*raw.QuantileBasisPoint), "basis_points", location)
	}
	return quantileToScale(*raw.Quantile, raw.QuantileUnit, location)
}

func convertV2Latency(raw rawV2SLI, location map[string]any) (int64, error) {
	if raw.LatencyLimit == nil {
		return 0, nil
	}
	if math.IsNaN(*raw.LatencyLimit) || math.IsInf(*raw.LatencyLimit, 0) || *raw.LatencyLimit < 0 {
		return 0, biz(CodeRule, "load_project.v2.latency", nil, location)
	}
	unit := strings.ToLower(strings.TrimSpace(raw.LatencyUnit))
	switch unit {
	case "us", "microsecond", "microseconds":
		return int64(math.Round(*raw.LatencyLimit)), nil
	case "ms", "millisecond", "milliseconds":
		return int64(math.Round(*raw.LatencyLimit * 1000)), nil
	default:
		return 0, biz(CodeUnit, "load_project.v2.latency", nil, withField(location, "unit", raw.LatencyUnit))
	}
}

func convertV2Thresholds(raw []rawV2Threshold, location map[string]any) ([]Threshold, error) {
	thresholds := make([]Threshold, len(raw))
	for index, source := range raw {
		thresholdLocation := withField(location, "threshold_index", index)
		if source.BurnPPM != nil && source.BurnBPS != nil {
			return nil, biz(CodeAmbiguousField, "load_project.v2.threshold", nil, thresholdLocation)
		}
		var burn int64
		switch {
		case source.BurnPPM != nil:
			burn = *source.BurnPPM
		case source.BurnBPS != nil:
			converted, err := burnToPPM(float64(*source.BurnBPS), "bps", thresholdLocation)
			if err != nil {
				return nil, err
			}
			burn = converted
		default:
			return nil, biz(CodeInvalidInput, "load_project.v2.threshold", nil, thresholdLocation)
		}
		if burn < 0 || burn > PPM*100 {
			return nil, biz(CodeRatioRange, "load_project.v2.threshold", nil, thresholdLocation)
		}
		thresholds[index] = Threshold{ID: source.ID, BurnPPM: burn, ConsecutiveWindows: source.ConsecutiveWindows}
	}
	return thresholds, nil
}

func resolveObservationTime(epochMs *int64, ordinal *int64, offsetMs *int64, originMs, ordinalBaseMs int64, location map[string]any) (int64, error) {
	if epochMs != nil && (ordinal != nil || offsetMs != nil) {
		return 0, biz(CodeAmbiguousField, "resolve_observation_time", nil, withField(location, "field", "timestamp"))
	}
	if epochMs != nil {
		return *epochMs, nil
	}
	if ordinal == nil {
		return 0, biz(CodeTimeBasis, "resolve_observation_time", nil, withField(location, "field", "window_ordinal"))
	}
	if ordinalBaseMs <= 0 {
		return 0, biz(CodeTimeBasis, "resolve_observation_time", nil, withField(location, "field", "ordinal_base_ms"))
	}
	if *ordinal < 0 {
		return 0, biz(CodeTimeBasis, "resolve_observation_time", nil, withField(location, "field", "window_ordinal"))
	}
	var offset int64
	if offsetMs != nil {
		offset = *offsetMs
		if offset < 0 || offset >= ordinalBaseMs {
			return 0, biz(CodeTimeBasis, "resolve_observation_time", nil, withField(location, "field", "offset_ms"))
		}
	}
	if *ordinal > (math.MaxInt64-originMs-offset)/ordinalBaseMs {
		return 0, biz(CodeTimeBasis, "resolve_observation_time", nil, withField(location, "field", "window_ordinal"))
	}
	return originMs + *ordinal*ordinalBaseMs + offset, nil
}

// Schema version 3: nested objects, epoch milliseconds, percentage ratios and
// basis-point quantiles. It also carries an optional budget_exhausted marker
// that the loader deliberately discards.

type rawV3Project struct {
	ProjectID    string             `json:"project_id"`
	Clock        rawV3Clock         `json:"clock"`
	Services     []rawV3Service     `json:"services"`
	Observations []rawV3Observation `json:"observations"`
}

type rawV3Clock struct {
	OriginEpochMs          int64 `json:"origin_epoch_ms"`
	OrdinalBaseMs          int64 `json:"ordinal_base_ms"`
	EvaluationStartEpochMs int64 `json:"evaluation_start_epoch_ms"`
	EvaluationEndEpochMs   int64 `json:"evaluation_end_epoch_ms"`
}

type rawV3Service struct {
	Name string     `json:"name"`
	SLIs []rawV3SLI `json:"slis"`
}

type rawV3SLI struct {
	ID              string           `json:"id"`
	Type            string           `json:"sli_type"`
	Route           string           `json:"route"`
	Statuses        []int            `json:"status_codes"`
	Window          rawV3Window      `json:"window"`
	Target          rawV3Ratio       `json:"target"`
	RollingWindows  rawV3Rolling     `json:"rolling"`
	Quantile        rawV3Ratio       `json:"quantile"`
	Latency         rawV3Latency     `json:"latency"`
	ExpectedEvents  int64            `json:"expected_throughput"`
	Thresholds      []rawV3Threshold `json:"thresholds"`
	GapPolicy       string           `json:"gap_policy"`
	BudgetExhausted bool             `json:"budget_exhausted"`
}

type rawV3Window struct {
	Value int64  `json:"value"`
	Unit  string `json:"unit"`
}

type rawV3Ratio struct {
	Value *float64 `json:"value"`
	Unit  string   `json:"unit"`
}

type rawV3Rolling struct {
	Windows int `json:"windows"`
}

type rawV3Latency struct {
	Value int64  `json:"value"`
	Unit  string `json:"unit"`
}

type rawV3Threshold struct {
	ID                 string     `json:"id"`
	Burn               rawV3Ratio `json:"burn"`
	ConsecutiveWindows int        `json:"consecutive_windows"`
}

type rawV3Observation struct {
	Seq             int64        `json:"id"`
	Service         string       `json:"service"`
	Route           string       `json:"route"`
	StatusCode      int          `json:"status_code"`
	EpochMs         *int64       `json:"at_epoch_ms"`
	WindowOrdinal   *int64       `json:"window_ordinal"`
	OffsetMs        *int64       `json:"offset_ms"`
	Metrics         rawV3Metrics `json:"metrics"`
	BudgetExhausted *bool        `json:"budget_exhausted"`
}

type rawV3Metrics struct {
	Events           int64   `json:"events"`
	Errors           int64   `json:"errors"`
	LatencySamplesUs []int64 `json:"latency_samples_us"`
	LatencySamplesMs []int64 `json:"latency_samples_ms"`
	ThroughputEvents int64   `json:"throughput_events"`
}

func loadV3(data []byte) (Project, error) {
	var raw rawV3Project
	if err := json.Unmarshal(data, &raw); err != nil {
		return Project{}, decodeError("load_project.v3", err)
	}
	project := Project{
		ProjectID: raw.ProjectID, TimeOriginMs: raw.Clock.OriginEpochMs,
		EvaluationStartMs: raw.Clock.EvaluationStartEpochMs,
		EvaluationEndMs:   raw.Clock.EvaluationEndEpochMs,
	}
	project.Services = make([]Service, len(raw.Services))
	for serviceIndex, rawService := range raw.Services {
		service := Service{Name: rawService.Name, SLIs: make([]SLI, len(rawService.SLIs))}
		for sliIndex, rawSLI := range rawService.SLIs {
			location := map[string]any{"service": rawService.Name, "sli": rawSLI.ID, "service_index": serviceIndex, "sli_index": sliIndex}
			if rawSLI.Target.Value == nil {
				return Project{}, biz(CodeInvalidInput, "load_project.v3.target", nil, withField(location, "field", "target"))
			}
			target, err := ratioToPPM(*rawSLI.Target.Value, rawSLI.Target.Unit, location)
			if err != nil {
				return Project{}, err
			}
			windowMs, err := durationToMs(rawSLI.Window.Value, rawSLI.Window.Unit, location)
			if err != nil {
				return Project{}, err
			}
			var quantile int64
			if rawSLI.Quantile.Value != nil {
				quantile, err = quantileToScale(*rawSLI.Quantile.Value, rawSLI.Quantile.Unit, location)
				if err != nil {
					return Project{}, err
				}
			}
			latencyUs, err := latencyToUs(rawSLI.Latency.Value, rawSLI.Latency.Unit, location)
			if err != nil {
				return Project{}, err
			}
			thresholds, err := convertV3Thresholds(rawSLI.Thresholds, location)
			if err != nil {
				return Project{}, err
			}
			gapPolicy := GapPolicy(rawSLI.GapPolicy)
			if gapPolicy == "" {
				gapPolicy = GapIncludeEmpty
			}
			service.SLIs[sliIndex] = SLI{
				ID: rawSLI.ID, Kind: SLIKind(rawSLI.Type), Route: rawSLI.Route,
				StatusCodes: NormalizeStatusCodes(rawSLI.Statuses), WindowMs: windowMs,
				TargetPPM: target, RollingWindows: rawSLI.RollingWindows.Windows,
				QuantilePPM: quantile, LatencyThresholdUs: latencyUs,
				ExpectedEventsPerWindow: rawSLI.ExpectedEvents,
				Thresholds:              thresholds, GapPolicy: gapPolicy,
			}
		}
		project.Services[serviceIndex] = service
	}
	project.Observations = make([]Observation, len(raw.Observations))
	for index, rawObservation := range raw.Observations {
		timestamp, err := resolveObservationTime(
			rawObservation.EpochMs, rawObservation.WindowOrdinal, rawObservation.OffsetMs,
			project.TimeOriginMs, raw.Clock.OrdinalBaseMs,
			map[string]any{"observation_index": index, "seq": rawObservation.Seq},
		)
		if err != nil {
			return Project{}, err
		}
		if len(rawObservation.Metrics.LatencySamplesUs) > 0 && len(rawObservation.Metrics.LatencySamplesMs) > 0 {
			return Project{}, biz(CodeAmbiguousField, "load_project.v3.metrics", nil,
				ctx2("observation_index", index, "field", "latency_samples"))
		}
		latencies := append([]int64(nil), rawObservation.Metrics.LatencySamplesUs...)
		if len(rawObservation.Metrics.LatencySamplesMs) > 0 {
			latencies = make([]int64, len(rawObservation.Metrics.LatencySamplesMs))
			for latencyIndex, latencyMs := range rawObservation.Metrics.LatencySamplesMs {
				latencies[latencyIndex] = latencyMs * 1000
			}
		}
		project.Observations[index] = Observation{
			Seq: rawObservation.Seq, Service: rawObservation.Service, Route: rawObservation.Route,
			StatusCode: rawObservation.StatusCode, TimestampMs: timestamp,
			Events: rawObservation.Metrics.Events, Errors: rawObservation.Metrics.Errors,
			LatencySamplesUs: latencies, ThroughputEvents: rawObservation.Metrics.ThroughputEvents,
		}
	}
	return project, nil
}

func convertV3Thresholds(raw []rawV3Threshold, location map[string]any) ([]Threshold, error) {
	thresholds := make([]Threshold, len(raw))
	for index, source := range raw {
		thresholdLocation := withField(location, "threshold_index", index)
		if source.Burn.Value == nil {
			return nil, biz(CodeInvalidInput, "load_project.v3.threshold", nil, thresholdLocation)
		}
		burn, err := burnToPPM(*source.Burn.Value, source.Burn.Unit, thresholdLocation)
		if err != nil {
			return nil, err
		}
		thresholds[index] = Threshold{ID: source.ID, BurnPPM: burn, ConsecutiveWindows: source.ConsecutiveWindows}
	}
	return thresholds, nil
}
