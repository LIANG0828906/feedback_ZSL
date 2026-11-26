package mirror

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	minOffset32 int64 = -1 << 31
	maxOffset32 int64 = 1<<31 - 1
)

type jsonObj map[string]any

func decodeJSONObject(data []byte) (jsonObj, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, errf(CodeSchema, "$", "invalid JSON: %v", err)
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, errf(CodeSchema, "$", "document root must be an object")
	}
	return jsonObj(obj), nil
}

func (o jsonObj) required(key, path string) (any, error) {
	v, ok := o[key]
	if !ok || v == nil {
		return nil, errf(CodeSchema, path+"."+key, "required field is missing")
	}
	return v, nil
}

func (o jsonObj) optional(key string) (any, bool) {
	v, ok := o[key]
	return v, ok && v != nil
}

func asObject(v any, path string) (jsonObj, error) {
	switch m := v.(type) {
	case map[string]any:
		return jsonObj(m), nil
	case jsonObj:
		return m, nil
	default:
		return nil, errf(CodeSchema, path, "expected object")
	}
}

func asArray(v any, path string) ([]any, error) {
	a, ok := v.([]any)
	if !ok {
		return nil, errf(CodeSchema, path, "expected array")
	}
	return a, nil
}

func asString(v any, path string) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", errf(CodeSchema, path, "expected string")
	}
	return s, nil
}

func asBool(v any, path string) (bool, error) {
	b, ok := v.(bool)
	if !ok {
		return false, errf(CodeSchema, path, "expected boolean")
	}
	return b, nil
}

func asInt64(v any, path string) (int64, error) {
	switch n := v.(type) {
	case json.Number:
		i, err := strconv.ParseInt(n.String(), 10, 64)
		if err != nil {
			return 0, errf(CodeSchema, path, "expected int64, got %q", n.String())
		}
		return i, nil
	case float64:
		if math.Trunc(n) != n || n < math.MinInt64 || n > math.MaxInt64 {
			return 0, errf(CodeSchema, path, "expected int64")
		}
		return int64(n), nil
	case int:
		return int64(n), nil
	case int64:
		return n, nil
	default:
		return 0, errf(CodeSchema, path, "expected integer")
	}
}

func requiredString(o jsonObj, key, path string) (string, error) {
	v, err := o.required(key, path)
	if err != nil {
		return "", err
	}
	return asString(v, path+"."+key)
}

func requiredInt(o jsonObj, key, path string) (int64, error) {
	v, err := o.required(key, path)
	if err != nil {
		return 0, err
	}
	return asInt64(v, path+"."+key)
}

func requiredObject(o jsonObj, key, path string) (jsonObj, error) {
	v, err := o.required(key, path)
	if err != nil {
		return nil, err
	}
	return asObject(v, path+"."+key)
}

func requiredArray(o jsonObj, key, path string) ([]any, error) {
	v, err := o.required(key, path)
	if err != nil {
		return nil, err
	}
	return asArray(v, path+"."+key)
}

func optionalBool(o jsonObj, key, path string, def bool) (bool, error) {
	v, ok := o.optional(key)
	if !ok {
		return def, nil
	}
	return asBool(v, path+"."+key)
}

// detectSchemaVersion recognizes only the three declared wire versions.
func detectSchemaVersion(root jsonObj) (string, error) {
	if v, ok := root["schema_version"]; ok {
		s, err := asString(v, "schema_version")
		if err != nil {
			return "", err
		}
		switch s {
		case "1", "1.0":
			return SchemaV1, nil
		default:
			return "", errf(CodeSchema, "schema_version", "unknown schema version %q", s)
		}
	}
	if v, ok := root["schemaVersion"]; ok {
		var code string
		switch n := v.(type) {
		case json.Number:
			code = n.String()
		case string:
			code = n
		default:
			return "", errf(CodeSchema, "schemaVersion", "unsupported version encoding")
		}
		switch code {
		case "2", "2.0":
			return SchemaV2, nil
		case "3", "3.0", "v3":
			return SchemaV3, nil
		default:
			return "", errf(CodeSchema, "schemaVersion", "unknown schema version %q", code)
		}
	}
	return "", errf(CodeSchema, "schema_version", "required version field is missing")
}

func scaleFromV1(v any, path string) (Scale, error) {
	s, err := asString(v, path)
	if err != nil {
		return 0, err
	}
	switch s {
	case "100%":
		return Scale100, nil
	case "125%":
		return Scale125, nil
	default:
		return 0, errf(CodeSchema, path, "scale enum %q is out of range for schema 1", s)
	}
}

func scaleToV1(s Scale, path string) (string, *Loss) {
	switch s {
	case Scale100:
		return "100%", nil
	case Scale125:
		return "125%", nil
	default:
		return "", &Loss{Path: path, Value: s.String(), Reason: "schema 1 scale enum only supports 100% and 125%", Disposition: "reject export"}
	}
}

func scaleFromV2(v any, path string) (Scale, error) {
	code, err := asInt64(v, path)
	if err != nil {
		return 0, err
	}
	switch code {
	case 10000:
		return Scale100, nil
	case 12500:
		return Scale125, nil
	case 15000:
		return Scale150, nil
	default:
		return 0, errf(CodeSchema, path, "scale enum code %d is out of range for schema 2", code)
	}
}

func scaleToV2(s Scale, path string) (int64, *Loss) {
	switch s {
	case Scale100, Scale125, Scale150:
		return int64(s) * 100, nil
	default:
		return 0, &Loss{Path: path, Value: s.String(), Reason: "schema 2 scale enum only supports 100%, 125%, and 150%", Disposition: "reject export"}
	}
}

func scaleFromV3(v any, path string) (Scale, error) {
	s, err := asString(v, path)
	if err != nil {
		return 0, err
	}
	switch s {
	case "S100":
		return Scale100, nil
	case "S125":
		return Scale125, nil
	case "S150":
		return Scale150, nil
	case "S200":
		return Scale200, nil
	default:
		return 0, errf(CodeSchema, path, "scale enum code %q is out of range for schema 3", s)
	}
}

func scaleToV3(s Scale) (string, *Loss) {
	switch s {
	case Scale100, Scale125, Scale150, Scale200:
		return fmt.Sprintf("S%d", int64(s)), nil
	default:
		return "", &Loss{Path: "scale", Value: s.String(), Reason: "unknown schema 3 scale enum", Disposition: "reject export"}
	}
}

func chromaFromV1(v any, path string) (Chroma, error) {
	s, err := asString(v, path)
	if err != nil {
		return "", err
	}
	switch s {
	case "444":
		return Chroma444, nil
	case "420":
		return Chroma420, nil
	default:
		return "", errf(CodeSchema, path, "chroma enum %q is out of range for schema 1", s)
	}
}

func chromaToV1(c Chroma, path string) (string, *Loss) {
	switch c {
	case Chroma444:
		return "444", nil
	case Chroma420:
		return "420", nil
	default:
		return "", &Loss{Path: path, Value: string(c), Reason: "schema 1 chroma enum only supports 444 and 420", Disposition: "reject export"}
	}
}

func chromaFromV2(v any, path string) (Chroma, error) {
	code, err := asInt64(v, path)
	if err != nil {
		return "", err
	}
	switch code {
	case 0:
		return Chroma444, nil
	case 1:
		return Chroma422, nil
	case 2:
		return Chroma420, nil
	default:
		return "", errf(CodeSchema, path, "chroma enum code %d is out of range for schema 2", code)
	}
}

func chromaToV2(c Chroma, path string) (int64, *Loss) {
	switch c {
	case Chroma444:
		return 0, nil
	case Chroma422:
		return 1, nil
	case Chroma420:
		return 2, nil
	default:
		return 0, &Loss{Path: path, Value: string(c), Reason: "schema 2 chroma enum only supports 444, 422, and 420", Disposition: "reject export"}
	}
}

func chromaFromV3(v any, path string) (Chroma, error) {
	s, err := asString(v, path)
	if err != nil {
		return "", err
	}
	switch s {
	case "YUV444":
		return Chroma444, nil
	case "YUV422":
		return Chroma422, nil
	case "YUV420":
		return Chroma420, nil
	case "YUV411":
		return Chroma411, nil
	default:
		return "", errf(CodeSchema, path, "chroma enum code %q is out of range for schema 3", s)
	}
}

func chromaToV3(c Chroma) (string, *Loss) {
	switch c {
	case Chroma444, Chroma422, Chroma420, Chroma411:
		return "YUV" + strings.ReplaceAll(string(c), ":", ""), nil
	default:
		return "", &Loss{Path: "chroma", Value: string(c), Reason: "unknown schema 3 chroma enum", Disposition: "reject export"}
	}
}

func parseScaleList(v any, version, path string) ([]Scale, error) {
	arr, err := asArray(v, path)
	if err != nil {
		return nil, err
	}
	if len(arr) == 0 {
		return nil, errf(CodeSchema, path, "at least one scale is required")
	}
	out := make([]Scale, 0, len(arr))
	seen := map[Scale]bool{}
	for i, item := range arr {
		p := fmt.Sprintf("%s[%d]", path, i)
		var s Scale
		switch version {
		case SchemaV1:
			s, err = scaleFromV1(item, p)
		case SchemaV2:
			s, err = scaleFromV2(item, p)
		case SchemaV3:
			s, err = scaleFromV3(item, p)
		default:
			err = errf(CodeSchema, p, "unsupported version")
		}
		if err != nil {
			return nil, err
		}
		if seen[s] {
			return nil, errf(CodeSchema, p, "duplicate scale enum")
		}
		seen[s] = true
		out = append(out, s)
	}
	return out, nil
}

func parseChromaList(v any, version, path string) ([]Chroma, error) {
	arr, err := asArray(v, path)
	if err != nil {
		return nil, err
	}
	if len(arr) == 0 {
		return nil, errf(CodeSchema, path, "at least one chroma mode is required")
	}
	out := make([]Chroma, 0, len(arr))
	seen := map[Chroma]bool{}
	for i, item := range arr {
		p := fmt.Sprintf("%s[%d]", path, i)
		var c Chroma
		switch version {
		case SchemaV1:
			c, err = chromaFromV1(item, p)
		case SchemaV2:
			c, err = chromaFromV2(item, p)
		case SchemaV3:
			c, err = chromaFromV3(item, p)
		default:
			err = errf(CodeSchema, p, "unsupported version")
		}
		if err != nil {
			return nil, err
		}
		if seen[c] {
			return nil, errf(CodeSchema, p, "duplicate chroma enum")
		}
		seen[c] = true
		out = append(out, c)
	}
	return out, nil
}

func parseScale(v any, version, path string) (Scale, error) {
	switch version {
	case SchemaV1:
		return scaleFromV1(v, path)
	case SchemaV2:
		return scaleFromV2(v, path)
	case SchemaV3:
		return scaleFromV3(v, path)
	default:
		return 0, errf(CodeSchema, path, "unsupported version")
	}
}

func parseChroma(v any, version, path string) (Chroma, error) {
	switch version {
	case SchemaV1:
		return chromaFromV1(v, path)
	case SchemaV2:
		return chromaFromV2(v, path)
	case SchemaV3:
		return chromaFromV3(v, path)
	default:
		return "", errf(CodeSchema, path, "unsupported version")
	}
}

func msToTicks(field string, ms, tickMillis int64, mode RoundingMode) (int64, int64, error) {
	if tickMillis <= 0 {
		return 0, 0, &Error{Code: CodeSchema, Path: field, Message: "tick duration must be positive"}
	}
	ticks, residual, err := MulDivChecked(field, ms, 1, tickMillis, mode)
	if err != nil {
		return 0, 0, err
	}
	return ticks, residual, nil
}

func ticksToMillis(field string, ticks, tickMillis int64) (int64, error) {
	ms, _, err := MulDivChecked(field, ticks, tickMillis, 1, RoundFloor)
	return ms, err
}

func parseTimeObject(v any, path string) (int64, error) {
	o, err := asObject(v, path)
	if err != nil {
		return 0, err
	}
	sec, err := requiredInt(o, "sec", path)
	if err != nil {
		return 0, err
	}
	ms, err := requiredInt(o, "ms", path)
	if err != nil {
		return 0, err
	}
	if ms < 0 || ms > 999 {
		return 0, errf(CodeSchema, path+".ms", "must be in [0,999]")
	}
	secMs, _, err := MulDivChecked(path+".sec", sec, 1000, 1, RoundFloor)
	if err != nil {
		return 0, err
	}
	total, err := AddChecked(path, secMs, ms)
	if err != nil {
		return 0, err
	}
	return total, nil
}

func trace(v string, path string, old, newValue any, rule string, residual int64) ConversionTrace {
	return ConversionTrace{Version: v, Path: path, OldValue: old, NewValue: newValue, Rule: rule, Residual: residual}
}
