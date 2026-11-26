// Package loader parses all supported schema versions into spec.Project.
package loader

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"dnslab/codes"
	"dnslab/fixedint"
	"dnslab/spec"
)

type LoadResult struct {
	SchemaVersion string        `json:"schema_version"`
	Project       *spec.Project `json:"project"`
}

// Load detects schema version and parses a JSON engineering document.
func Load(data []byte) (*LoadResult, error) {
	root, err := decodeObject(data, "$")
	if err != nil {
		return nil, err
	}
	schema, err := detectSchema(root)
	if err != nil {
		return nil, err
	}
	switch schema {
	case spec.SchemaV1:
		p, err := loadV1(root)
		return &LoadResult{SchemaVersion: schema, Project: p}, err
	case spec.SchemaV2:
		p, err := loadV2(root)
		return &LoadResult{SchemaVersion: schema, Project: p}, err
	case spec.SchemaV3:
		p, err := loadV3(root)
		return &LoadResult{SchemaVersion: schema, Project: p}, err
	default:
		return nil, codes.New(codes.ESchemaUnsupported, "unsupported schema").
			WithContext("schema", schema)
	}
}

func decodeObject(data []byte, path string) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, codes.Wrap(codes.EFieldInvalid, "invalid JSON document", err).
			WithContext("path", path)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, codes.New(codes.EFieldType, "root must be a JSON object").
			WithField(path).WithContext("actual", fmt.Sprintf("%T", v))
	}
	return m, nil
}

func detectSchema(root map[string]any) (string, error) {
	markers := map[string]string{}
	for _, key := range []string{"schema", "schemaVersion", "apiVersion"} {
		if v, ok := root[key]; ok {
			s, err := asString(v, "$."+key)
			if err != nil {
				return "", err
			}
			markers[key] = s
		}
	}
	if len(markers) > 1 {
		return "", codes.New(codes.EVersionConflict, "multiple schema markers cannot be disambiguated").
			WithContext("markers", markers)
	}
	if len(markers) == 1 {
		for _, s := range markers {
			switch s {
			case spec.SchemaV1, spec.SchemaV2, spec.SchemaV3:
				return s, nil
			}
			return "", codes.New(codes.ESchemaUnsupported, "schema marker is not supported").
				WithContext("schema", s)
		}
	}

	indicators := map[string]string{}
	if _, ok := root["flat_rules"]; ok {
		indicators["flat_rules"] = spec.SchemaV1
	}
	if _, ok := root["authority_profiles"]; ok {
		indicators["authority_profiles"] = spec.SchemaV1
	}
	if _, ok := root["upstreams"]; ok {
		indicators["upstreams"] = spec.SchemaV2
	}
	if _, ok := root["rules"]; ok {
		indicators["rules"] = spec.SchemaV2
	}
	if _, ok := root["regionTree"]; ok {
		indicators["regionTree"] = spec.SchemaV3
	}
	versions := map[string]bool{}
	for _, v := range indicators {
		versions[v] = true
	}
	if len(versions) == 1 {
		for v := range versions {
			return v, nil
		}
	}
	if len(versions) > 1 {
		return "", codes.New(codes.EVersionConflict, "document organization matches multiple schema versions").
			WithContext("indicators", indicators)
	}
	return "", codes.New(codes.ESchemaUnsupported, "document has no recognizable schema marker or organization")
}

func asMap(v any, path string) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, codes.New(codes.EFieldType, "expected object").
			WithField(path).WithContext("actual", fmt.Sprintf("%T", v))
	}
	return m, nil
}

func asArray(v any, path string) ([]any, error) {
	a, ok := v.([]any)
	if !ok {
		return nil, codes.New(codes.EFieldType, "expected array").
			WithField(path).WithContext("actual", fmt.Sprintf("%T", v))
	}
	return a, nil
}

func reqMap(m map[string]any, key, path string) (map[string]any, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, codes.New(codes.EFieldMissing, "required object field is missing").
			WithField(path + "." + key)
	}
	return asMap(v, path+"."+key)
}

func reqArray(m map[string]any, key, path string) ([]any, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, codes.New(codes.EFieldMissing, "required array field is missing").
			WithField(path + "." + key)
	}
	return asArray(v, path+"."+key)
}

func reqValue(m map[string]any, key, path string) (any, string, error) {
	v, ok := m[key]
	if !ok || v == nil || v == "" {
		return nil, "", codes.New(codes.EFieldMissing, "required field is missing or empty").
			WithField(path + "." + key)
	}
	return v, path + "." + key, nil
}

func reqString(m map[string]any, key, path string) (string, error) {
	v, field, err := reqValue(m, key, path)
	if err != nil {
		return "", err
	}
	return asString(v, field)
}

func asString(v any, field string) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case json.Number:
		return x.String(), nil
	default:
		return "", codes.New(codes.EFieldType, "expected string").
			WithField(field).WithContext("actual", fmt.Sprintf("%T", v))
	}
}

func reqInt(m map[string]any, key, path string) (int64, error) {
	v, field, err := reqValue(m, key, path)
	if err != nil {
		return 0, err
	}
	return asInt(v, field)
}

func asInt(v any, field string) (int64, error) {
	switch x := v.(type) {
	case json.Number:
		n, err := strconv.ParseInt(x.String(), 10, 64)
		if err != nil {
			return 0, codes.Wrap(codes.EFieldInvalid, "expected fixed-width int64", err).
				WithField(field).WithContext("value", x.String())
		}
		return n, nil
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		if err != nil {
			return 0, codes.Wrap(codes.EFieldInvalid, "expected integer string", err).
				WithField(field).WithContext("value", x)
		}
		return n, nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	default:
		return 0, codes.New(codes.EFieldType, "expected integer").
			WithField(field).WithContext("actual", fmt.Sprintf("%T", v))
	}
}

func boolOr(m map[string]any, key string, def bool) bool {
	v, ok := m[key]
	if !ok || v == nil {
		return def
	}
	b, ok := v.(bool)
	if !ok {
		return def
	}
	return b
}

func msFromSeconds(seconds int64, field string) (int64, error) {
	ms, saturated := fixedint.SecondsToMS(seconds)
	if saturated {
		return 0, codes.New(codes.ETTLOverflow, "seconds-to-milliseconds multiplication saturated").
			WithField(field).
			WithContext("seconds", seconds).
			WithContext("bits", fixedint.TimeBits)
	}
	return ms, nil
}

func absoluteFromRemaining(referenceMS, remainingMS int64, field string) (int64, error) {
	expires, saturated := fixedint.RemainingToAbsolute(referenceMS, remainingMS)
	if saturated {
		return 0, codes.New(codes.EIntOverflow, "absolute expiry addition saturated").
			WithField(field).
			WithContext("reference_ms", referenceMS).
			WithContext("remaining_ms", remainingMS)
	}
	return expires, nil
}

func finalizeProject(p *spec.Project) (*spec.Project, error) {
	if err := fixedint.ValidateWorkerBudget(p.WorkerBudget); err != nil {
		return nil, err
	}
	if err := fixedint.ValidateHopBudget(p.MaxHops); err != nil {
		return nil, err
	}
	if p.DefaultTimeoutMS != 0 {
		if err := fixedint.ValidateTimeoutMS(p.DefaultTimeoutMS); err != nil {
			return nil, err
		}
	}
	for _, u := range p.Upstreams {
		if err := fixedint.ValidateTimeoutMS(u.TimeoutMS); err != nil {
			return nil, err.(*codes.Error).WithContext("upstream_index", u.Index)
		}
		if u.ForwardTo < 0 || u.ForwardTo > len(p.Upstreams) {
			return nil, codes.New(codes.EUpstreamReference, "forward target index is outside upstream list").
				WithContext("upstream_index", u.Index).
				WithContext("forward_to", u.ForwardTo).
				WithContext("upstream_count", len(p.Upstreams))
		}
	}
	seenRegion := map[string]bool{}
	for _, r := range p.Regions {
		if seenRegion[r.ID] {
			return nil, codes.New(codes.ERegionDuplicate, "duplicate region id").
				WithContext("region_id", r.ID)
		}
		seenRegion[r.ID] = true
	}
	for _, r := range p.Rules {
		if !seenRegion[r.RegionID] {
			return nil, codes.New(codes.ERegionNotFound, "rule references unknown region").
				WithContext("rule_number", r.Number).
				WithContext("region_id", r.RegionID)
		}
		if r.UpstreamIndex < 1 || r.UpstreamIndex > len(p.Upstreams) {
			return nil, codes.New(codes.EUpstreamReference, "rule references invalid upstream index").
				WithContext("rule_number", r.Number).
				WithContext("upstream_index", r.UpstreamIndex).
				WithContext("upstream_count", len(p.Upstreams))
		}
	}
	p.Fingerprint = spec.FingerprintOf(p)
	if strings.HasPrefix(p.Version, "det-") && len(p.Version) > 4 {
		// Generated documents carry the expected version. Loading preserves it;
		// this check catches accidental canonicalization drift during tests.
		expected := "det-" + p.Fingerprint[:12]
		if p.Version != expected {
			biz := codes.New(codes.EVersionConflict, "document version does not match canonical content fingerprint").
				WithContext("document_version", p.Version).
				WithContext("canonical_version", expected)
			return p, biz
		}
	}
	return p, nil
}
