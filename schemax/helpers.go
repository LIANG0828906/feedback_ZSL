package schemax

import (
	"encoding/json"
	"fmt"

	"iconflow/errs"
)

type rawObject map[string]json.RawMessage

func decodeObject(data []byte, field string) (rawObject, *errs.Error) {
	var obj rawObject
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, errs.Schema(field, "invalid JSON object: %v", err)
	}
	if obj == nil {
		return nil, errs.Schema(field, "document must be a JSON object")
	}
	return obj, nil
}

func asObject(raw json.RawMessage, field string) (rawObject, *errs.Error) {
	obj, err := decodeObject(raw, field)
	if err != nil {
		return nil, err
	}
	return obj, nil
}

func field(obj rawObject, key, path string) (json.RawMessage, *errs.Error) {
	raw, ok := obj[key]
	if !ok || string(raw) == "null" {
		return nil, errs.Schema(path+"."+key, "missing required field %q", key)
	}
	return raw, nil
}

func getString(obj rawObject, key, path string) (string, *errs.Error) {
	raw, err := field(obj, key, path)
	if err != nil {
		return "", err
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", errs.Schema(path+"."+key, "expected string: %v", err)
	}
	if value == "" {
		return "", errs.Schema(path+"."+key, "must not be empty")
	}
	return value, nil
}

func optionalString(obj rawObject, key string) string {
	raw, ok := obj[key]
	if !ok {
		return ""
	}
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func getInt(obj rawObject, key, path string) (int, *errs.Error) {
	raw, err := field(obj, key, path)
	if err != nil {
		return 0, err
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		var floatValue float64
		if ferr := json.Unmarshal(raw, &floatValue); ferr == nil && floatValue == float64(int(floatValue)) {
			return int(floatValue), nil
		}
		return 0, errs.Schema(path+"."+key, "expected integer: %v", err)
	}
	return value, nil
}

func optionalInt(obj rawObject, key string, fallback int) int {
	raw, ok := obj[key]
	if !ok {
		return fallback
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		return fallback
	}
	return value
}

func getBool(obj rawObject, key, path string) (bool, *errs.Error) {
	raw, err := field(obj, key, path)
	if err != nil {
		return false, err
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, errs.Schema(path+"."+key, "expected boolean: %v", err)
	}
	return value, nil
}

func optionalBool(obj rawObject, key string, fallback bool) bool {
	raw, ok := obj[key]
	if !ok {
		return fallback
	}
	var value bool
	_ = json.Unmarshal(raw, &value)
	return value
}

func getSlice(raw json.RawMessage, path string) ([]json.RawMessage, *errs.Error) {
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, errs.Schema(path, "expected array: %v", err)
	}
	return values, nil
}

func intSlice(raw json.RawMessage, path string) ([]int, *errs.Error) {
	var values []int
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, errs.Schema(path, "expected integer array: %v", err)
	}
	return values, nil
}

func marshalObject(v any) (rawObject, []byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	obj, err := decodeObject(data, "$")
	if err != nil {
		return nil, nil, fmt.Errorf("%w", err)
	}
	return obj, data, nil
}
