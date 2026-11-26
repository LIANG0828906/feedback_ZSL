package schema

import (
	"encoding/json"
	"fmt"
	"os"

	"maskqc/model"
)

const RoundNearestHalfUp = "nearest_half_up"

func DetectVersion(data []byte) (model.SchemaVersion, string, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return "", "", model.NewError(model.CodeSchema, "$", "", "invalid JSON: "+err.Error())
	}
	objectID := ""
	if raw, ok := root["project_id"]; ok {
		_ = json.Unmarshal(raw, &objectID)
	}
	if raw, ok := root["object"]; ok {
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) == nil {
			if idRaw, ok := obj["id"]; ok {
				_ = json.Unmarshal(idRaw, &objectID)
			}
		}
	}
	if raw, ok := root["project"]; ok {
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) == nil {
			if idRaw, ok := obj["id"]; ok {
				_ = json.Unmarshal(idRaw, &objectID)
			}
		}
	}
	for _, key := range []string{"schema", "schema_version", "schemaVersion"} {
		if raw, ok := root[key]; ok {
			var value any
			if err := json.Unmarshal(raw, &value); err != nil {
				continue
			}
			var text string
			switch typed := value.(type) {
			case string:
				text = typed
			case float64:
				text = fmt.Sprintf("%.0f", typed)
			default:
				text = fmt.Sprint(typed)
			}
			version, valid := model.ParseSchemaVersion(text)
			if !valid {
				return "", objectID, model.NewError(model.CodeSchema, "$."+key, objectID, "unknown schema version "+text)
			}
			return version, objectID, nil
		}
	}
	return "", objectID, model.NewError(model.CodeSchema, "$.schema|$.schema_version|$.schemaVersion", objectID, "schema version marker is missing")
}

func LoadFile(path string) (*model.Project, *model.ConversionReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	return Load(data)
}

func Load(data []byte) (*model.Project, *model.ConversionReport, error) {
	version, objectID, err := DetectVersion(data)
	if err != nil {
		return nil, nil, err
	}
	root, err := parseObject(data, "$", objectID)
	if err != nil {
		return nil, nil, err
	}
	report := &model.ConversionReport{}
	var project *model.Project
	var problems model.Errors
	switch version {
	case model.SchemaV1:
		project, problems = loadV1(root, objectID, report)
	case model.SchemaV2:
		project, problems = loadV2(root, objectID, report)
	case model.SchemaV3:
		project, problems = loadV3(root, objectID, report)
	default:
		return nil, nil, model.NewError(model.CodeSchema, "$.schema", objectID, "unsupported schema")
	}
	if project != nil {
		problems = append(problems, validateProject(project)...)
	}
	report.Finish()
	if len(problems) > 0 {
		return nil, report, problems
	}
	return project, report, nil
}

func parseObject(data []byte, path, objectID string) (object, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, model.NewError(model.CodeSchema, path, objectID, "expected object: "+err.Error())
	}
	return object(raw), nil
}

type object map[string]json.RawMessage

func (o object) has(key string) bool {
	_, ok := o[key]
	return ok
}

func (o object) raw(key string) json.RawMessage {
	return o[key]
}

func (o object) stringField(key, path, objectID string, required bool) (string, *model.Error) {
	raw, ok := o[key]
	if !ok {
		if required {
			return "", missing(path+"."+key, objectID)
		}
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", typeError(path+"."+key, objectID, "string")
	}
	if required && value == "" {
		return "", missing(path+"."+key, objectID)
	}
	return value, nil
}

func (o object) versionField(key, path, objectID string) (string, *model.Error) {
	raw, ok := o[key]
	if !ok {
		return "", missing(path+"."+key, objectID)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", typeError(path+"."+key, objectID, "string or integer")
	}
	switch typed := value.(type) {
	case string:
		return typed, nil
	case float64:
		if typed == float64(int64(typed)) {
			return fmt.Sprintf("%.0f", typed), nil
		}
	}
	return "", typeError(path+"."+key, objectID, "string or integer")
}

func (o object) intField(key, path, objectID string, required bool) (int64, bool, *model.Error) {
	raw, ok := o[key]
	if !ok {
		if required {
			return 0, false, missing(path+"."+key, objectID)
		}
		return 0, false, nil
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false, typeError(path+"."+key, objectID, "integer")
	}
	return value, true, nil
}

func (o object) floatField(key, path, objectID string, required bool) (float64, bool, *model.Error) {
	raw, ok := o[key]
	if !ok {
		if required {
			return 0, false, missing(path+"."+key, objectID)
		}
		return 0, false, nil
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false, typeError(path+"."+key, objectID, "number")
	}
	return value, true, nil
}

func (o object) objectField(key, path, objectID string, required bool) (object, *model.Error) {
	raw, ok := o[key]
	if !ok {
		if required {
			return nil, missing(path+"."+key, objectID)
		}
		return nil, nil
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(raw, &nested); err != nil {
		return nil, typeError(path+"."+key, objectID, "object")
	}
	return object(nested), nil
}

func (o object) arrayField(key, path, objectID string, required bool) ([]json.RawMessage, *model.Error) {
	raw, ok := o[key]
	if !ok {
		if required {
			return nil, missing(path+"."+key, objectID)
		}
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, typeError(path+"."+key, objectID, "array")
	}
	return items, nil
}

func missing(path, objectID string) *model.Error {
	return model.NewError(model.CodeSchema, path, objectID, "required field is missing")
}

func typeError(path, objectID, expected string) *model.Error {
	return model.NewError(model.CodeSchema, path, objectID, "expected "+expected)
}

func decodeIntArray(raw json.RawMessage, length int) ([]int64, bool) {
	var values []int64
	if err := json.Unmarshal(raw, &values); err != nil || len(values) != length {
		return nil, false
	}
	return values, true
}

func decodeMask(raw json.RawMessage, path, objectID string) ([]byte, *model.Error) {
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return nil, typeError(path, objectID, "base64 string")
	}
	mask, err := decodeRawMask(encoded)
	if err != nil {
		return nil, model.NewError(model.CodeSchema, path, objectID, err.Error())
	}
	return mask, nil
}
