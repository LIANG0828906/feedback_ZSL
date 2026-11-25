package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"

	"featurekernel/internal/kernel"
)

const HourMs int64 = 60 * 60 * 1000

type versionHead struct {
	V1 string `json:"schema_version"`
	V2 string `json:"schemaVersion"`
	V3 string `json:"schema-version"`
}

func Load(data []byte) (*kernel.Project, error) {
	var head versionHead
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, kernel.WrapError(kernel.CodeSchemaInvalidCombination, err, map[string]any{"stage": "detect_version"})
	}

	switch {
	case head.V1 == "v1":
		var dto V1Project
		if err := strictDecode(data, &dto); err != nil {
			return nil, err
		}
		return loadV1(&dto)
	case head.V2 == "v2":
		var dto V2Project
		if err := strictDecode(data, &dto); err != nil {
			return nil, err
		}
		return loadV2(&dto)
	case head.V3 == "v3":
		var dto V3Project
		if err := strictDecode(data, &dto); err != nil {
			return nil, err
		}
		return loadV3(&dto)
	default:
		version := head.V1
		if version == "" {
			version = head.V2
		}
		if version == "" {
			version = head.V3
		}
		return nil, kernel.NewError(kernel.CodeSchemaUnsupportedVersion, "unsupported schema version", map[string]any{
			"detected_version": version,
			"supported":        []string{"v1", "v2", "v3"},
		})
	}
}

func strictDecode(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		code := kernel.CodeSchemaInvalidCombination
		message := err.Error()
		if strings.Contains(message, "unknown field") {
			code = kernel.CodeSchemaUnknownField
		}
		return kernel.WrapError(code, err, map[string]any{"stage": "strict_json_decode"})
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return kernel.NewError(kernel.CodeSchemaInvalidCombination, "multiple JSON values are not allowed", nil)
		}
		return kernel.WrapError(kernel.CodeSchemaInvalidCombination, err, map[string]any{"stage": "trailing_json_decode"})
	}
	return nil
}

func normalizeAggregations(values []string, context map[string]any) ([]kernel.AggFunc, error) {
	out := make([]kernel.AggFunc, 0, len(values))
	seen := map[kernel.AggFunc]bool{}
	for _, value := range values {
		fn, err := kernel.ParseAggFunc(value)
		if err != nil {
			return nil, err
		}
		if seen[fn] {
			if context == nil {
				context = map[string]any{}
			}
			context["function"] = value
			return nil, kernel.NewError(kernel.CodeAggDuplicateFunction, "aggregation function is duplicated", context)
		}
		seen[fn] = true
		out = append(out, fn)
	}
	return out, nil
}

func ppmWeights(raw map[string]int64, scale int64, featureID string) (map[string]int64, error) {
	if scale != 1000 && scale != 10000 {
		return nil, kernel.NewError(kernel.CodeAggWeightOutOfRange, "unsupported fixed-point weight scale", map[string]any{
			"feature_id": featureID,
			"scale":      scale,
			"supported":  []int64{1000, 10000},
		})
	}
	out := make(map[string]int64, len(raw))
	factor := 1_000_000 / scale
	for channel, value := range raw {
		if value < 0 || value > scale {
			return nil, kernel.NewError(kernel.CodeAggWeightOutOfRange, "weight is outside declared fixed-point scale", map[string]any{
				"feature_id": featureID,
				"channel":    channel,
				"raw_weight": value,
				"scale":      scale,
			})
		}
		out[channel] = value * factor
	}
	return out, nil
}

func sortRecords(records []kernel.Record) {
	sort.SliceStable(records, func(i, j int) bool { return records[i].Seq < records[j].Seq })
}

func validateRecordSequence(records []kernel.Record) error {
	seen := map[int64]bool{}
	var expected int64 = 1
	for _, record := range records {
		if record.Seq != expected {
			return kernel.NewError(kernel.CodeSchemaInvalidCombination, "record sequence must be contiguous starting at 1", map[string]any{
				"expected_seq": expected,
				"actual_seq":   record.Seq,
			})
		}
		if seen[record.Seq] {
			return kernel.NewError(kernel.CodeSchemaInvalidCombination, "duplicate record sequence", map[string]any{"seq": record.Seq})
		}
		seen[record.Seq] = true
		expected++
	}
	return nil
}

func firstEventTime(records []kernel.Record) (int64, bool) {
	for _, record := range records {
		if record.Kind == kernel.RecordEvent && record.Event != nil {
			return record.Event.TimeMs, true
		}
	}
	return 0, false
}

func epochHourAnchor(baseMs int64) int64 {
	return baseMs - (baseMs % HourMs)
}

func defaultPathPolicy(threshold float64) kernel.PathPolicy {
	if threshold <= 0 {
		threshold = 3
	}
	return kernel.PathPolicy{
		IncrementalRatioThreshold: threshold,
		Boundary:                  kernel.PathBoundaryRule,
	}
}

func defaultBudget(budget kernel.Budget) kernel.Budget {
	if budget.Workers == 0 {
		budget.Workers = 4
	}
	if budget.MaxWindowsPerFeature == 0 {
		budget.MaxWindowsPerFeature = 256
	}
	return budget
}
