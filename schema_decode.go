package maskqc

import (
	"encoding/json"
	"os"
)

type rawObject map[string]json.RawMessage

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

func decodeObject(data []byte, path string) (rawObject, error) {
	var obj rawObject
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, schemaError(path, "expected object: %v", err)
	}
	if obj == nil {
		return nil, schemaError(path, "required object is missing or null")
	}
	return obj, nil
}

func (o rawObject) object(path, key string) (rawObject, error) {
	raw, ok := o[key]
	if !ok || string(raw) == "null" {
		return nil, schemaError(path+"."+key, "missing required field")
	}
	obj, err := decodeObject(raw, path+"."+key)
	if err != nil {
		return nil, err
	}
	return obj, nil
}

func (o rawObject) array(path, key string) ([]json.RawMessage, error) {
	raw, ok := o[key]
	if !ok || string(raw) == "null" {
		return nil, schemaError(path+"."+key, "missing required field %q", key)
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, schemaError(path+"."+key, "expected array: %v", err)
	}
	if arr == nil {
		return nil, schemaError(path+"."+key, "field must be an array")
	}
	return arr, nil
}

func (o rawObject) string(path, key string) (string, error) {
	raw, ok := o[key]
	if !ok || string(raw) == "null" {
		return "", schemaError(path+"."+key, "missing required string field")
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", schemaError(path+"."+key, "expected string: %v", err)
	}
	if v == "" {
		return "", schemaError(path+"."+key, "string must not be empty")
	}
	return v, nil
}

func (o rawObject) int(path, key string) (int, error) {
	raw, ok := o[key]
	if !ok || string(raw) == "null" {
		return 0, schemaError(path+"."+key, "missing required integer field")
	}
	var v int
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, schemaError(path+"."+key, "expected integer: %v", err)
	}
	return v, nil
}

func (o rawObject) int64Value(path, key string) (int64, error) {
	raw, ok := o[key]
	if !ok || string(raw) == "null" {
		return 0, schemaError(path+"."+key, "missing required integer field")
	}
	var v int64
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, schemaError(path+"."+key, "expected integer: %v", err)
	}
	return v, nil
}

func (o rawObject) optionalInt(key string, def int) int {
	raw, ok := o[key]
	if !ok || string(raw) == "null" {
		return def
	}
	var v int
	if json.Unmarshal(raw, &v) != nil {
		return def
	}
	return v
}

func decodeIntPair(raw json.RawMessage, path string) (Point, error) {
	var pair []int
	if err := json.Unmarshal(raw, &pair); err != nil || len(pair) != 2 {
		var obj struct {
			X int `json:"x"`
			Y int `json:"y"`
		}
		if err2 := json.Unmarshal(raw, &obj); err2 != nil {
			return Point{}, schemaError(path, "expected [x,y] or {x,y} integer pair")
		}
		return Point{X: obj.X, Y: obj.Y}, nil
	}
	return Point{X: pair[0], Y: pair[1]}, nil
}

func decodeBBox(raw json.RawMessage, path string) (BBox, error) {
	var arr []int
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) == 4 {
		return BBox{MinX: arr[0], MinY: arr[1], MaxX: arr[2], MaxY: arr[3]}, nil
	}
	var b BBox
	if err := json.Unmarshal(raw, &b); err != nil {
		return BBox{}, schemaError(path, "expected [minX,minY,maxX,maxY] or bbox object")
	}
	return b, nil
}
