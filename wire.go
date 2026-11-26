package bonerig

import (
	"bytes"
	"encoding/json"
	"fmt"
)

func decodeObject(data []byte) (map[string]any, *Error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		e := errorf(CodeSchema, "invalid JSON document: "+err.Error())
		e.Field = "$"
		return nil, e
	}
	m, ok := v.(map[string]any)
	if !ok {
		e := errorf(CodeSchema, "root must be an object")
		e.Field = "$"
		return nil, e
	}
	return m, nil
}

func typeError(path string, want string, got any) *Error {
	e := errorf(CodeSchema, fmt.Sprintf("expected %s", want))
	e.Field = path
	e.OpA = got
	e.OpB = want
	return e
}

func asMapAt(v any, path string) (map[string]any, *Error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, typeError(path, "object", v)
	}
	return m, nil
}

func reqMap(root map[string]any, key, path string) (map[string]any, *Error) {
	v, ok := root[key]
	if !ok || v == nil {
		e := errorf(CodeSchema, "missing required field")
		e.Field = joinPath(path, key)
		return nil, e
	}
	return asMapAt(v, joinPath(path, key))
}

func reqSlice(root map[string]any, key, path string) ([]any, *Error) {
	v, ok := root[key]
	if !ok || v == nil {
		e := errorf(CodeSchema, "missing required field")
		e.Field = joinPath(path, key)
		return nil, e
	}
	s, ok := v.([]any)
	if !ok {
		return nil, typeError(joinPath(path, key), "array", v)
	}
	return s, nil
}

func reqString(root map[string]any, key, path string) (string, *Error) {
	v, ok := root[key]
	if !ok || v == nil {
		e := errorf(CodeSchema, "missing required field")
		e.Field = joinPath(path, key)
		return "", e
	}
	s, ok := v.(string)
	if !ok {
		return "", typeError(joinPath(path, key), "string", v)
	}
	if s == "" {
		e := errorf(CodeSchema, "field must not be empty")
		e.Field = joinPath(path, key)
		return "", e
	}
	return s, nil
}

func reqInt(root map[string]any, key, path string) (int64, *Error) {
	v, ok := root[key]
	if !ok || v == nil {
		e := errorf(CodeSchema, "missing required field")
		e.Field = joinPath(path, key)
		return 0, e
	}
	return asInt(v, joinPath(path, key))
}

func optInt(root map[string]any, key string) (int64, bool, *Error) {
	v, ok := root[key]
	if !ok || v == nil {
		return 0, false, nil
	}
	n, err := asInt(v, key)
	if err != nil {
		return 0, false, err
	}
	return n, true, nil
}

func asInt(v any, path string) (int64, *Error) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err == nil {
			return i, nil
		}
		f, ferr := n.Float64()
		if ferr != nil {
			e := errorf(CodeSchema, "expected integer")
			e.Field = path
			e.OpA = n.String()
			return 0, e
		}
		return int64(f), nil
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	case float64:
		return int64(n), nil
	default:
		return 0, typeError(path, "integer", v)
	}
}

func asFloat(v any, path string) (float64, *Error) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			e := errorf(CodeSchema, "expected number")
			e.Field = path
			return 0, e
		}
		return f, nil
	case float64:
		return n, nil
	default:
		return 0, typeError(path, "number", v)
	}
}

func reqIntArray(v any, path string, n int) ([]int64, *Error) {
	arr, ok := v.([]any)
	if !ok {
		return nil, typeError(path, "integer array", v)
	}
	if n >= 0 && len(arr) != n {
		e := errorf(CodeSchema, fmt.Sprintf("expected %d components", n))
		e.Field = path
		e.OpA = len(arr)
		e.OpB = n
		return nil, e
	}
	out := make([]int64, len(arr))
	for i, item := range arr {
		n, err := asInt(item, fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return nil, err
		}
		out[i] = n
	}
	return out, nil
}

func joinPath(parent, key string) string {
	if parent == "" || parent == "$" {
		if key == "" {
			return "$"
		}
		return "$." + key
	}
	if key == "" {
		return parent
	}
	return parent + "." + key
}

func indexPath(parent string, i int) string {
	return fmt.Sprintf("%s[%d]", parent, i)
}

func marshalDoc(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func quatToScale(q Quat, scale int64) [4]int64 {
	return [4]int64{
		roundQ(q.W*scale, q.Scale),
		roundQ(q.X*scale, q.Scale),
		roundQ(q.Y*scale, q.Scale),
		roundQ(q.Z*scale, q.Scale),
	}
}

func vecToScale(v Vec3, scale int64) [3]int64 {
	return [3]int64{
		roundQ(v.X*scale, v.Scale),
		roundQ(v.Y*scale, v.Scale),
		roundQ(v.Z*scale, v.Scale),
	}
}
