package acoustic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

func LoadProjectFile(path string) (*Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &Error{Code: CodeSchema, Op: "load", Message: "read project", Cause: err.Error()}
	}
	return LoadProject(data)
}

func LoadProject(data []byte) (*Project, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, &Error{Code: CodeSchema, Op: "load", Message: "empty project document"}
	}

	top, err := objectFields(data)
	if err != nil {
		return nil, schemaJSONError("load", "$", err)
	}
	topRequired := []string{
		"schemaVersion", "paramsVersion", "algorithmVersion", "rooms", "walls",
		"openings", "sources", "reflectors", "listeners", "tolerance", "truth",
	}
	if err := requireFields(top, "$", topRequired); err != nil {
		return nil, err
	}

	if err := requireArrayObjects(top["rooms"], "$.rooms", []string{"id", "rect"}); err != nil {
		return nil, err
	}
	if err := requireNestedRect(top["rooms"], "$.rooms", "rect", []string{"minX", "minY", "maxX", "maxY"}); err != nil {
		return nil, err
	}
	if err := requireArrayObjects(top["walls"], "$.walls", []string{"id", "roomA", "roomB", "a", "b"}); err != nil {
		return nil, err
	}
	if err := requireNestedPoint(top["walls"], "$.walls", "a"); err != nil {
		return nil, err
	}
	if err := requireNestedPoint(top["walls"], "$.walls", "b"); err != nil {
		return nil, err
	}
	if err := requireArrayObjects(top["openings"], "$.openings", []string{"id", "wallId", "start", "width"}); err != nil {
		return nil, err
	}
	if err := requireArrayObjects(top["sources"], "$.sources", []string{"id", "roomId", "pos", "levelDb"}); err != nil {
		return nil, err
	}
	if err := requireNestedPoint(top["sources"], "$.sources", "pos"); err != nil {
		return nil, err
	}
	if err := requireArrayObjects(top["reflectors"], "$.reflectors", []string{"id", "roomId", "a", "b", "nx", "ny", "absorption"}); err != nil {
		return nil, err
	}
	if err := requireNestedPoint(top["reflectors"], "$.reflectors", "a"); err != nil {
		return nil, err
	}
	if err := requireNestedPoint(top["reflectors"], "$.reflectors", "b"); err != nil {
		return nil, err
	}
	if err := requireArrayObjects(top["listeners"], "$.listeners", []string{"id", "roomId", "pos"}); err != nil {
		return nil, err
	}
	if err := requireNestedPoint(top["listeners"], "$.listeners", "pos"); err != nil {
		return nil, err
	}
	if err := requireObject(top["tolerance"], "$.tolerance", []string{"coordinateTicks", "pathTicks", "incidenceCos", "splDb"}); err != nil {
		return nil, err
	}
	if err := requireArrayObjects(top["truth"], "$.truth", []string{"listenerId", "roomId", "validPathCount", "sources"}); err != nil {
		return nil, err
	}
	if err := requireTruthSources(top["truth"]); err != nil {
		return nil, err
	}

	var project Project
	if err := json.Unmarshal(data, &project); err != nil {
		return nil, schemaJSONError("load", "$", err)
	}
	if err := project.Validate(); err != nil {
		return nil, err
	}
	return &project, nil
}

func objectFields(data []byte) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, fmt.Errorf("document must be an object")
	}
	return fields, nil
}

func requireFields(fields map[string]json.RawMessage, path string, names []string) error {
	for _, name := range names {
		raw, ok := fields[name]
		if !ok || isJSONNull(raw) {
			return &Error{Code: CodeSchema, Op: "load", Field: path + "." + name, Message: "missing required field"}
		}
	}
	return nil
}

func requireObject(raw json.RawMessage, path string, names []string) error {
	fields, err := objectFields(raw)
	if err != nil {
		return schemaJSONError("load", path, err)
	}
	return requireFields(fields, path, names)
}

func requireArrayObjects(raw json.RawMessage, path string, names []string) error {
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return schemaJSONError("load", path, err)
	}
	if values == nil {
		return &Error{Code: CodeSchema, Op: "load", Field: path, Message: "array must not be null"}
	}
	for i, value := range values {
		if err := requireObject(value, fmt.Sprintf("%s[%d]", path, i), names); err != nil {
			return err
		}
	}
	return nil
}

func requireNestedPoint(raw json.RawMessage, path string, name string) error {
	var values []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return schemaJSONError("load", path, err)
	}
	for i, value := range values {
		if err := requireObject(value[name], fmt.Sprintf("%s[%d].%s", path, i, name), []string{"x", "y"}); err != nil {
			return err
		}
	}
	return nil
}

func requireNestedRect(raw json.RawMessage, path string, name string, names []string) error {
	var values []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return schemaJSONError("load", path, err)
	}
	for i, value := range values {
		if err := requireObject(value[name], fmt.Sprintf("%s[%d].%s", path, i, name), names); err != nil {
			return err
		}
	}
	return nil
}

func requireTruthSources(raw json.RawMessage) error {
	var truth []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &truth); err != nil {
		return schemaJSONError("load", "$.truth", err)
	}
	for i, listener := range truth {
		var sources []json.RawMessage
		if err := json.Unmarshal(listener["sources"], &sources); err != nil {
			return schemaJSONError("load", fmt.Sprintf("$.truth[%d].sources", i), err)
		}
		if sources == nil {
			return &Error{Code: CodeSchema, Op: "load", Field: fmt.Sprintf("$.truth[%d].sources", i), Message: "array must not be null"}
		}
		for j, sourceRaw := range sources {
			sourcePath := fmt.Sprintf("$.truth[%d].sources[%d]", i, j)
			if err := requireObject(sourceRaw, sourcePath, []string{"sourceId", "validPathCount"}); err != nil {
				return err
			}
			var source map[string]json.RawMessage
			if err := json.Unmarshal(sourceRaw, &source); err != nil {
				return schemaJSONError("load", sourcePath, err)
			}
			if rawPaths, ok := source["paths"]; ok && !isJSONNull(rawPaths) {
				var paths []json.RawMessage
				if err := json.Unmarshal(rawPaths, &paths); err != nil {
					return schemaJSONError("load", sourcePath+".paths", err)
				}
				for k, pathRaw := range paths {
					if err := requireObject(pathRaw, fmt.Sprintf("%s.paths[%d]", sourcePath, k), []string{
						"sourceId", "reflectorId", "reflectionPoint", "pathLengthTicks", "incidenceCos", "splDb",
					}); err != nil {
						return err
					}
					var path map[string]json.RawMessage
					if err := json.Unmarshal(pathRaw, &path); err != nil {
						return schemaJSONError("load", fmt.Sprintf("%s.paths[%d]", sourcePath, k), err)
					}
					if err := requireObject(path["reflectionPoint"], fmt.Sprintf("%s.paths[%d].reflectionPoint", sourcePath, k), []string{"x", "y"}); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func schemaJSONError(op, field string, err error) error {
	return &Error{Code: CodeSchema, Op: op, Field: field, Message: "invalid JSON value", Cause: err.Error()}
}
