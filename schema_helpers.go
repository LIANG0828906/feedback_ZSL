package vpf

import (
	"bytes"
	"encoding/json"
	"fmt"
)

func decodeObject(data []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return nil, &Error{Code: CodeSchema, Message: "invalid JSON document", FieldPath: "$", Details: err.Error()}
	}
	if root == nil {
		return nil, schemaError(0, "$", "root must be a JSON object")
	}
	return root, nil
}

func joinField(parent, key string) string {
	if parent == "" || parent == "$" {
		return "$." + key
	}
	return parent + "." + key
}

func indexField(parent string, index int) string {
	return fmt.Sprintf("%s[%d]", parent, index)
}

func asObject(value any) (map[string]any, bool) {
	m, ok := value.(map[string]any)
	return m, ok
}

func asArray(value any) ([]any, bool) {
	a, ok := value.([]any)
	return a, ok
}

func integerValue(value any) (int64, bool) {
	switch v := value.(type) {
	case json.Number:
		n, err := v.Int64()
		if err == nil {
			return n, true
		}
		f, ferr := v.Float64()
		if ferr == nil && f == float64(int64(f)) {
			return int64(f), true
		}
		return 0, false
	case int:
		return int64(v), true
	case int64:
		return v, true
	case float64:
		if v == float64(int64(v)) {
			return int64(v), true
		}
		return 0, false
	default:
		return 0, false
	}
}

func requiredString(m map[string]any, key, parentPath string, version int) (string, error) {
	path := joinField(parentPath, key)
	value, exists := m[key]
	if !exists || value == nil {
		return "", schemaError(version, path, "required string field is missing")
	}
	s, ok := value.(string)
	if !ok || s == "" {
		return "", schemaError(version, path, "expected a non-empty string")
	}
	return s, nil
}

func requiredBool(m map[string]any, key, parentPath string, version int) (bool, error) {
	path := joinField(parentPath, key)
	value, exists := m[key]
	if !exists || value == nil {
		return false, schemaError(version, path, "required boolean field is missing")
	}
	b, ok := value.(bool)
	if !ok {
		return false, schemaError(version, path, "expected boolean, got %T", value)
	}
	return b, nil
}

func requiredInt(m map[string]any, key, parentPath string, version int) (int64, error) {
	path := joinField(parentPath, key)
	value, exists := m[key]
	if !exists || value == nil {
		return 0, schemaError(version, path, "required integer field is missing")
	}
	n, ok := integerValue(value)
	if !ok {
		return 0, schemaError(version, path, "expected an integer value, got %T", value)
	}
	return n, nil
}

func optionalIntDefault(m map[string]any, key, parentPath string, version int, defaultValue int64, traces *TraceBook, targetPath string) (int64, error) {
	path := joinField(parentPath, key)
	value, exists := m[key]
	if !exists || value == nil {
		traces.Add(FieldTrace{
			SourceVersion: version, SourcePath: path, TargetPath: targetPath,
			Transform: "default_fill", OldValue: nil, NewValue: defaultValue, DefaultValue: defaultValue,
			Note: "optional field absent; declared schema default applied",
		})
		return defaultValue, nil
	}
	n, ok := integerValue(value)
	if !ok {
		return 0, schemaError(version, path, "expected an integer value, got %T", value)
	}
	return n, nil
}

func optionalStringDefault(m map[string]any, key, parentPath string, version int, defaultValue string, traces *TraceBook, targetPath string) (string, error) {
	path := joinField(parentPath, key)
	value, exists := m[key]
	if !exists || value == nil {
		traces.Add(FieldTrace{
			SourceVersion: version, SourcePath: path, TargetPath: targetPath,
			Transform: "default_fill", OldValue: nil, NewValue: defaultValue, DefaultValue: defaultValue,
		})
		return defaultValue, nil
	}
	s, ok := value.(string)
	if !ok {
		return "", schemaError(version, path, "expected string, got %T", value)
	}
	return s, nil
}

func requiredObject(m map[string]any, key, parentPath string, version int) (map[string]any, string, error) {
	path := joinField(parentPath, key)
	value, exists := m[key]
	if !exists || value == nil {
		return nil, path, schemaError(version, path, "required object field is missing")
	}
	obj, ok := asObject(value)
	if !ok {
		return nil, path, schemaError(version, path, "expected object, got %T", value)
	}
	return obj, path, nil
}

func requiredArray(m map[string]any, key, parentPath string, version int) ([]any, string, error) {
	path := joinField(parentPath, key)
	value, exists := m[key]
	if !exists || value == nil {
		return nil, path, schemaError(version, path, "required array field is missing")
	}
	arr, ok := asArray(value)
	if !ok {
		return nil, path, schemaError(version, path, "expected array, got %T", value)
	}
	return arr, path, nil
}

func traceConvert(traces *TraceBook, version int, sourcePath, targetPath, transform string, old, new any) {
	traces.Add(FieldTrace{
		SourceVersion: version,
		SourcePath:    sourcePath,
		TargetPath:    targetPath,
		Transform:     transform,
		OldValue:      old,
		NewValue:      new,
	})
}

func clampIntRange(v int64, min, max int, path string, version int) (int, error) {
	if v < int64(min) || v > int64(max) {
		return 0, schemaError(version, path, "value %d outside declared range [%d,%d]", v, min, max)
	}
	return int(v), nil
}
