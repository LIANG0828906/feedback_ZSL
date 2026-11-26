// Package projectio loads and emits the three supported quota project schemas.
package projectio

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"quota-cluster/internal/bizerror"
	"quota-cluster/internal/fixed"
	"quota-cluster/internal/model"
)

// Load detects one of the three declared schema versions and normalizes it.
func Load(data []byte) (*model.Project, error) {
	var raw map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return nil, bizerror.New(bizerror.CodeFieldInvalidType, "/", "parse JSON: %v", err)
	}
	if be := rejectAmbiguousFields(raw); be != nil {
		return nil, be
	}

	version, be := detectVersion(raw)
	if be != nil {
		return nil, be
	}

	switch version {
	case model.SchemaV1:
		var doc docV1
		if err := strictDecode(data, &doc); err != nil {
			return nil, err
		}
		return mapV1(&doc)
	case model.SchemaV2:
		var doc docV2
		if err := strictDecode(data, &doc); err != nil {
			return nil, err
		}
		return mapV2(&doc)
	case model.SchemaV3:
		var doc docV3
		if err := strictDecode(data, &doc); err != nil {
			return nil, err
		}
		return mapV3(&doc)
	default:
		return nil, bizerror.New(bizerror.CodeSchemaUnsupported, "/schema", "unsupported schema %q", version)
	}
}

func strictDecode(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			path := "/" + strings.ReplaceAll(typeErr.Field, ".", "/")
			return bizerror.New(bizerror.CodeFieldInvalidType, path, "expected %s for %s", typeErr.Type, typeErr.Value)
		}
		return bizerror.New(bizerror.CodeFieldInvalidType, "/", "%v", err)
	}
	return nil
}

func detectVersion(raw map[string]any) (string, *bizerror.Error) {
	_, hasV1 := raw["schema_version"]
	api, hasV2 := raw["apiVersion"]
	spec, hasV3 := raw["spec"]
	count := 0
	if hasV1 {
		count++
	}
	if hasV2 {
		count++
	}
	if hasV3 {
		count++
	}
	if count > 1 {
		return "", bizerror.New(bizerror.CodeFieldAmbiguous, "/", "more than one schema identifier is present")
	}
	if count == 0 {
		return "", bizerror.New(bizerror.CodeFieldRequired, "/schema", "one of schema_version, apiVersion, or spec is required")
	}
	if hasV1 {
		v, _ := api.(string)
		_ = v
		s, ok := raw["schema_version"].(string)
		if !ok || s != model.SchemaV1 {
			return "", bizerror.New(bizerror.CodeSchemaUnsupported, "/schema_version", "unsupported schema_version %v", raw["schema_version"])
		}
		return model.SchemaV1, nil
	}
	if hasV2 {
		s, ok := api.(string)
		if !ok || s != model.SchemaV2 {
			return "", bizerror.New(bizerror.CodeSchemaUnsupported, "/apiVersion", "unsupported apiVersion %v", api)
		}
		return model.SchemaV2, nil
	}
	s, ok := spec.(string)
	if !ok || s != model.SchemaV3 {
		return "", bizerror.New(bizerror.CodeSchemaUnsupported, "/spec", "unsupported spec %v", spec)
	}
	return model.SchemaV3, nil
}

var ambiguousAliasGroups = [][]string{
	{"capacity_bytes", "capacityBytes", "capacity_kib", "capacityKiB"},
	{"at_tick", "atTick", "tick", "at_millis", "atMillis", "timestamp_ms", "timestampMs"},
	{"quota_permille", "quotaPermille", "quota_basis_points", "quotaBasisPoints", "quota_permyriad", "quotaPermyriad"},
	{"guarantee_permille", "guaranteePermille", "guarantee_basis_points", "guaranteeBasisPoints", "guarantee_permyriad", "guaranteePermyriad"},
	{"burst_cap_bytes", "burstCapBytes", "burst_limit_bytes", "burstLimitBytes", "burst_cap_kib", "burstCapKiB", "burst_limit_kib", "burstLimitKiB"},
}

func rejectAmbiguousFields(root map[string]any) *bizerror.Error {
	var walk func(any, string) *bizerror.Error
	walk = func(v any, path string) *bizerror.Error {
		switch x := v.(type) {
		case map[string]any:
			present := map[string]bool{}
			for k := range x {
				present[k] = true
			}
			for _, group := range ambiguousAliasGroups {
				var found []string
				for _, name := range group {
					if present[name] {
						found = append(found, name)
					}
				}
				canonicalKinds := map[string]bool{}
				for _, name := range found {
					kind := aliasKind(name)
					if canonicalKinds[kind] {
						return bizerror.New(bizerror.CodeFieldAmbiguous, path, "fields %s provide the same quantity in incompatible/disambiguated forms", strings.Join(found, ", "))
					}
					canonicalKinds[kind] = true
				}
				if len(found) >= 2 && len(canonicalKinds) >= 2 {
					return bizerror.New(bizerror.CodeFieldAmbiguous, path, "fields %s cannot be used together without an explicit unit/scale/time basis", strings.Join(found, ", "))
				}
			}
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			for _, k := range keys {
				if be := walk(x[k], path+"/"+escapeJSONPointer(k)); be != nil {
					return be
				}
			}
		case []any:
			for i, item := range x {
				if be := walk(item, fmt.Sprintf("%s/%d", path, i)); be != nil {
					return be
				}
			}
		}
		return nil
	}
	return walk(root, "")
}

func aliasKind(name string) string {
	switch {
	case strings.Contains(name, "bytes") || strings.Contains(name, "Bytes"):
		return "bytes"
	case strings.Contains(name, "kib") || strings.Contains(name, "KiB"):
		return "kib"
	case strings.Contains(name, "permille") || strings.Contains(name, "Permille"):
		return "permille"
	case strings.Contains(name, "basis_points") || strings.Contains(name, "BasisPoints") || strings.Contains(name, "permyriad") || strings.Contains(name, "Permyriad"):
		return "basis_points"
	case strings.Contains(name, "tick") || strings.Contains(name, "Tick"):
		return "tick"
	case strings.Contains(name, "millis") || strings.Contains(name, "Millis") || strings.Contains(name, "ms"):
		return "millis"
	default:
		return name
	}
}

func escapeJSONPointer(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	return strings.ReplaceAll(s, "/", "~1")
}

func requireNonEmpty(value, path string) error {
	if strings.TrimSpace(value) == "" {
		return bizerror.New(bizerror.CodeFieldRequired, path, "field is required")
	}
	return nil
}

func positive[T ~int64 | ~int](value T, path string) error {
	if value <= 0 {
		return bizerror.New(bizerror.CodeValueOutOfRange, path, "must be greater than zero")
	}
	return nil
}

func checkOverflow(r fixed.Result, path string) error {
	if r.Overflow {
		return bizerror.New(bizerror.CodeScaleOverflow, path, "fixed-point conversion overflow/saturation")
	}
	return nil
}
