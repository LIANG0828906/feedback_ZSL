package downsample

import (
	"encoding/json"
	"fmt"
)

type rawObject map[string]json.RawMessage

func allowedSet(keys ...string) map[string]bool {
	out := make(map[string]bool, len(keys))
	for _, key := range keys {
		out[key] = true
	}
	return out
}

func parseStrictObject(data []byte, version SchemaVersion, path string, allowed map[string]bool) (rawObject, *Error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, NewError(CodeSchemaInvalidJSON, "invalid JSON object").
			WithVersion(string(version)).
			WithFieldPath(path).
			WithContext("parse_error", err.Error())
	}
	for key := range obj {
		if !allowed[key] {
			fieldPath := joinPath(path, key)
			return nil, NewError(CodeSchemaUnknownField, "unrecognized schema field").
				WithVersion(string(version)).
				WithFieldPath(fieldPath)
		}
	}
	return rawObject(obj), nil
}

func (o rawObject) raw(key string) (json.RawMessage, bool) {
	v, ok := o[key]
	return v, ok
}

func decodeStrict[T any](raw json.RawMessage, version SchemaVersion, path string) (T, *Error) {
	var zero T
	if len(raw) == 0 {
		return zero, NewError(CodeSchemaInvalidJSON, "missing JSON value").
			WithVersion(string(version)).WithFieldPath(path)
	}
	if err := json.Unmarshal(raw, &zero); err != nil {
		return zero, NewError(CodeSchemaInvalidJSON, "invalid JSON value").
			WithVersion(string(version)).
			WithFieldPath(path).
			WithContext("parse_error", err.Error())
	}
	return zero, nil
}

func getString(o rawObject, key string, version SchemaVersion, path string, required bool) (string, *Error) {
	raw, ok := o.raw(key)
	if !ok {
		if required {
			return "", NewError(CodeSchemaFieldCombination, "required field is missing").
				WithVersion(string(version)).WithFieldPath(joinPath(path, key))
		}
		return "", nil
	}
	return decodeStrict[string](raw, version, joinPath(path, key))
}

func getInt64(o rawObject, key string, version SchemaVersion, path string, required bool) (int64, *Error) {
	raw, ok := o.raw(key)
	if !ok {
		if required {
			return 0, NewError(CodeSchemaFieldCombination, "required field is missing").
				WithVersion(string(version)).WithFieldPath(joinPath(path, key))
		}
		return 0, nil
	}
	return decodeStrict[int64](raw, version, joinPath(path, key))
}

func getInt(o rawObject, key string, version SchemaVersion, path string, required bool) (int, *Error) {
	v, err := getInt64(o, key, version, path, required)
	return int(v), err
}

func getFloat64(o rawObject, key string, version SchemaVersion, path string, required bool) (float64, *Error) {
	raw, ok := o.raw(key)
	if !ok {
		if required {
			return 0, NewError(CodeSchemaFieldCombination, "required field is missing").
				WithVersion(string(version)).WithFieldPath(joinPath(path, key))
		}
		return 0, nil
	}
	return decodeStrict[float64](raw, version, joinPath(path, key))
}

func getBool(o rawObject, key string, version SchemaVersion, path string, required bool) (bool, *Error) {
	raw, ok := o.raw(key)
	if !ok {
		if required {
			return false, NewError(CodeSchemaFieldCombination, "required field is missing").
				WithVersion(string(version)).WithFieldPath(joinPath(path, key))
		}
		return false, nil
	}
	return decodeStrict[bool](raw, version, joinPath(path, key))
}

func joinPath(parent, key string) string {
	if parent == "" || parent == "$" {
		return "$." + key
	}
	return parent + "." + key
}

func indexPath(parent string, i int) string {
	return fmt.Sprintf("%s[%d]", parent, i)
}

func parseScale(value, version, path string) (WeightScale, *Error) {
	switch value {
	case "permille_1000", "PERMILLE_1000", "1000", "thousandths":
		return ScalePermille, nil
	case "permyriad_10000", "PERMYRIAD_10000", "10000", "ten_thousandths":
		return ScalePermyriad, nil
	default:
		return 0, NewError(CodeWeightScaleOutOfRange, "unsupported weight scale").
			WithVersion(version).
			WithFieldPath(path).
			WithContext("value", value).
			WithContext("allowed", []string{"permille_1000", "permyriad_10000"})
	}
}

func parseFuncName(value string) Func {
	switch value {
	case "AVG", "Avg", "avg", "mean":
		return FuncAvg
	case "SUM", "Sum", "sum":
		return FuncSum
	case "MIN", "Min", "min":
		return FuncMin
	case "MAX", "Max", "max":
		return FuncMax
	case "COUNT", "Count", "count":
		return FuncCount
	case "FIRST", "First", "first":
		return FuncFirst
	case "LAST", "Last", "last":
		return FuncLast
	case "WEIGHTED_AVG", "WeightedAvg", "weighted_avg", "wavg":
		return FuncWeightedAvg
	default:
		return Func(value)
	}
}
