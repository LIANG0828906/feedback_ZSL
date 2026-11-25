package loader

import (
	"encoding/json"
	"strings"

	"chaosdiag/model"
)

type versionEnvelope struct {
	SchemaVersion string `json:"schema_version"`
	APIVersion    string `json:"apiVersion"`
	APISchema     string `json:"api_schema"`
}

func Load(data []byte) (*model.Project, error) {
	var env versionEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, model.NewError(model.CodeFieldInvalid, "$", "invalid JSON: %v", err)
	}
	version := firstNonEmpty(env.SchemaVersion, env.APIVersion, env.APISchema)
	return LoadVersion(version, data)
}

func LoadVersion(version string, data []byte) (*model.Project, error) {
	normalized := normalizeVersion(version)
	switch normalized {
	case model.SchemaV1:
		var doc v1Doc
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, model.NewError(model.CodeFieldInvalid, "$", "v1 decode failed: %v", err)
		}
		p, err := convertV1(&doc)
		if err != nil {
			return nil, err
		}
		return finalize(p)
	case model.SchemaV2:
		var doc v2Doc
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, model.NewError(model.CodeFieldInvalid, "$", "v2 decode failed: %v", err)
		}
		p, err := convertV2(&doc)
		if err != nil {
			return nil, err
		}
		return finalize(p)
	case model.SchemaV3:
		var doc v3Doc
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, model.NewError(model.CodeFieldInvalid, "$", "v3 decode failed: %v", err)
		}
		p, err := convertV3(&doc)
		if err != nil {
			return nil, err
		}
		return finalize(p)
	default:
		return nil, model.NewError(model.CodeSchemaUnsupported, "schema_version", "unsupported schema %q", version)
	}
}

func finalize(p *model.Project) (*model.Project, error) {
	if p.Schema == "" {
		p.Schema = model.SchemaV1
	}
	if err := model.ValidateProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	switch v {
	case "v1", "1", "chaosdiag/v1", model.SchemaV1:
		return model.SchemaV1
	case "v2", "2", "chaosdiag/v2", model.SchemaV2:
		return model.SchemaV2
	case "v3", "3", "chaosdiag/v3", model.SchemaV3:
		return model.SchemaV3
	default:
		return v
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func intValue(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func intPtr(v int64) *int64 { return &v }
