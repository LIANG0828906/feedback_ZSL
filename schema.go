package plancache

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
)

// RenderMap returns a version-specific wire document. The generator uses this
// so tests can assert that all three documents originate from one truth model.
func RenderMap(p *Project, version string) (map[string]any, error) {
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	switch version {
	case SchemaV1:
		return renderV1(p)
	case SchemaV2:
		return renderV2(p), nil
	case SchemaV3:
		return renderV3(p), nil
	default:
		return nil, newError(CodeSchemaUnknownVersion, "schema", "unknown schema version %q", version)
	}
}

func Render(p *Project, version string) ([]byte, error) {
	doc, err := RenderMap(p, version)
	if err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return b, nil
}

func Load(data []byte) (*Project, error) {
	root, err := decodeObject(data, "$")
	if err != nil {
		return nil, err
	}
	v1, has1 := root["schema_id"]
	v2, has2 := root["schemaId"]
	v3, has3 := root["SchemaID"]
	values := map[string]string{}
	if has1 {
		values["schema_id"] = asString(v1)
	}
	if has2 {
		values["schemaId"] = asString(v2)
	}
	if has3 {
		values["SchemaID"] = asString(v3)
	}
	if len(values) > 1 {
		expected := ""
		for _, v := range values {
			if expected == "" {
				expected = v
			} else if v != expected {
				return nil, withContext(newError(CodeSchemaFieldConflict, "$.schema", "multiple schema identifiers cannot be disambiguated"), "values", values)
			}
		}
	}
	version := ""
	for _, v := range values {
		version = v
	}
	switch version {
	case SchemaV1:
		return loadV1(root)
	case SchemaV2:
		return loadV2(root)
	case SchemaV3:
		return loadV3(root)
	case "":
		return nil, newError(CodeSchemaMissingField, "$.schema", "schema identifier is required")
	default:
		return nil, newError(CodeSchemaUnknownVersion, "$.schema", "unsupported schema version %q", version)
	}
}

func LoadV1(data []byte) (*Project, error) {
	root, err := decodeObject(data, "$")
	if err != nil {
		return nil, err
	}
	return loadV1(root)
}

func LoadV2(data []byte) (*Project, error) {
	root, err := decodeObject(data, "$")
	if err != nil {
		return nil, err
	}
	return loadV2(root)
}

func LoadV3(data []byte) (*Project, error) {
	root, err := decodeObject(data, "$")
	if err != nil {
		return nil, err
	}
	return loadV3(root)
}

func decodeObject(data []byte, path string) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, withContext(newError(CodeSchemaUnknownVersion, path, "invalid JSON document: %v", err), "detail", err.Error())
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, newError(CodeSchemaUnknownVersion, path, "top-level document must be an object")
	}
	return m, nil
}

func asString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case nil:
		return ""
	default:
		return fmt.Sprint(x)
	}
}

func requireString(m map[string]any, key, path string) (string, error) {
	v, ok := m[key]
	if !ok || v == nil || asString(v) == "" {
		return "", newError(CodeSchemaMissingField, path+"."+key, "required string field is missing or empty")
	}
	if _, ok := v.(string); !ok {
		return "", withContext(newError(CodeSchemaFieldConflict, path+"."+key, "field must be a JSON string"), "actual_type", fmt.Sprintf("%T", v))
	}
	return v.(string), nil
}

func optionalString(m map[string]any, key string) string {
	if v, ok := m[key]; ok && v != nil {
		return asString(v)
	}
	return ""
}

func asMap(v any, path string) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, newError(CodeSchemaFieldConflict, path, "expected object")
	}
	return m, nil
}

func asArray(v any, path string) ([]any, error) {
	a, ok := v.([]any)
	if !ok {
		return nil, newError(CodeSchemaFieldConflict, path, "expected array")
	}
	return a, nil
}

func int64Value(v any, path string) (int64, error) {
	switch x := v.(type) {
	case json.Number:
		n, err := x.Int64()
		if err == nil {
			return n, nil
		}
		f, ferr := x.Float64()
		if ferr == nil && math.Trunc(f) == f {
			return int64(f), nil
		}
		return 0, withContext(newError(CodeSchemaFieldConflict, path, "invalid integer %q", x.String()), "detail", err.Error())
	case float64:
		if math.Trunc(x) == x {
			return int64(x), nil
		}
	}
	return 0, newError(CodeSchemaFieldConflict, path, "expected integer-compatible number")
}

func intValue(v any, path string) (int, error) {
	n, err := int64Value(v, path)
	return int(n), err
}

func optionalInt(m map[string]any, key, path string, def int) (int, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return def, nil
	}
	return intValue(v, path+"."+key)
}

func stringSlice(v any) []string {
	a, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(a))
	for _, x := range a {
		out = append(out, asString(x))
	}
	return out
}

func intSlice(v any, path string) ([]int, error) {
	a, ok := v.([]any)
	if !ok {
		return nil, newError(CodeSchemaFieldConflict, path, "expected integer array")
	}
	out := make([]int, 0, len(a))
	for i, x := range a {
		n, err := intValue(x, fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

func anySlice[T any](v any, path string, fn func(map[string]any, string) (T, error)) ([]T, error) {
	a, err := asArray(v, path)
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(a))
	for i, item := range a {
		m, err := asMap(item, fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return nil, err
		}
		x, err := fn(m, fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, nil
}

func settingsMap(v any) map[string]string {
	m, ok := v.(map[string]any)
	if !ok {
		return map[string]string{}
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		out[k] = asString(val)
	}
	return out
}

func renderSettingsRules(rules map[string]SettingRule, pascal bool) map[string]any {
	out := map[string]any{}
	for k, r := range rules {
		if pascal {
			out[k] = map[string]any{"Mode": r.Mode, "AllowedValues": r.AllowedValues, "Minimum": r.Minimum}
		} else {
			out[k] = map[string]any{"mode": r.Mode, "allowed_values": r.AllowedValues, "minimum_value": r.Minimum}
		}
	}
	return out
}

func loadSettingsRules(v any, path string) (map[string]SettingRule, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return map[string]SettingRule{}, nil
	}
	out := map[string]SettingRule{}
	for k, rv := range m {
		rm, err := asMap(rv, path+"."+k)
		if err != nil {
			return nil, err
		}
		mode := optionalString(rm, "mode")
		if mode == "" {
			mode = optionalString(rm, "Mode")
		}
		var allowed []string
		if av, ok := rm["allowed_values"]; ok {
			allowed = stringSlice(av)
		} else if av, ok := rm["AllowedValues"]; ok {
			allowed = stringSlice(av)
		}
		var min int64
		for _, key := range []string{"minimum_value", "minimum", "Minimum"} {
			if mv, ok := rm[key]; ok {
				min, err = int64Value(mv, path+"."+k+"."+key)
				if err != nil {
					return nil, err
				}
				break
			}
		}
		out[k] = SettingRule{Mode: mode, AllowedValues: allowed, Minimum: min}
	}
	return out, nil
}
