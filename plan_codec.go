package channelpipe

import (
	"encoding/json"
	"fmt"
)

const (
	VersionV1 = 1
	VersionV2 = 2
	VersionV3 = 3

	DefaultSeed int64 = 424242
)

func LoadPlanSet(data []byte) (*LoadResult, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, WrapError(CodeSchema, err, "$", "plan document is not valid JSON")
	}

	versionField := ""
	var rawVersion json.RawMessage
	for _, candidate := range []string{"plan_ver", "schemaVersion", "version"} {
		if raw, ok := envelope[candidate]; ok {
			versionField = candidate
			rawVersion = raw
			break
		}
	}
	if versionField == "" {
		return nil, FieldError(CodeSchema, "version", "one of plan_ver, schemaVersion or version is required")
	}

	var version int
	if err := json.Unmarshal(rawVersion, &version); err != nil {
		return nil, WrapError(CodeSchema, err, versionField, "version must be an integer")
	}
	if version != VersionV1 && version != VersionV2 && version != VersionV3 {
		return nil, FieldError(CodeVersion, versionField, "unsupported plan version %d", version).
			withDetails(map[string]any{"supported": []int{1, 2, 3}, "value": version})
	}

	switch version {
	case VersionV1:
		return loadV1(data, versionField)
	case VersionV2:
		return loadV2(data, versionField)
	default:
		return loadV3(data, versionField)
	}
}

func (e *Error) withDetails(details map[string]any) *Error {
	if e.Details == nil {
		e.Details = details
	} else {
		for key, value := range details {
			e.Details[key] = value
		}
	}
	return e
}

func rawInt(raw json.RawMessage, field string) (int, error) {
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, WrapError(CodeSchema, err, field, "field must be an integer")
	}
	return value, nil
}

func requirePointer[T any](value *T, path string) (T, error) {
	var zero T
	if value == nil {
		return zero, FieldError(CodeSchema, path, "required field is missing")
	}
	return *value, nil
}

func stringDefault(value *string, defaultValue string, planID string, stageIndex int, path string, traces *[]FieldTrace) string {
	if value == nil || *value == "" {
		*traces = append(*traces, FieldTrace{
			PlanID:        planID,
			StageIndex:    stageIndex,
			FieldPath:     path,
			OldValue:      nil,
			SourceDefault: defaultValue,
			Reason:        "low-version upgrade default",
		})
		return defaultValue
	}
	return *value
}

func intDefault(value *int, defaultValue int, planID string, stageIndex int, path string, traces *[]FieldTrace) int {
	if value == nil {
		*traces = append(*traces, FieldTrace{
			PlanID:        planID,
			StageIndex:    stageIndex,
			FieldPath:     path,
			OldValue:      nil,
			SourceDefault: defaultValue,
			Reason:        "low-version upgrade default",
		})
		return defaultValue
	}
	return *value
}

func int64Default(value *int64, defaultValue int64, planID string, path string, traces *[]FieldTrace) int64 {
	if value == nil {
		*traces = append(*traces, FieldTrace{
			PlanID:        planID,
			FieldPath:     path,
			OldValue:      nil,
			SourceDefault: defaultValue,
			Reason:        "low-version upgrade default",
		})
		return defaultValue
	}
	return *value
}

func parseLayout(value string, path string) ([]Channel, error) {
	if value == "" {
		return nil, FieldError(CodeSchema, path, "channel layout is empty")
	}
	layout := make([]Channel, 0, len(value))
	for _, r := range value {
		channel := Channel(fmt.Sprintf("%c", r))
		switch channel {
		case ChannelR, ChannelG, ChannelB, ChannelA:
			layout = append(layout, channel)
		default:
			return nil, FieldError(CodeSchema, path, "unknown channel %q", channel)
		}
	}
	return layout, nil
}

func layoutString(layout []Channel) string {
	result := ""
	for _, channel := range layout {
		result += string(channel)
	}
	return result
}

func resolveChannelIndex(layout []Channel, channel Channel, path string) (int, error) {
	index := channelIndex(layout, channel)
	if index < 0 {
		return -1, FieldError(CodeSchema, path, "channel %s is absent from input layout %s", channel, layoutString(layout))
	}
	return index, nil
}
