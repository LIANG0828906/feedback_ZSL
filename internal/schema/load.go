package schema

import (
	"encoding/json"

	"presentkernel/internal/domain"
)

func LoadProject(data []byte) (*LoadResult, error) {
	version, err := DetectVersion(data)
	if err != nil {
		return nil, err
	}
	switch version {
	case domain.SchemaV1:
		return loadV1(data)
	case domain.SchemaV2:
		return loadV2(data)
	case domain.SchemaV3:
		return loadV3(data)
	default:
		return nil, domain.SchemaError("schemaVersion", "unsupported schema version %q", version)
	}
}

func DetectVersion(data []byte) (string, error) {
	var head map[string]json.RawMessage
	if err := json.Unmarshal(data, &head); err != nil {
		return "", domain.SchemaError("$", "invalid JSON: %v", err)
	}
	for _, key := range []string{"schema_version", "schemaVersion"} {
		if raw, ok := head[key]; ok {
			var v string
			if err := json.Unmarshal(raw, &v); err != nil {
				return "", domain.SchemaError(key, "schema version must be a string")
			}
			if v == "" {
				return "", domain.SchemaError(key, "schema version is required")
			}
			return v, nil
		}
	}
	return "", domain.SchemaError("schemaVersion", "schema version field is required")
}

func validateTextWeights(path string, text string, count int) error {
	runeCount := len([]rune(text))
	if runeCount == 0 {
		return domain.SchemaError(path+".text", "text is required")
	}
	if count == 0 {
		return domain.SchemaError(path+".character_weights", "at least one character weight is required")
	}
	if count != runeCount {
		return domain.SchemaError(path+".character_weights", "weight count %d does not match text rune count %d", count, runeCount)
	}
	return nil
}

func categoriesOrStandard(categories []domain.Category) []domain.Category {
	if len(categories) == 0 {
		return domain.StandardCategories()
	}
	haveBridge := false
	haveSilence := false
	for _, c := range categories {
		if c.Code == "TR" {
			haveBridge = true
		}
		if c.Code == "SIL" {
			haveSilence = true
		}
	}
	if haveBridge && haveSilence {
		return categories
	}
	seen := map[string]bool{}
	for _, c := range categories {
		seen[c.Code] = true
	}
	for _, c := range domain.StandardCategories() {
		if (c.Code == "TR" && !haveBridge) || (c.Code == "SIL" && !haveSilence) {
			if !seen[c.Code] {
				categories = append(categories, c)
			}
		}
	}
	return categories
}
