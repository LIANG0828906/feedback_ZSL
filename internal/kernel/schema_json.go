package kernel

import (
	"encoding/json"
	"fmt"
)

type Transcript struct {
	Fills       []Issue `json:"fills"`
	Losses      []Issue `json:"losses"`
	Conversions []Issue `json:"conversions"`
}

type LoadResult struct {
	Scene      *Scene     `json:"scene"`
	Transcript Transcript `json:"transcript"`
}

type ExportResult struct {
	SchemaVersion int        `json:"schema_version"`
	Data          []byte     `json:"-"`
	Transcript    Transcript `json:"transcript"`
}

func decodeRootObject(data []byte, sourceVersion int) (map[string]json.RawMessage, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, asError(Issue{
			Code:          CodeDecode,
			Field:         "$",
			SourceVersion: sourceVersion,
			Message:       "document is not decodable JSON: " + err.Error(),
		})
	}
	if root == nil {
		return nil, schemaError("$", sourceVersion, "root must be a JSON object")
	}
	return root, nil
}

func field(raw map[string]json.RawMessage, key, path string, version int) (json.RawMessage, error) {
	value, ok := raw[key]
	if !ok || string(value) == "null" {
		return nil, schemaError(path, version, "required field %q is missing", key)
	}
	return value, nil
}

func optionalField(raw map[string]json.RawMessage, key string) (json.RawMessage, bool) {
	value, ok := raw[key]
	if !ok || string(value) == "null" {
		return nil, false
	}
	return value, true
}

func asObject(raw json.RawMessage, path string, version int) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, schemaError(path, version, "field must be an object")
	}
	return object, nil
}

func asArray(raw json.RawMessage, path string, version int) ([]json.RawMessage, error) {
	var array []json.RawMessage
	if err := json.Unmarshal(raw, &array); err != nil {
		return nil, schemaError(path, version, "field must be an array")
	}
	return array, nil
}

func asString(raw json.RawMessage, path string, version int) (string, error) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", schemaError(path, version, "field must be a string")
	}
	if value == "" {
		return "", schemaError(path, version, "string must not be empty")
	}
	return value, nil
}

func asInt(raw json.RawMessage, path string, version int) (int64, error) {
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, schemaError(path, version, "field must be an integer")
	}
	return value, nil
}

func asBool(raw json.RawMessage, path string, version int) (bool, error) {
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, schemaError(path, version, "field must be a boolean")
	}
	return value, nil
}

func requiredString(obj map[string]json.RawMessage, key, path string, version int) (string, error) {
	raw, err := field(obj, key, path, version)
	if err != nil {
		return "", err
	}
	return asString(raw, path, version)
}

func requiredInt(obj map[string]json.RawMessage, key, path string, version int) (int64, error) {
	raw, err := field(obj, key, path, version)
	if err != nil {
		return 0, err
	}
	return asInt(raw, path, version)
}

func requiredObject(obj map[string]json.RawMessage, key, path string, version int) (map[string]json.RawMessage, error) {
	raw, err := field(obj, key, path+"."+key, version)
	if err != nil {
		return nil, err
	}
	return asObject(raw, path+"."+key, version)
}

func requiredArray(obj map[string]json.RawMessage, key, path string, version int) ([]json.RawMessage, error) {
	raw, err := field(obj, key, path+"."+key, version)
	if err != nil {
		return nil, err
	}
	return asArray(raw, path+"."+key, version)
}

func requiredXY(obj map[string]json.RawMessage, path string, version int) (Vec2, error) {
	x, err := requiredInt(obj, "x", path+".x", version)
	if err != nil {
		return Vec2{}, err
	}
	y, err := requiredInt(obj, "y", path+".y", version)
	if err != nil {
		return Vec2{}, err
	}
	return Vec2{X: x, Y: y}, nil
}

func scaledWorldXY(world Vec2, path, id string, version int) (Vec2, error) {
	x, err := ScaleWorld(world.X, path+".x", id, version)
	if err != nil {
		return Vec2{}, err
	}
	y, err := ScaleWorld(world.Y, path+".y", id, version)
	if err != nil {
		return Vec2{}, err
	}
	return Vec2{X: x, Y: y}, nil
}

func fillTrace(fieldPath, defaultValue, sourceDefault string, version int) Issue {
	return Issue{
		Code:          ErrorCode("FILL_DEFAULT"),
		Field:         fieldPath,
		SourceVersion: version,
		OldValue:      "missing",
		DefaultValue:  defaultValue,
		Disposition:   sourceDefault,
		Message:       fmt.Sprintf("filled missing field with declared default %s", defaultValue),
	}
}

func conversionTrace(fieldPath, oldValue, newValue, disposition string, version int) Issue {
	return Issue{
		Code:          ErrorCode("CONVERSION"),
		Field:         fieldPath,
		SourceVersion: version,
		OldValue:      oldValue,
		DefaultValue:  newValue,
		Disposition:   disposition,
		Message:       disposition,
	}
}
