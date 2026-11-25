package schema

import (
	"encoding/json"

	"mvccworkbench/internal/domain"
)

type rawHeader struct {
	SchemaVersion      string `json:"schema_version"`
	SchemaVersionCamel string `json:"schemaVersion"`
	SchemaVersionKebab string `json:"schema-version"`
	Schema             string `json:"schema"`
}

// LoadDocument detects one of the three supported external editions and maps
// it to the canonical model. The source project bytes are never mutated.
func LoadDocument(data []byte) (*domain.Project, string, error) {
	var header rawHeader
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, "", domain.NewError(domain.ESchemaUnknown, "document is not valid JSON").
			At("document").With("decode_error", err.Error())
	}
	version := header.SchemaVersion
	if version == "" {
		version = header.SchemaVersionCamel
	}
	if version == "" {
		version = header.SchemaVersionKebab
	}
	if version == "" {
		version = header.Schema
	}
	if version == "" {
		return nil, "", domain.NewError(domain.ESchemaUnknown, "schema version is missing").
			At("schema_version")
	}
	project, err := Load(version, data)
	if err != nil {
		return nil, version, err
	}
	return project, version, nil
}

func Load(version string, data []byte) (*domain.Project, error) {
	switch version {
	case domain.SchemaV1:
		project, err := loadV1(data)
		if err != nil {
			return nil, err
		}
		project.Canonicalize()
		return project, nil
	case domain.SchemaV2:
		project, err := loadV2(data)
		if err != nil {
			return nil, err
		}
		project.Canonicalize()
		return project, nil
	case domain.SchemaV3:
		project, err := loadV3(data)
		if err != nil {
			return nil, err
		}
		project.Canonicalize()
		return project, nil
	default:
		return nil, domain.NewError(domain.ESchemaUnknown, "unrecognized schema version or feature combination").
			At("schema_version").With("schema_version", version).
			With("supported", []string{domain.SchemaV1, domain.SchemaV2, domain.SchemaV3})
	}
}

func Encode(version string, project *domain.Project) ([]byte, error) {
	switch version {
	case domain.SchemaV1:
		return EncodeV1(project)
	case domain.SchemaV2:
		return EncodeV2(project)
	case domain.SchemaV3:
		return EncodeV3(project)
	default:
		return nil, domain.NewError(domain.ESchemaUnknown, "cannot encode unrecognized schema version").
			At("schema_version").With("schema_version", version)
	}
}

// CanonicalJSON is useful for checkpoint diagnostics and deep-equality tests.
func CanonicalJSON(project *domain.Project) ([]byte, error) {
	return json.Marshal(project)
}
