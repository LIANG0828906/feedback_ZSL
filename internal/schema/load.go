package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"quotaflow/internal/domain"
)

var unitFieldNames = map[string]bool{
	"quota_percent":      true,
	"reserve_percent":    true,
	"quotaBasisPoints":   true,
	"reserveBasisPoints": true,
	"quotaBp":            true,
	"reserveBp":          true,
	"core_capacity":      true,
	"cores":              true,
	"capacityMillicores": true,
	"capacity_milli":     true,
	"millicores":         true,
	"seq":                true,
	"timestampMs":        true,
	"at":                 true,
	"schema_version":     true,
	"schema":             true,
	"version":            true,
}

func LoadFile(path string) (*domain.Project, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, domain.NewBizError(domain.ESchemaInvalid, path, "read project: %v", err)
	}
	project, err := LoadBytes(raw, path)
	if err != nil {
		return nil, err
	}
	project.SourceName = path
	return project, nil
}

func LoadBytes(raw []byte, source string) (*domain.Project, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, domain.NewBizError(domain.ESchemaInvalid, "$", "invalid JSON: %v", err)
	}
	version, err := detectVersion(top, source)
	if err != nil {
		return nil, err
	}
	var project *domain.Project
	switch version {
	case domain.SchemaV1:
		project, err = loadV1(raw)
	case domain.SchemaV2:
		project, err = loadV2(raw)
	case domain.SchemaV3:
		project, err = loadV3(raw)
	default:
		err = domain.NewBizError(domain.ESchemaVersion, "$", "unsupported schema version %q", version)
	}
	if err != nil {
		return nil, err
	}
	project.SourceSchema = version
	project.SourceName = source
	if err := domain.ValidateProject(project); err != nil {
		return nil, err
	}
	return project, nil
}

func detectVersion(top map[string]json.RawMessage, source string) (string, error) {
	versionKeys := 0
	for _, key := range []string{"schema_version", "schema", "version"} {
		if _, ok := top[key]; ok {
			versionKeys++
		}
	}
	if versionKeys == 0 {
		return "", domain.NewBizError(domain.ESchemaVersion, "$", "missing schema version discriminator")
	}
	if versionKeys > 1 {
		return "", domain.NewBizError(domain.ESchemaMixedUnits, "$",
			"multiple schema version discriminators are present in %q", source)
	}
	var version string
	switch {
	case top["schema_version"] != nil:
		if err := json.Unmarshal(top["schema_version"], &version); err != nil {
			return "", domain.NewBizError(domain.ESchemaVersion, "$.schema_version", "schema_version must be a string")
		}
		if version != domain.SchemaV1 {
			return "", domain.NewBizError(domain.ESchemaVersion, "$.schema_version", "unsupported schema_version %q", version)
		}
	case top["schema"] != nil:
		if err := json.Unmarshal(top["schema"], &version); err != nil {
			return "", domain.NewBizError(domain.ESchemaVersion, "$.schema", "schema must be a string")
		}
		if version != domain.SchemaV2 {
			return "", domain.NewBizError(domain.ESchemaVersion, "$.schema", "unsupported schema %q", version)
		}
	case top["version"] != nil:
		if err := json.Unmarshal(top["version"], &version); err != nil {
			return "", domain.NewBizError(domain.ESchemaVersion, "$.version", "version must be a string")
		}
		if version != domain.SchemaV3 && version != "3.0" {
			return "", domain.NewBizError(domain.ESchemaVersion, "$.version", "unsupported version %q", version)
		}
		version = domain.SchemaV3
	}
	return version, nil
}

func decodeStrict(raw []byte, target any, path string) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(target); err != nil {
		message := err.Error()
		if strings.Contains(message, "unknown field") {
			field := ""
			if start := strings.Index(message, "\""); start >= 0 {
				if end := strings.Index(message[start+1:], "\""); end >= 0 {
					field = message[start+1 : start+1+end]
				}
			}
			code := domain.ESchemaUnknownField
			if unitFieldNames[field] {
				code = domain.ESchemaMixedUnits
			}
			return domain.NewBizError(code, path+"."+field, "field %q is not valid for this schema: %s", field, message)
		}
		return domain.NewBizError(domain.ESchemaInvalid, path, "decode failed: %v", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return domain.NewBizError(domain.ESchemaInvalid, path, "trailing JSON content")
	}
	return nil
}

type rawEvent struct {
	TimeKey    int64
	InputIndex int
	Event      domain.Event
}

func canonicalizeEvents(raw []rawEvent, path string) ([]domain.Event, error) {
	sort.SliceStable(raw, func(i, j int) bool {
		if raw[i].TimeKey != raw[j].TimeKey {
			return raw[i].TimeKey < raw[j].TimeKey
		}
		return raw[i].InputIndex < raw[j].InputIndex
	})
	events := make([]domain.Event, 0, len(raw))
	lastTime := int64(-1)
	tick := int64(0)
	order := 0
	for index, item := range raw {
		if item.TimeKey < 0 {
			return nil, domain.NewBizError(domain.EEventOrder, fmt.Sprintf("%s[%d].time", path, index), "negative logical time")
		}
		if item.TimeKey != lastTime {
			tick++
			lastTime = item.TimeKey
			order = 0
		}
		event := item.Event
		event.Tick = tick
		event.Order = order
		events = append(events, event)
		order++
	}
	return events, nil
}

func normalizeTenants(tenants []domain.Tenant) []domain.Tenant {
	out := append([]domain.Tenant(nil), tenants...)
	for i := range out {
		if out[i].Name == "" {
			out[i].Name = out[i].ID
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func normalizeNodes(nodes []domain.Node) []domain.Node {
	out := append([]domain.Node(nil), nodes...)
	for i := range out {
		out[i].AllowedTenants = domain.SortedCopy(out[i].AllowedTenants)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func normalizeDomains(domains []domain.FaultDomain) []domain.FaultDomain {
	out := append([]domain.FaultDomain(nil), domains...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func ensureRackDomains(nodes []domain.Node, domains []domain.FaultDomain) []domain.FaultDomain {
	seen := map[string]bool{}
	for _, fd := range domains {
		seen[fd.ID] = true
	}
	for _, node := range nodes {
		if (node.FaultDomain == "" || node.FaultDomain == node.Rack) && node.Rack != "" && !seen[node.Rack] {
			domains = append(domains, domain.FaultDomain{ID: node.Rack})
			seen[node.Rack] = true
		}
	}
	return normalizeDomains(domains)
}

func validateRackTree(project *domain.Project) error {
	for _, node := range project.Nodes {
		if node.Rack == "" {
			return domain.NewBizError(domain.ESchemaRequired, "$.rackTree."+node.ID, "rack id is required")
		}
	}
	return nil
}

func timeKeyRequired(key int64, path string) error {
	if key < 0 {
		return domain.NewBizError(domain.EEventOrder, path, "time key must be non-negative")
	}
	return nil
}

func eventKind(kind string, path string) (domain.EventKind, error) {
	switch kind {
	case "request", "allocate", "reserve":
		return domain.EventRequest, nil
	case "release", "deallocate", "free":
		return domain.EventRelease, nil
	case "weight", "reweight":
		return domain.EventWeight, nil
	case "node_failure", "node_down", "failure":
		return domain.EventNodeFailure, nil
	case "node_recovery", "node_up", "recovery":
		return domain.EventNodeRecovery, nil
	default:
		return "", domain.NewBizError(domain.EEventInvalid, path, "unknown event kind %q", kind)
	}
}
