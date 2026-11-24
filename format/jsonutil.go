package format

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"economycore/model"
)

type jsonObject map[string]json.RawMessage

func decodeObject(data []byte, path string) (jsonObject, error) {
	var out jsonObject
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, model.NewError(model.ESchema, path, "invalid JSON: "+err.Error())
	}
	if out == nil {
		return nil, model.NewError(model.ESchema, path, "expected JSON object")
	}
	return out, nil
}

func (o jsonObject) has(key string) bool {
	raw, ok := o[key]
	return ok && string(raw) != "null"
}

func (o jsonObject) required(key, path string) (json.RawMessage, error) {
	raw, ok := o[key]
	if !ok || string(raw) == "null" {
		return nil, model.NewError(model.ESchema, model.JSONPointer(trimPath(path), key), "missing required field")
	}
	return raw, nil
}

func (o jsonObject) child(key, path string) (jsonObject, bool, error) {
	raw, ok := o[key]
	if !ok || string(raw) == "null" {
		return nil, false, nil
	}
	child, err := decodeObject(raw, model.JSONPointer(trimPath(path), key))
	if err != nil {
		return nil, true, err
	}
	return child, true, nil
}

func (o jsonObject) requiredChild(key, path string) (jsonObject, error) {
	raw, err := o.required(key, path)
	if err != nil {
		return nil, err
	}
	return decodeObject(raw, model.JSONPointer(trimPath(path), key))
}

func (o jsonObject) array(key, path string) ([]json.RawMessage, bool, error) {
	raw, ok := o[key]
	if !ok || string(raw) == "null" {
		return nil, false, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, true, model.NewError(model.ESchema, model.JSONPointer(trimPath(path), key), "expected array")
	}
	return items, true, nil
}

func (o jsonObject) requiredArray(key, path string) ([]json.RawMessage, error) {
	items, ok, err := o.array(key, path)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, model.NewError(model.ESchema, model.JSONPointer(trimPath(path), key), "missing required array")
	}
	return items, nil
}

func (o jsonObject) string(key, path string, required bool) (string, error) {
	fieldPath := model.JSONPointer(trimPath(path), key)
	raw, ok := o[key]
	if !ok || string(raw) == "null" {
		if required {
			return "", model.NewError(model.ESchema, fieldPath, "missing required string")
		}
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", model.NewError(model.ESchema, fieldPath, "expected string")
	}
	return value, nil
}

func (o jsonObject) requiredString(key, path string) (string, error) {
	return o.string(key, path, true)
}

func (o jsonObject) integer(key, path string, required bool) (int64, bool, error) {
	fieldPath := model.JSONPointer(trimPath(path), key)
	raw, ok := o[key]
	if !ok || string(raw) == "null" {
		if required {
			return 0, false, model.NewError(model.ESchema, fieldPath, "missing required integer")
		}
		return 0, false, nil
	}
	value, err := parseInt(raw)
	if err != nil {
		return 0, true, model.NewError(model.ESchema, fieldPath, "expected integer")
	}
	return value, true, nil
}

func (o jsonObject) requiredInt(key, path string) (int64, error) {
	value, _, err := o.integer(key, path, true)
	return value, err
}

func (o jsonObject) boolean(key, path string, required bool) (bool, bool, error) {
	fieldPath := model.JSONPointer(trimPath(path), key)
	raw, ok := o[key]
	if !ok || string(raw) == "null" {
		if required {
			return false, false, model.NewError(model.ESchema, fieldPath, "missing required boolean")
		}
		return false, false, nil
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, true, model.NewError(model.ESchema, fieldPath, "expected boolean")
	}
	return value, true, nil
}

func (o jsonObject) requiredBool(key, path string) (bool, error) {
	value, _, err := o.boolean(key, path, true)
	return value, err
}

func parseInt(raw json.RawMessage) (int64, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var number json.Number
	if err := decoder.Decode(&number); err != nil {
		return 0, err
	}
	return strconv.ParseInt(number.String(), 10, 64)
}

func decodeIntMap(raw json.RawMessage, path string) (map[string]int64, error) {
	object, err := decodeObject(raw, path)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(object))
	for key, value := range object {
		parsed, err := parseInt(value)
		if err != nil {
			return nil, model.NewError(model.ESchema, model.JSONPointer(trimPath(path), key), "expected integer")
		}
		out[key] = parsed
	}
	return out, nil
}

func optionalIntMap(o jsonObject, key, path string) (map[string]int64, error) {
	raw, ok := o[key]
	if !ok || string(raw) == "null" {
		return map[string]int64{}, nil
	}
	return decodeIntMap(raw, model.JSONPointer(trimPath(path), key))
}

func requiredIntMap(o jsonObject, key, path string) (map[string]int64, error) {
	raw, err := o.required(key, path)
	if err != nil {
		return nil, err
	}
	return decodeIntMap(raw, model.JSONPointer(trimPath(path), key))
}

func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func trimPath(path string) string {
	if path == "/" {
		return ""
	}
	return path
}

func arrayItem(items []json.RawMessage, index int, path string) (jsonObject, error) {
	itemPath := model.JSONPointer(trimPath(path), fmt.Sprint(index))
	return decodeObject(items[index], itemPath)
}
