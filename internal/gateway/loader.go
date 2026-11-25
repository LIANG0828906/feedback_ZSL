package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type versionProbe struct {
	Schema        string `json:"schema"`
	SchemaVersion string `json:"schema_version"`
	APIVersion    string `json:"apiVersion"`
}

func LoadSchema(data []byte) (*Project, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	var probe versionProbe
	if err := dec.Decode(&probe); err != nil {
		return nil, NewError(CodeFieldType, "$", "invalid JSON: %v", err)
	}
	version := strings.ToLower(firstNonEmpty(probe.Schema, probe.SchemaVersion, probe.APIVersion))
	switch {
	case version == string(SchemaV1) || version == "gateway.v1" || version == "1.0":
		return loadV1(data)
	case version == string(SchemaV2) || version == "gateway.v2" || version == "2.0":
		return loadV2(data)
	case version == string(SchemaV3) || version == "gateway.v3" || version == "3.0":
		return loadV3(data)
	default:
		return nil, NewError(CodeSchemaUnsupported, "schema", "unsupported schema version").
			WithContext("schema", version)
	}
}

func rejectDualRouteRepresentations(data []byte, primary string, alternatives ...string) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return NewError(CodeFieldType, "$", "invalid JSON while checking route representations: %v", err)
	}
	primaryValue, primaryExists := raw[primary]
	if !primaryExists || isEmptyJSONArray(primaryValue) {
		return nil
	}
	for _, alternative := range alternatives {
		value, exists := raw[alternative]
		if exists && !isEmptyJSONArray(value) {
			return NewError(CodeFieldAmbiguous, alternative, "both flat and nested route representations are present").
				WithContext("primary", primary).WithContext("conflicting_field", alternative)
		}
	}
	return nil
}

func isEmptyJSONArray(raw json.RawMessage) bool {
	text := strings.TrimSpace(string(raw))
	return text == "" || text == "null" || text == "[]"
}

func decodeUseNumber(data []byte, target any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(target)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

type dtoBudget struct {
	Workers          int `json:"workers"`
	ServiceBuffer    int `json:"service_buffer"`
	MaxWorkers       int `json:"max_workers"`
	MaxServiceBuffer int `json:"max_service_buffer"`
}

type dtoPriority struct {
	Priority   int    `json:"priority"`
	Level      string `json:"level"`
	ServiceID  string `json:"service_id"`
	RouteID    string `json:"route_id"`
	StatusCode int    `json:"status_code"`
}

type dtoExport struct {
	MaxWindows int           `json:"max_windows_per_batch"`
	MaxRecords int           `json:"max_records_per_batch"`
	Priorities []dtoPriority `json:"priority_rules"`
}

type dtoErrorRule struct {
	Class       string `json:"class"`
	StatusMin   int    `json:"status_min"`
	StatusMax   int    `json:"status_max"`
	TimeoutOnly bool   `json:"timeout_only"`
}

func convertPriority(in []dtoPriority) []PriorityRule {
	out := make([]PriorityRule, 0, len(in))
	for _, rule := range in {
		out = append(out, PriorityRule(rule))
	}
	return out
}

func convertErrorRules(in []dtoErrorRule) []ErrorRule {
	out := make([]ErrorRule, 0, len(in))
	for _, rule := range in {
		out = append(out, ErrorRule(rule))
	}
	return out
}

func parseInt64Any(value any, path string) (int64, error) {
	switch v := value.(type) {
	case nil:
		return 0, nil
	case int64:
		return v, nil
	case json.Number:
		i, err := v.Int64()
		if err == nil {
			return i, nil
		}
		f, ferr := v.Float64()
		if ferr == nil && f == float64(int64(f)) {
			return int64(f), nil
		}
		return 0, NewError(CodeFieldType, path, "expected integer").WithContext("value", v.String())
	case float64:
		if v == float64(int64(v)) {
			return int64(v), nil
		}
		return 0, NewError(CodeFieldType, path, "expected integer").WithContext("value", v)
	case string:
		i, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, NewError(CodeFieldType, path, "expected integer string: %v", err).WithContext("value", v)
		}
		return i, nil
	default:
		return 0, NewError(CodeFieldType, path, "unsupported integer representation").WithContext("type", fmt.Sprintf("%T", value))
	}
}

func durationField(value any, unit, path string) (FixedDuration, string, error) {
	if value == nil {
		return 0, "", nil
	}
	switch unit {
	case "us":
		us, err := parseInt64Any(value, path)
		if err != nil {
			return 0, unit, err
		}
		if us < 0 {
			return 0, unit, NewError(CodeDurationNegative, path, "negative duration").WithContext("value", us)
		}
		fixed, err := USSmallToFixed(us)
		return fixed, fmt.Sprint(value), err
	case "ms":
		whole, frac, raw, err := parseDecimal(value, path)
		if err != nil {
			return 0, unit, err
		}
		fixed, err := DecimalMillisecondsToFixed(whole, frac)
		if err != nil {
			if ge, ok := AsError(err); ok {
				ge.Path = path
			}
			return 0, unit, err
		}
		return fixed, raw, nil
	default:
		return 0, unit, NewError(CodeFieldType, path, "unknown duration unit").WithContext("unit", unit)
	}
}

func parseDecimal(value any, path string) (int64, string, string, error) {
	raw := ""
	switch v := value.(type) {
	case json.Number:
		raw = v.String()
	case string:
		raw = v
	case float64:
		raw = strconv.FormatFloat(v, 'f', -1, 64)
	case int64:
		raw = strconv.FormatInt(v, 10)
	default:
		return 0, "", "", NewError(CodeFieldType, path, "unsupported duration representation").WithContext("type", fmt.Sprintf("%T", value))
	}
	raw = strings.TrimSpace(raw)
	negative := strings.HasPrefix(raw, "-")
	raw = strings.TrimPrefix(raw, "-")
	parts := strings.SplitN(raw, ".", 2)
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, "", raw, NewError(CodeFieldType, path, "invalid duration whole part").WithContext("value", raw)
	}
	if negative {
		whole = -whole
	}
	frac := ""
	if len(parts) == 2 {
		frac = parts[1]
	}
	if negative && whole == 0 {
		// Negative fractional values are rejected by the caller's sign check.
		return -1, frac, "-" + raw, nil
	}
	return whole, frac, raw, nil
}

func sameInt64Pointer(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func checkDurationAmbiguity(usValue, msValue any, path string) (FixedDuration, string, error) {
	hasUS := usValue != nil
	hasMS := msValue != nil
	switch {
	case hasUS && hasMS:
		usFixed, _, err := durationField(usValue, "us", path+".latency_us")
		if err != nil {
			return 0, "", err
		}
		msFixed, _, err := durationField(msValue, "ms", path+".latency_ms")
		if err != nil {
			return 0, "", err
		}
		if usFixed != msFixed {
			return 0, "", NewError(CodeFieldAmbiguous, path, "both microsecond and millisecond durations are present but do not convert equally").
				WithContext("duration_us_fixed", int64(usFixed)).
				WithContext("milliseconds_fixed", int64(msFixed))
		}
		return usFixed, "both-consistent", nil
	case hasUS:
		fixed, raw, err := durationField(usValue, "us", path+".latency_us")
		return fixed, raw, err
	case hasMS:
		fixed, raw, err := durationField(msValue, "ms", path+".latency_ms")
		return fixed, raw, err
	default:
		return 0, "", NewError(CodeFieldRequired, path, "duration is required")
	}
}

func resolveEventTime(epochMS any, eventSeq any, origin, step int64, path string) (int64, *int64, error) {
	hasEpoch := epochMS != nil
	hasSeq := eventSeq != nil
	if !hasEpoch && !hasSeq {
		return 0, nil, NewError(CodeFieldRequired, path, "either epoch event time or relative event sequence is required")
	}
	if hasEpoch {
		epoch, err := parseInt64Any(epochMS, path+".event_time_ms")
		if err != nil {
			return 0, nil, err
		}
		if hasSeq {
			seq, err := parseInt64Any(eventSeq, path+".event_seq")
			if err != nil {
				return 0, nil, err
			}
			fromSeq := origin + seq*step
			if fromSeq != epoch {
				return 0, nil, NewError(CodeFieldAmbiguous, path, "epoch time and event sequence disagree").
					WithContext("epoch_ms", epoch).WithContext("sequence_time_ms", fromSeq).WithContext("event_seq", seq)
			}
			seqCopy := seq
			return epoch, &seqCopy, nil
		}
		return epoch, nil, nil
	}
	seq, err := parseInt64Any(eventSeq, path+".event_seq")
	if err != nil {
		return 0, nil, err
	}
	if step <= 0 {
		return 0, nil, NewError(CodeFieldRequired, "sequence_step_ms", "positive sequence step is required for relative event sequence")
	}
	seqCopy := seq
	return origin + seq*step, &seqCopy, nil
}
