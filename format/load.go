// Package format loads and exports the supported v1, v2 and v3 economy
// schemas and implements explicit conversion traces and loss reports.
package format

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"economycore/model"
)

// Load detects the schema version and maps the document into the canonical
// internal model.
func Load(data []byte) (*model.Model, error) {
	root, err := decodeObject(data, "")
	if err != nil {
		return nil, err
	}
	version, path, err := detectVersion(root)
	if err != nil {
		return nil, err
	}
	var loaded *model.Model
	switch version {
	case model.SchemaV1:
		loaded, err = loadV1(root)
	case model.SchemaV2:
		loaded, err = loadV2(root)
	case model.SchemaV3:
		loaded, err = loadV3(root)
	default:
		return nil, model.NewError(model.EVer, path, "unsupported schema version "+version)
	}
	if err != nil {
		return nil, err
	}
	loaded.SourceVersion = version
	loaded.RateScale = 10000
	loaded.WorkerScale = 10000
	if loaded.ConversionVersion == "" {
		loaded.ConversionVersion = model.DefaultConversionVersion
	}
	if loaded.RoundMode == "" {
		loaded.RoundMode = "floor"
	}
	loaded.Normalize()
	if err := loaded.Validate(); err != nil {
		return nil, err
	}
	return loaded, nil
}

// LoadFile reads and loads a project document.
func LoadFile(path string) (*model.Model, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, model.NewError(model.EIO, path, err.Error()).WithCause(err)
	}
	return Load(data)
}

func detectVersion(root jsonObject) (string, string, error) {
	type candidate struct {
		key   string
		value string
	}
	var found []candidate
	if root.has("schema_version") {
		value, err := root.string("schema_version", "", true)
		if err != nil {
			return "", "", err
		}
		found = append(found, candidate{"schema_version", value})
	}
	if root.has("formatVersion") {
		value, err := root.string("formatVersion", "", true)
		if err != nil {
			return "", "", err
		}
		found = append(found, candidate{"formatVersion", value})
	}
	if root.has("specVersion") {
		raw := root["specVersion"]
		var value string
		if err := json.Unmarshal(raw, &value); err == nil {
			found = append(found, candidate{"specVersion", value})
		} else {
			number, numberErr := parseInt(raw)
			if numberErr != nil {
				return "", "", model.NewError(model.ESchema, "/specVersion", "expected version string or integer")
			}
			found = append(found, candidate{"specVersion", fmt.Sprint(number)})
		}
	}
	if len(found) == 0 {
		return "", "", model.NewError(model.ESchema, "/version", "missing schema version field")
	}
	for _, item := range found {
		value := item.value
		path := model.JSONPointer(item.key)
		switch {
		case item.key == "schema_version" && value == "economy-v1":
			return model.SchemaV1, path, nil
		case item.key == "formatVersion" && value == "v2":
			return model.SchemaV2, path, nil
		case item.key == "specVersion" && (value == "v3" || value == "3"):
			return model.SchemaV3, path, nil
		}
	}
	first := found[0]
	return "", model.JSONPointer(first.key), model.NewError(model.EVer, model.JSONPointer(first.key), "unsupported schema version "+first.value)
}

func parseFormula(raw json.RawMessage, path string) (model.Expr, error) {
	var expression model.Expr
	if err := json.Unmarshal(raw, &expression); err != nil {
		return model.Expr{}, model.NewError(model.ESchema, path, "invalid expression tree: "+err.Error())
	}
	if err := expression.Validate(); err != nil {
		return model.Expr{}, model.NewError(model.ESchema, path, err.Error())
	}
	return expression, nil
}

func resourceKindFromCode(code int64, path string) (model.ResourceKind, error) {
	switch code {
	case 0:
		return model.KindRaw, nil
	case 1:
		return model.KindCrafted, nil
	case 2:
		return model.KindFood, nil
	case 3:
		return model.KindEnergy, nil
	default:
		return "", model.NewError(model.ESchema, path, "unknown resource type code")
	}
}

func resourceKindFromString(value, path string) (model.ResourceKind, error) {
	kind := model.ResourceKind(strings.ToLower(value))
	switch kind {
	case model.KindRaw, model.KindCrafted, model.KindFood, model.KindEnergy:
		return kind, nil
	default:
		return "", model.NewError(model.ESchema, path, "unknown resource kind "+value)
	}
}

func codeFromResourceKind(kind model.ResourceKind) (int64, bool) {
	switch kind {
	case model.KindRaw:
		return 0, true
	case model.KindCrafted:
		return 1, true
	case model.KindFood:
		return 2, true
	case model.KindEnergy:
		return 3, true
	default:
		return 0, false
	}
}

func readRoundMode(root jsonObject, path string) (string, error) {
	value, err := root.string("rounding", path, false)
	if err != nil {
		return "", err
	}
	if value == "" {
		return "floor", nil
	}
	switch value {
	case "floor", "ceil", "nearest":
		return value, nil
	default:
		return "", model.NewError(model.ESchema, model.JSONPointer(trimPath(path), "rounding"), "unsupported rounding mode")
	}
}
