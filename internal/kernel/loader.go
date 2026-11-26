package kernel

import (
	"encoding/json"
	"fmt"
	"strings"
)

const SchemaCanonical SchemaVersion = "canonical"

type rawObject map[string]json.RawMessage

func LoadJSON(data []byte) (*Project, error) {
	var head rawObject
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, NewBizError(CodeInvalidJSON, "%v", err).WithPath("$")
	}
	schemaRaw, ok := head["schema"]
	if !ok {
		return nil, NewBizError(CodeSchemaVersion, "missing schema field").WithPath("$.schema")
	}
	var schema string
	if err := json.Unmarshal(schemaRaw, &schema); err != nil {
		return nil, NewBizError(CodeSchemaVersion, "schema must be a string").WithPath("$.schema")
	}
	version, err := parseSchemaVersion(schema)
	if err != nil {
		return nil, err
	}
	switch version {
	case SchemaV1:
		return loadV1(head)
	case SchemaV2:
		return loadV2(head)
	case SchemaV3:
		return loadV3(head)
	default:
		return nil, NewBizError(CodeSchemaVersion, "unsupported schema %q", schema).WithPath("$.schema")
	}
}

func parseSchemaVersion(s string) (SchemaVersion, error) {
	switch strings.TrimSpace(s) {
	case "v1", "1", "1.0", "quotakernel.project/v1":
		return SchemaV1, nil
	case "v2", "2", "2.0", "quotakernel.project/v2":
		return SchemaV2, nil
	case "v3", "3", "3.0", "quotakernel.project/v3":
		return SchemaV3, nil
	default:
		return "", NewBizError(CodeSchemaVersion, "unsupported schema %q", s).WithPath("$.schema")
	}
}

func asObject(raw json.RawMessage, path string) (rawObject, error) {
	var obj rawObject
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, NewBizError(CodeInvalidJSON, "expected object: %v", err).WithPath(path)
	}
	if obj == nil {
		obj = rawObject{}
	}
	return obj, nil
}

func (o rawObject) checkAllowed(path string, allowed ...string) error {
	known := map[string]bool{}
	for _, k := range allowed {
		known[k] = true
	}
	for k := range o {
		if !known[k] {
			return NewBizError(CodeUnknownField, "unrecognized field %q", k).
				WithPath(path+"."+k).
				WithDetail("allowed_fields", allowed)
		}
	}
	return nil
}

func (o rawObject) field(key string) json.RawMessage {
	return o[key]
}

func reqString(o rawObject, key, path string) (string, error) {
	raw, ok := o[key]
	if !ok {
		return "", NewBizError(CodeUnknownField, "missing required field %q", key).WithPath(path + "." + key)
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", NewBizError(CodeInvalidJSON, "field %q must be string: %v", key, err).WithPath(path + "." + key)
	}
	return v, nil
}

func optString(o rawObject, key, path string, def string) (string, error) {
	if _, ok := o[key]; !ok {
		return def, nil
	}
	return reqString(o, key, path)
}

func reqInt(o rawObject, key, path string) (int64, error) {
	raw, ok := o[key]
	if !ok {
		return 0, NewBizError(CodeUnknownField, "missing required field %q", key).WithPath(path + "." + key)
	}
	return decodeInt(raw, path+"."+key)
}

func optInt(o rawObject, key, path string, def int64) (int64, error) {
	if _, ok := o[key]; !ok {
		return def, nil
	}
	return reqInt(o, key, path)
}

func decodeInt(raw json.RawMessage, path string) (int64, error) {
	var v int64
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, NewBizError(CodeInvalidJSON, "expected integer: %v", err).WithPath(path)
	}
	return v, nil
}

func reqBool(o rawObject, key, path string) (bool, error) {
	raw, ok := o[key]
	if !ok {
		return false, NewBizError(CodeUnknownField, "missing required field %q", key).WithPath(path + "." + key)
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return false, NewBizError(CodeInvalidJSON, "expected boolean: %v", err).WithPath(path + "." + key)
	}
	return v, nil
}

func reqList(o rawObject, key, path string) ([]json.RawMessage, error) {
	raw, ok := o[key]
	if !ok {
		return nil, NewBizError(CodeUnknownField, "missing required field %q", key).WithPath(path + "." + key)
	}
	var v []json.RawMessage
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, NewBizError(CodeInvalidJSON, "expected array: %v", err).WithPath(path + "." + key)
	}
	return v, nil
}

func optStringList(o rawObject, key, path string) ([]string, error) {
	raw, ok := o[key]
	if !ok {
		return nil, nil
	}
	var v []string
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, NewBizError(CodeInvalidJSON, "expected string array: %v", err).WithPath(path + "." + key)
	}
	return v, nil
}

func parsePolicy(raw json.RawMessage, path string, camel, kebab bool) (FixedPolicy, error) {
	p := DefaultFixedPolicy()
	if len(raw) == 0 {
		return p, nil
	}
	obj, err := asObject(raw, path)
	if err != nil {
		return p, err
	}
	var allowed []string
	switch {
	case kebab:
		allowed = []string{"bit-width", "fraction-bits", "tolerance-bytes", "tolerance-ppm"}
	case camel:
		allowed = []string{"bitWidth", "fractionBits", "toleranceBytes", "tolerancePPM"}
	default:
		allowed = []string{"bit_width", "fraction_bits", "tolerance_bytes", "tolerance_ppm"}
	}
	if err := obj.checkAllowed(path, allowed...); err != nil {
		return p, err
	}
	bitKey, fracKey, tolByteKey, tolPPMKey := "bit_width", "fraction_bits", "tolerance_bytes", "tolerance_ppm"
	if camel {
		bitKey, fracKey, tolByteKey, tolPPMKey = "bitWidth", "fractionBits", "toleranceBytes", "tolerancePPM"
	}
	if kebab {
		bitKey, fracKey, tolByteKey, tolPPMKey = "bit-width", "fraction-bits", "tolerance-bytes", "tolerance-ppm"
	}
	if v, err := optInt(obj, bitKey, path, int64(p.BitWidth)); err != nil {
		return p, err
	} else {
		p.BitWidth = int(v)
	}
	if v, err := optInt(obj, fracKey, path, int64(p.FractionBits)); err != nil {
		return p, err
	} else {
		p.FractionBits = uint8(v)
	}
	if v, err := optInt(obj, tolByteKey, path, p.ToleranceBytes); err != nil {
		return p, err
	} else {
		p.ToleranceBytes = v
	}
	if v, err := optInt(obj, tolPPMKey, path, p.TolerancePPM); err != nil {
		return p, err
	} else {
		p.TolerancePPM = v
	}
	return p, p.Validate()
}

func idsFromStrings(ss []string) []ID {
	out := make([]ID, 0, len(ss))
	for _, s := range ss {
		out = append(out, ID(s))
	}
	return out
}

func loadV1(top rawObject) (*Project, error) {
	const path = "$"
	if err := top.checkAllowed(path, "schema", "project_id", "seed", "tick_ms", "fixed_policy", "nodes", "tenants", "events"); err != nil {
		return nil, err
	}
	pid, err := reqString(top, "project_id", path)
	if err != nil {
		return nil, err
	}
	seed, err := optInt(top, "seed", path, 42)
	if err != nil {
		return nil, err
	}
	tickMS, err := reqInt(top, "tick_ms", path)
	if err != nil {
		return nil, err
	}
	policy, err := parsePolicy(top["fixed_policy"], "$.fixed_policy", false, false)
	if err != nil {
		return nil, err
	}
	p := &Project{SchemaVersion: SchemaCanonical, ProjectID: pid, Seed: seed, TickMS: tickMS, Policy: policy}

	nodeRaws, err := reqList(top, "nodes", path)
	if err != nil {
		return nil, err
	}
	for i, raw := range nodeRaws {
		np := fmt.Sprintf("$.nodes[%d]", i)
		obj, err := asObject(raw, np)
		if err != nil {
			return nil, err
		}
		if err := obj.checkAllowed(np, "node_id", "rack", "capacity_bytes", "allowed_tenants"); err != nil {
			return nil, err
		}
		id, err := reqString(obj, "node_id", np)
		if err != nil {
			return nil, err
		}
		rack, err := reqString(obj, "rack", np)
		if err != nil {
			return nil, err
		}
		capacity, err := reqInt(obj, "capacity_bytes", np)
		if err != nil {
			return nil, err
		}
		hosts, err := optStringList(obj, "allowed_tenants", np)
		if err != nil {
			return nil, err
		}
		p.Nodes = append(p.Nodes, Node{ID: ID(id), Rack: rack, CapacityBytes: capacity, AllowedTenants: idsFromStrings(hosts)})
	}

	tenantRaws, err := reqList(top, "tenants", path)
	if err != nil {
		return nil, err
	}
	for i, raw := range tenantRaws {
		tp := fmt.Sprintf("$.tenants[%d]", i)
		obj, err := asObject(raw, tp)
		if err != nil {
			return nil, err
		}
		if err := obj.checkAllowed(tp, "tenant_id", "priority", "weight_permille", "quota_bytes", "guarantee_bytes", "burst_limit_bytes", "guarantee_node_id"); err != nil {
			return nil, err
		}
		id, err := reqString(obj, "tenant_id", tp)
		if err != nil {
			return nil, err
		}
		priority, err := optInt(obj, "priority", tp, 0)
		if err != nil {
			return nil, err
		}
		weightPermille, err := optInt(obj, "weight_permille", tp, 0)
		if err != nil {
			return nil, err
		}
		weightBP, err := PermilleToPermyriad(weightPermille)
		if err != nil {
			return nil, err.(*BizError).WithPath(tp + ".weight_permille").WithTenant(ID(id))
		}
		quota, err := reqInt(obj, "quota_bytes", tp)
		if err != nil {
			return nil, err
		}
		guarantee, err := reqInt(obj, "guarantee_bytes", tp)
		if err != nil {
			return nil, err
		}
		burst, err := optInt(obj, "burst_limit_bytes", tp, 0)
		if err != nil {
			return nil, err
		}
		gnode, err := optString(obj, "guarantee_node_id", tp, "")
		if err != nil {
			return nil, err
		}
		p.Tenants = append(p.Tenants, Tenant{ID: ID(id), Priority: int(priority), WeightPermyriad: weightBP, QuotaBytes: quota, GuaranteeBytes: guarantee, BurstLimitBytes: burst, GuaranteeNodeID: ID(gnode)})
	}

	eventRaws, err := reqList(top, "events", path)
	if err != nil {
		return nil, err
	}
	for i, raw := range eventRaws {
		ep := fmt.Sprintf("$.events[%d]", i)
		e, err := loadV1Event(raw, ep)
		if err != nil {
			return nil, err
		}
		p.Events = append(p.Events, e)
	}
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func loadV1Event(raw json.RawMessage, ep string) (Event, error) {
	obj, err := asObject(raw, ep)
	if err != nil {
		return Event{}, err
	}
	if err := obj.checkAllowed(ep, "event_id", "tick", "op", "type", "tenant_id", "node_id", "amount_bytes", "budget_ms", "weight_permille", "causal_refs"); err != nil {
		return Event{}, err
	}
	_, hasOp := obj["op"]
	_, hasType := obj["type"]
	if hasOp && hasType {
		return Event{}, NewBizError(CodeFieldConflict, "op and type are mutually exclusive schema aliases").
			WithPath(ep)
	}
	id, err := reqString(obj, "event_id", ep)
	if err != nil {
		return Event{}, err
	}
	tick, err := reqInt(obj, "tick", ep)
	if err != nil {
		return Event{}, err
	}
	op, err := optString(obj, "op", ep, "")
	if err != nil {
		return Event{}, err
	}
	if op == "" {
		op, err = reqString(obj, "type", ep)
		if err != nil {
			return Event{}, err
		}
	}
	tenant, err := optString(obj, "tenant_id", ep, "")
	if err != nil {
		return Event{}, err
	}
	node, err := optString(obj, "node_id", ep, "")
	if err != nil {
		return Event{}, err
	}
	amount, err := optInt(obj, "amount_bytes", ep, 0)
	if err != nil {
		return Event{}, err
	}
	budget, err := optInt(obj, "budget_ms", ep, 0)
	if err != nil {
		return Event{}, err
	}
	weightPermille, err := optInt(obj, "weight_permille", ep, 0)
	if err != nil {
		return Event{}, err
	}
	refs, err := optStringList(obj, "causal_refs", ep)
	if err != nil {
		return Event{}, err
	}
	weightBP, err := PermilleToPermyriad(weightPermille)
	if err != nil {
		return Event{}, err.(*BizError).WithPath(ep + ".weight_permille").WithEvent(id)
	}
	return Event{ID: id, Tick: tick, Type: normalizeOp(op), TenantID: ID(tenant), NodeID: ID(node), AmountBytes: amount, BudgetMS: budget, WeightPermyriad: weightBP, Refs: refs}, nil
}

func normalizeOp(op string) string {
	switch op {
	case EventTypeApply, "reserve", "capacity.apply":
		return EventTypeApply
	case EventTypeRelease, "free", "capacity.release":
		return EventTypeRelease
	case EventTypeWeight, "reweight", "weight.set":
		return EventTypeWeight
	default:
		return op
	}
}
