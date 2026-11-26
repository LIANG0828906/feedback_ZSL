// Package loader maps the three raw schema versions onto the version-neutral
// domain model. All numeric normalisation goes through the fixed-point scale
// package; nothing here uses floating point.
package loader

import (
	"bytes"
	"encoding/json"
	"strconv"

	"schedlab/internal/domain"
	"schedlab/internal/faults"
	"schedlab/internal/fixedint"
	"schedlab/internal/gen"
)

// Schema version identifiers.
const (
	V1 = "v1"
	V2 = "v2"
	V3 = "v3"
)

// Options controls strictness knobs used by the fault-injection chain.
type Options struct {
	// TopologySemantics optionally provides authoritative key->semantic
	// expectations; mismatches raise E_TOPO_KEY_CONFLICT.
	TopologySemantics map[string]string
}

// Load parses raw JSON of the declared version and normalises it.
func Load(raw []byte, version string, opts Options) (*domain.Project, error) {
	switch version {
	case V1:
		var dto gen.V1Project
		if err := unmarshalStrict(raw, &dto); err != nil {
			return nil, err
		}
		return fromV1(&dto, opts)
	case V2:
		var dto gen.V2Project
		if err := unmarshalStrict(raw, &dto); err != nil {
			return nil, err
		}
		return fromV2(&dto, opts)
	case V3:
		var dto gen.V3Project
		if err := unmarshalStrict(raw, &dto); err != nil {
			return nil, err
		}
		return fromV3(&dto, opts)
	default:
		return nil, faults.New(faults.E_SCHEMA_VERSION, "unknown schema version",
			"version", version)
	}
}

func unmarshalStrict(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return faults.Wrap(faults.E_RESOURCE_PARSE, "invalid project json", err)
	}
	return nil
}

func checkNegative(kind string, cpu, mem int64, entity string) error {
	if cpu < 0 || mem < 0 {
		return faults.New(faults.E_RESOURCE_NEGATIVE, "negative resource quantity",
			"entity", entity, "resource", kind, "cpu_milli", itoa(cpu), "mem_bytes", itoa(mem))
	}
	return nil
}

func checkWeight(phase string, bps int64, ruleID string) error {
	if bps < 0 || bps > fixedint.BPS {
		return faults.New(faults.E_WEIGHT_RANGE, "weight outside [0,10000] bps",
			"rule", ruleID, "phase", phase, "weight_bps", itoa(bps))
	}
	return nil
}

func validateSemantics(seen map[string]string, expected map[string]string) error {
	for key, want := range expected {
		got, ok := seen[key]
		if !ok {
			continue
		}
		if got != want {
			return faults.New(faults.E_TOPO_KEY_CONFLICT, "topology key semantic conflict",
				"topology_key", key, "expected", want, "observed", got)
		}
	}
	return nil
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
