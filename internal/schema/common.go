package schema

import (
	"encoding/json"
	"fmt"

	"lip-pipeline-prototype/internal/domain"
)

const ShadowKey = "x_conversion_shadow"

type Shadow struct {
	Affinities     map[string]int    `json:"affinities,omitempty"`
	EncodingGroups map[string]string `json:"encoding_groups,omitempty"`
	ParamVersion   string            `json:"param_version,omitempty"`
}

type LoadResult struct {
	Project *domain.Project   `json:"project"`
	Report  domain.LoadReport `json:"report"`
	Shadow  *Shadow           `json:"shadow,omitempty"`
}

func schemaError(path, msg string) *domain.Error {
	return &domain.Error{Code: domain.CodeSchema, Message: msg, Path: path}
}

func decodeMap(data []byte) (map[string]json.RawMessage, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, schemaError("$", "invalid JSON document: "+err.Error())
	}
	if root == nil {
		return nil, schemaError("$", "project document must be a JSON object")
	}
	return root, nil
}

func requireField(root map[string]json.RawMessage, path, name string) (json.RawMessage, error) {
	raw, ok := root[name]
	if !ok || string(raw) == "null" {
		return nil, schemaError(path+"."+name, "required field is missing")
	}
	return raw, nil
}

func decodeObject(raw json.RawMessage, path string) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, schemaError(path, "expected object")
	}
	return m, nil
}

func decodeArray(raw json.RawMessage, path string) ([]json.RawMessage, error) {
	var a []json.RawMessage
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, schemaError(path, "expected array")
	}
	return a, nil
}

func requireString(m map[string]json.RawMessage, path, name string) (string, error) {
	raw, err := requireField(m, path, name)
	if err != nil {
		return "", err
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || value == "" {
		return "", schemaError(path+"."+name, "expected non-empty string")
	}
	return value, nil
}

func requireInt(m map[string]json.RawMessage, path, name string) (int64, error) {
	raw, err := requireField(m, path, name)
	if err != nil {
		return 0, err
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, schemaError(path+"."+name, "expected integer")
	}
	return value, nil
}

func optionalInt(m map[string]json.RawMessage, name string, def int) int {
	raw, ok := m[name]
	if !ok {
		return def
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		return def
	}
	return value
}

func parseShadow(root map[string]json.RawMessage) *Shadow {
	raw, ok := root[ShadowKey]
	if !ok {
		raw, ok = root["x-conversion-shadow"]
	}
	if !ok {
		return nil
	}
	var shadow Shadow
	if err := json.Unmarshal(raw, &shadow); err != nil {
		return nil
	}
	return &shadow
}

func LoadDocument(data []byte) (*LoadResult, error) {
	root, err := decodeMap(data)
	if err != nil {
		return nil, err
	}
	versionRaw, err := requireField(root, "$", "schema")
	if err != nil {
		return nil, err
	}
	var version string
	if err := json.Unmarshal(versionRaw, &version); err != nil {
		return nil, schemaError("$.schema", "schema must be a string")
	}
	switch domain.SchemaVersion(version) {
	case domain.SchemaV1:
		return loadV1(data, root)
	case domain.SchemaV2:
		return loadV2(data, root)
	case domain.SchemaV3:
		return loadV3(data, root)
	default:
		return nil, schemaError("$.schema", fmt.Sprintf("unsupported schema version %q", version))
	}
}

func fill(report *domain.LoadReport, path string, old any, def any, reason string) {
	report.Fills = append(report.Fills, domain.FillRecord{
		Path: path, OldValue: old, DefaultValue: def, Reason: reason, Version: report.Version,
	})
}

func applyShadow(groups map[string]string, affinities map[string]int, shadow *Shadow) {
	if shadow == nil {
		return
	}
	for id, group := range shadow.EncodingGroups {
		groups[id] = group
	}
	for id, affinity := range shadow.Affinities {
		affinities[id] = affinity
	}
}
