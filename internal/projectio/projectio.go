package projectio

import (
	"encoding/json"
	"sort"

	"mirrorcast/internal/bizerror"
	"mirrorcast/internal/session"
)

type schemaHead struct {
	Schema        string `json:"schema"`
	SchemaVersion string `json:"schemaVersion"`
}

func Load(data []byte) (*session.Project, error) {
	var head schemaHead
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, bizerror.New("E_JSON_INVALID", "cannot detect schema version: "+err.Error())
	}
	switch head.Schema {
	case SchemaV1Wire:
		return loadV1(data)
	case SchemaV2Wire:
		return loadV2(data)
	}
	switch head.SchemaVersion {
	case SchemaV3Wire:
		return loadV3(data)
	}
	return nil, bizerror.New(bizerror.ESchemaUnsupported, "missing or unsupported schema identifier").
		WithDetail("schema", head.Schema).WithDetail("schemaVersion", head.SchemaVersion).
		WithDetail("supported", []string{SchemaV1Wire, SchemaV2Wire, SchemaV3Wire})
}

func LoadVersion(data []byte, version string) (*session.Project, error) {
	switch version {
	case session.SchemaV1:
		return loadV1(data)
	case session.SchemaV2:
		return loadV2(data)
	case session.SchemaV3:
		return loadV3(data)
	default:
		return nil, bizerror.New(bizerror.ESchemaUnsupported, "unknown requested schema version").WithDetail("version", version)
	}
}

func Write(project session.Project, version string) ([]byte, error) {
	switch version {
	case session.SchemaV1:
		return WriteV1(project)
	case session.SchemaV2:
		return WriteV2(project)
	case session.SchemaV3:
		return WriteV3(project)
	default:
		return nil, bizerror.New(bizerror.ESchemaUnsupported, "unknown requested schema version").WithDetail("version", version)
	}
}

func WriteTruth(truth session.TruthFile) ([]byte, error) {
	truth.SchemaVersion = TruthWire
	if truth.Tolerances == (session.Tolerances{}) {
		truth.Tolerances = session.DeclaredTolerances()
	}
	return json.MarshalIndent(truth, "", "  ")
}

func NormalizeLoaded(project *session.Project) error {
	sort.SliceStable(project.Screens, func(i, j int) bool { return project.Screens[i].ID < project.Screens[j].ID })
	sort.SliceStable(project.Events, func(i, j int) bool { return project.Events[i].Tick < project.Events[j].Tick })
	return session.ValidateProject(project)
}
