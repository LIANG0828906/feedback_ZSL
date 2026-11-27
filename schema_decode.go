package grid

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// DecodeVersion detects one of the three supported schema revisions and
// upgrades it to the canonical fixed-point model.
func DecodeVersion(data []byte) (*Level, ConversionReport, error) {
	root, err := decodeObject(data)
	if err != nil {
		return nil, ConversionReport{}, err
	}
	version, verr := detectSchema(root)
	if verr != nil {
		return nil, ConversionReport{}, verr
	}
	switch version {
	case 1:
		l, rep, err := decodeV1(root)
		rep.TargetSchema = 3
		if err == nil && l != nil {
			l.Seed = seedFromCode(l.Code)
		}
		return l, rep, err
	case 2:
		l, rep, err := decodeV2(root)
		rep.TargetSchema = 3
		if err == nil && l != nil {
			l.Seed = seedFromCode(l.Code)
		}
		return l, rep, err
	case 3:
		l, rep, err := decodeV3(root)
		rep.TargetSchema = 3
		if err == nil && l != nil {
			l.Seed = seedFromCode(l.Code)
		}
		return l, rep, err
	default:
		return nil, ConversionReport{}, decodeError("", "$.schemaVersion", "unsupported schema version %d", version)
	}
}

func decodeObject(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil, decodeError("", "$", "invalid JSON document: %v", err)
	}
	if root == nil {
		return nil, decodeError("", "$", "document must be a JSON object")
	}
	return root, nil
}

func detectSchema(root map[string]any) (int, error) {
	if v, ok := root["schema_version"]; ok {
		n, err := asInt(v, "$.schema_version")
		if err != nil {
			return 0, err
		}
		return int(n), nil
	}
	if v, ok := root["schema"]; ok {
		n, err := asInt(v, "$.schema")
		if err != nil {
			return 0, err
		}
		return int(n), nil
	}
	if v, ok := root["schemaLevel"]; ok {
		n, err := asInt(v, "$.schemaLevel")
		if err != nil {
			return 0, err
		}
		return int(n), nil
	}
	return 0, decodeError("", "$", "missing schema version field (schema_version/schema/schemaLevel)")
}

func asInt(v any, path string) (int64, error) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, decodeError("", path, "must be an int64 integer: %v", err)
		}
		return i, nil
	case int64:
		return n, nil
	case float64:
		return int64(n), nil
	default:
		return 0, decodeError("", path, "must be an integer, got %T", v)
	}
}

func reqField(m map[string]any, key, path string) (any, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, decodeError("", join(path, key), "missing required field %q", key)
	}
	return v, nil
}

func reqMap(m map[string]any, key, path string) (map[string]any, error) {
	v, err := reqField(m, key, path)
	if err != nil {
		return nil, err
	}
	out, ok := v.(map[string]any)
	if !ok {
		return nil, decodeError("", join(path, key), "must be an object")
	}
	return out, nil
}

func reqString(m map[string]any, key, path string) (string, error) {
	v, err := reqField(m, key, path)
	if err != nil {
		return "", err
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", decodeError("", join(path, key), "must be a non-empty string")
	}
	return s, nil
}

func reqIntField(m map[string]any, key, path, objectID string) (int64, error) {
	v, err := reqField(m, key, path)
	if err != nil {
		if ge, ok := AsError(err, CodeDecode); ok {
			ge.ObjectID = objectID
		}
		return 0, err
	}
	n, err := asInt(v, join(path, key))
	if err != nil {
		if ge, ok := AsError(err, CodeDecode); ok {
			ge.ObjectID = objectID
		}
		return 0, err
	}
	return n, nil
}

func reqPointMM(m map[string]any, key, path, objectID, xKey, yKey string) (Point, error) {
	pm, err := reqMap(m, key, path)
	if err != nil {
		if ge, ok := AsError(err, CodeDecode); ok {
			ge.ObjectID = objectID
		}
		return Point{}, err
	}
	fp := join(path, key)
	x, err := reqIntField(pm, xKey, fp, objectID)
	if err != nil {
		return Point{}, err
	}
	y, err := reqIntField(pm, yKey, fp, objectID)
	if err != nil {
		return Point{}, err
	}
	ux, err := MMToUnits(objectID, join(fp, xKey), x)
	if err != nil {
		return Point{}, err
	}
	uy, err := MMToUnits(objectID, join(fp, yKey), y)
	if err != nil {
		return Point{}, err
	}
	return Point{X: ux, Y: uy}, nil
}

func reqPointPlain(m map[string]any, key, path, objectID string) (Point, error) {
	pm, err := reqMap(m, key, path)
	if err != nil {
		if ge, ok := AsError(err, CodeDecode); ok {
			ge.ObjectID = objectID
		}
		return Point{}, err
	}
	fp := join(path, key)
	x, err := reqIntField(pm, "x", fp, objectID)
	if err != nil {
		return Point{}, err
	}
	y, err := reqIntField(pm, "y", fp, objectID)
	if err != nil {
		return Point{}, err
	}
	return Point{X: x, Y: y}, nil
}

func reqMMPointPlain(m map[string]any, key, path, objectID string) (Point, error) {
	p, err := reqPointPlain(m, key, path, objectID)
	if err != nil {
		return Point{}, err
	}
	x, err := MMToUnits(objectID, join(join(path, key), "x"), p.X)
	if err != nil {
		return Point{}, err
	}
	y, err := MMToUnits(objectID, join(join(path, key), "y"), p.Y)
	if err != nil {
		return Point{}, err
	}
	return Point{X: x, Y: y}, nil
}

func join(path, field string) string {
	if path == "" || path == "$" {
		if field != "" && field[0] == '[' {
			return "$" + field
		}
		return "$." + field
	}
	if len(field) > 0 && field[0] == '[' {
		return path + field
	}
	return path + "." + field
}

func arrayItem(path string, i int) string {
	return fmt.Sprintf("%s[%d]", path, i)
}

func decodeBounds(root map[string]any, key, path string) ([2]Point, error) {
	v, err := reqField(root, key, path)
	if err != nil {
		return [2]Point{}, err
	}
	arr, ok := v.([]any)
	if !ok || len(arr) != 2 {
		return [2]Point{}, decodeError("", join(path, key), "must be an array of two points")
	}
	var pts [2]Point
	for i := 0; i < 2; i++ {
		pm, ok := arr[i].(map[string]any)
		if !ok {
			return [2]Point{}, decodeError("", fmt.Sprintf("%s.%s[%d]", path, key, i), "point must be an object")
		}
		p, err := pointFromMapMM(pm, fmt.Sprintf("%s.%s[%d]", path, key, i), "")
		if err != nil {
			return [2]Point{}, err
		}
		pts[i] = p
	}
	return pts, nil
}

func pointFromMapMM(m map[string]any, path, objectID string) (Point, error) {
	x, err := reqIntField(m, "x_mm", path, objectID)
	if err != nil {
		// v2-style x/y is accepted by callers that know the schema; this
		// helper intentionally only handles the _mm dialect.
		return Point{}, err
	}
	y, err := reqIntField(m, "y_mm", path, objectID)
	if err != nil {
		return Point{}, err
	}
	ux, err := MMToUnits(objectID, join(path, "x_mm"), x)
	if err != nil {
		return Point{}, err
	}
	uy, err := MMToUnits(objectID, join(path, "y_mm"), y)
	if err != nil {
		return Point{}, err
	}
	return Point{X: ux, Y: uy}, nil
}

func validateNormal(n Vec2, objectID, path string) error {
	if !NormalUnitValid(n) {
		ge := decodeError(objectID, path, "normal must be unit length within ±5‰, got squared length %d", n.X*n.X+n.Y*n.Y)
		ge.Measured = fmt.Sprintf("normSq=%d", n.X*n.X+n.Y*n.Y)
		return ge
	}
	return nil
}
