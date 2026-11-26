package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"screenrelay/internal/model"
)

func decodeObject(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return nil, model.NewError(model.EDecode, "invalid JSON: %v", err)
	}
	obj, ok := root.(map[string]any)
	if !ok {
		return nil, model.SchemaError("$", "root must be a JSON object")
	}
	return obj, nil
}

func field(obj map[string]any, key, path string) (any, string, error) {
	v, ok := obj[key]
	if !ok || v == nil {
		return nil, "", model.SchemaError(path, "missing required field %q", key)
	}
	return v, path + "." + key, nil
}

func optionalField(obj map[string]any, key string) (any, bool) {
	v, ok := obj[key]
	if !ok || v == nil {
		return nil, false
	}
	return v, true
}

func asString(v any, path string) (string, error) {
	switch x := v.(type) {
	case string:
		if x == "" {
			return "", model.SchemaError(path, "string must not be empty")
		}
		return x, nil
	case json.Number:
		return x.String(), nil
	default:
		return "", model.SchemaError(path, "expected string, got %T", v)
	}
}

func asInt(v any, path string) (int64, error) {
	switch x := v.(type) {
	case json.Number:
		n, err := strconv.ParseInt(x.String(), 10, 64)
		if err != nil {
			return 0, model.SchemaError(path, "expected int64, got %q", x.String())
		}
		return n, nil
	case int64:
		return x, nil
	case float64:
		return int64(x), nil
	default:
		return 0, model.SchemaError(path, "expected integer, got %T", v)
	}
}

func asSlice(v any, path string) ([]any, error) {
	items, ok := v.([]any)
	if !ok {
		return nil, model.SchemaError(path, "expected array, got %T", v)
	}
	return items, nil
}

func asObject(v any, path string) (map[string]any, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, model.SchemaError(path, "expected object, got %T", v)
	}
	return obj, nil
}

func logicalPoint(obj map[string]any, xKey, yKey, path string) (model.Point, error) {
	xv, xp, err := field(obj, xKey, path)
	if err != nil {
		return model.Point{}, err
	}
	yv, yp, err := field(obj, yKey, path)
	if err != nil {
		return model.Point{}, err
	}
	xLogical, err := asInt(xv, xp)
	if err != nil {
		return model.Point{}, err
	}
	yLogical, err := asInt(yv, yp)
	if err != nil {
		return model.Point{}, err
	}
	x, err := logicalToCoord(xLogical, xp)
	if err != nil {
		return model.Point{}, err
	}
	y, err := logicalToCoord(yLogical, yp)
	if err != nil {
		return model.Point{}, err
	}
	return model.Point{X: x, Y: y}, nil
}

func logicalRect(obj map[string]any, xKeys, yKeys, wKeys, hKeys []string, path string) (model.Rect, error) {
	get := func(keys []string, kind string) (int64, error) {
		for _, key := range keys {
			if v, ok := optionalField(obj, key); ok {
				p := path + "." + key
				n, err := asInt(v, p)
				if err != nil {
					return 0, err
				}
				return n, nil
			}
		}
		return 0, model.SchemaError(path, "missing required %s field (tried %s)", kind, strings.Join(keys, "/"))
	}
	x, err := get(xKeys, "x")
	if err != nil {
		return model.Rect{}, err
	}
	y, err := get(yKeys, "y")
	if err != nil {
		return model.Rect{}, err
	}
	w, err := get(wKeys, "width")
	if err != nil {
		return model.Rect{}, err
	}
	h, err := get(hKeys, "height")
	if err != nil {
		return model.Rect{}, err
	}
	xi, err := logicalToCoord(x, path+".x")
	if err != nil {
		return model.Rect{}, err
	}
	yi, err := logicalToCoord(y, path+".y")
	if err != nil {
		return model.Rect{}, err
	}
	wi, err := logicalToCoord(w, path+".width")
	if err != nil {
		return model.Rect{}, err
	}
	hi, err := logicalToCoord(h, path+".height")
	if err != nil {
		return model.Rect{}, err
	}
	if wi <= 0 || hi <= 0 {
		return model.Rect{}, model.SchemaError(path, "width and height must be positive")
	}
	return model.Rect{X: xi, Y: yi, W: wi, H: hi}, nil
}

func logicalToCoord(v int64, path string) (int64, error) {
	if v > (1<<62)/model.InternalCoordUnit || v < -(1<<62)/model.InternalCoordUnit {
		return 0, model.OvfError(path, v, model.InternalCoordUnit)
	}
	return v * model.InternalCoordUnit, nil
}

func detectMajor(root map[string]any) (major int, source string, path string, err error) {
	if v, ok := optionalField(root, "schema_version"); ok {
		s, err := asString(v, "$.schema_version")
		if err == nil {
			major, err = majorVersion(s)
			if err != nil {
				if coded, ok := model.AsError(err); ok {
					coded.Path = "$.schema_version"
				}
			}
			return major, s, "$.schema_version", err
		}
		n, err2 := asInt(v, "$.schema_version")
		if err2 != nil {
			return 0, "", "$.schema_version", err2
		}
		return int(n), fmt.Sprint(n), "$.schema_version", nil
	}
	if v, ok := optionalField(root, "schemaVersion"); ok {
		s, err := asString(v, "$.schemaVersion")
		if err != nil {
			return 0, "", "$.schemaVersion", err
		}
		major, err = majorVersion(s)
		if err != nil {
			if coded, ok := model.AsError(err); ok {
				coded.Path = "$.schemaVersion"
			}
		}
		return major, s, "$.schemaVersion", err
	}
	if v, ok := optionalField(root, "ver"); ok {
		s, err := asString(v, "$.ver")
		if err == nil {
			major, err = majorVersion(s)
			if err != nil {
				if coded, ok := model.AsError(err); ok {
					coded.Path = "$.ver"
				}
			}
			return major, s, "$.ver", err
		}
		n, err2 := asInt(v, "$.ver")
		if err2 != nil {
			return 0, "", "$.ver", err2
		}
		return int(n), fmt.Sprintf("%d.0", n), "$.ver", nil
	}
	return 0, "", "$.schema_version", model.SchemaError("$.schema_version", "unknown schema version: no schema_version/schemaVersion/ver field")
}

func majorVersion(s string) (int, error) {
	parts := strings.SplitN(strings.TrimSpace(s), ".", 2)
	n, err := strconv.Atoi(parts[0])
	if err != nil || n <= 0 {
		return 0, model.SchemaError("$.schema_version", "unknown schema version %q", s)
	}
	if n != 1 && n != 2 && n != 3 {
		return 0, model.SchemaError("$.schema_version", "unsupported schema major %q", s)
	}
	return n, nil
}
