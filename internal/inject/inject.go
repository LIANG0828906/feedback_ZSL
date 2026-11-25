package inject

import (
	"encoding/json"

	"quotaflow/internal/domain"
)

const (
	NegativeQuota    = "negative-quota"
	ReserveOverQuota = "reserve-over-quota"
	FaultDomainCycle = "fault-domain-cycle"
	SnapshotVersion  = "snapshot-version"
	UnknownUnitField = "unknown-unit-field"
)

// MutateProject injects a fault into an in-memory JSON document and returns a
// new byte slice. The caller's source bytes and files are never modified.
func MutateProject(kind string, raw []byte) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, domain.NewBizError(domain.ESchemaInvalid, "$", "injection source is invalid JSON: %v", err)
	}
	switch kind {
	case NegativeQuota:
		tenant := firstMap(doc, "tenants", "tenantList")
		switch {
		case tenant["quota_percent"] != nil:
			tenant["quota_percent"] = -1
		case tenant["quotaBasisPoints"] != nil:
			tenant["quotaBasisPoints"] = -1
		case tenant["quotaBp"] != nil:
			tenant["quotaBp"] = -1
		default:
			return nil, domain.NewBizError(domain.EInjectionSource, "$.tenants[0]", "no quota field found")
		}
	case ReserveOverQuota:
		tenant := firstMap(doc, "tenants", "tenantList")
		switch {
		case tenant["reserve_percent"] != nil:
			tenant["reserve_percent"] = 150
		case tenant["reserveBasisPoints"] != nil:
			tenant["reserveBasisPoints"] = 10001
		case tenant["reserveBp"] != nil:
			tenant["reserveBp"] = 10001
		default:
			return nil, domain.NewBizError(domain.EInjectionSource, "$.tenants[0]", "no reserve field found")
		}
	case FaultDomainCycle:
		if doc["faultDomains"] == nil {
			doc["faultDomains"] = []any{
				map[string]any{"domainId": "rack-a", "parentId": "rack-b"},
				map[string]any{"domainId": "rack-b", "parentId": "rack-a"},
				map[string]any{"domainId": "rack-c"},
			}
		} else {
			domains, ok := doc["faultDomains"].([]any)
			if !ok || len(domains) < 2 {
				return nil, domain.NewBizError(domain.EInjectionSource, "$.faultDomains", "need two domains for cycle")
			}
			first := domains[0].(map[string]any)
			second := domains[1].(map[string]any)
			firstID, _ := first["domainId"].(string)
			secondID, _ := second["domainId"].(string)
			first["parentId"] = secondID
			second["parentId"] = firstID
		}
	case SnapshotVersion:
		switch {
		case doc["schema_version"] != nil:
			doc["schema_version"] = "v9"
		case doc["schema"] != nil:
			doc["schema"] = "v9"
		case doc["version"] != nil:
			doc["version"] = "9.0"
		default:
			return nil, domain.NewBizError(domain.EInjectionSource, "$", "no version field found")
		}
	case UnknownUnitField:
		doc["quotaBasisPoints"] = 100
	default:
		return nil, domain.NewBizError(domain.EInjectionSource, "$.injection", "unknown injection kind %q", kind)
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, domain.NewBizError(domain.EInjectionSource, "$", "encode injected document: %v", err)
	}
	return append(out, '\n'), nil
}

func firstMap(doc map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		items, ok := doc[key].([]any)
		if !ok || len(items) == 0 {
			continue
		}
		if item, ok := items[0].(map[string]any); ok {
			return item
		}
	}
	return map[string]any{}
}
