package forward

import (
	"encoding/json"
	"strconv"
	"strings"
)

type jsonObject map[string]any

func decodeObject(data []byte, path string) (jsonObject, error) {
	var raw any
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return nil, NewBizError(CodeSchemaParse, path, "invalid JSON: %v", err)
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, NewBizError(CodeFieldType, path, "top-level document must be an object")
	}
	return obj, nil
}

func asObject(v any, path string) (jsonObject, *BizError) {
	if v == nil {
		return nil, NewBizError(CodeFieldRequired, path, "object is required")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, NewBizError(CodeFieldType, path, "expected object, got %T", v)
	}
	return m, nil
}

func asArray(v any, path string) ([]any, *BizError) {
	if v == nil {
		return nil, nil
	}
	a, ok := v.([]any)
	if !ok {
		return nil, NewBizError(CodeFieldType, path, "expected array, got %T", v)
	}
	return a, nil
}

func getString(m jsonObject, path, key string) (string, *BizError) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", NewBizError(CodeFieldRequired, path+"."+key, "field is required")
	}
	switch s := v.(type) {
	case string:
		return s, nil
	case json.Number:
		return s.String(), nil
	default:
		return "", NewBizError(CodeFieldType, path+"."+key, "expected string, got %T", v)
	}
}

func optString(m jsonObject, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

func getInt64(m jsonObject, path, key string) (int64, *BizError) {
	v, ok := m[key]
	if !ok || v == nil {
		return 0, NewBizError(CodeFieldRequired, path+"."+key, "field is required")
	}
	return toInt64(v, path+"."+key)
}

func optInt64(m jsonObject, key string) (int64, bool, *BizError) {
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false, nil
	}
	n, err := toInt64(v, key)
	return n, true, err
}

func toInt64(v any, path string) (int64, *BizError) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err == nil {
			return i, nil
		}
		f, ferr := n.Float64()
		if ferr != nil {
			return 0, NewBizError(CodeFieldType, path, "invalid integer %q", n.String())
		}
		return int64(f), nil
	case float64:
		return int64(n), nil
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	default:
		return 0, NewBizError(CodeFieldType, path, "expected integer, got %T", v)
	}
}

func toFloat64(v any, path string) (float64, *BizError) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, NewBizError(CodeFieldType, path, "invalid number %q", n.String())
		}
		return f, nil
	case float64:
		return n, nil
	case int64:
		return float64(n), nil
	default:
		return 0, NewBizError(CodeFieldType, path, "expected number, got %T", v)
	}
}

func getBool(m jsonObject, key string) bool {
	v, ok := m[key]
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

func marshalCanonical(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func atoiOrZero(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}
