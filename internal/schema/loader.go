package schema

import (
	"encoding/json"
	"os"

	"maintenanceorchestrator/internal/domain"
)

type versionHeader struct {
	Schema string `json:"schema"`
}

func LoadFile(path string) (*domain.Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, domain.NewError(domain.ESchemaVersionUnsupported, path, "read schema file: %v", err)
	}
	return Load(data)
}

func Load(data []byte) (*domain.Project, error) {
	var header versionHeader
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, domain.NewError(domain.ESchemaUnknownFieldCombo, "$", "invalid JSON envelope: %v", err)
	}
	switch header.Schema {
	case domain.SchemaV1:
		return loadV1(data)
	case domain.SchemaV2:
		return loadV2(data)
	case domain.SchemaV3:
		return loadV3(data)
	case "":
		return nil, domain.NewError(domain.ESchemaVersionUnsupported, "$.schema", "missing schema version")
	default:
		return nil, domain.NewError(domain.ESchemaVersionUnsupported, "$.schema", "unsupported schema %q", header.Schema)
	}
}

func Encode(p domain.Project, version string) ([]byte, error) {
	switch version {
	case domain.SchemaV1:
		return EncodeV1(p)
	case domain.SchemaV2:
		return EncodeV2(p)
	case domain.SchemaV3:
		return EncodeV3(p)
	default:
		return nil, domain.NewError(domain.ESchemaVersionUnsupported, "$.schema", "unsupported target schema %q", version)
	}
}
