package quota

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

type LoadOptions struct {
	ExpectedVersion string
}

type rawEnvelope struct {
	SchemaVersion string `json:"schema_version"`
}

type rawTenantV1 struct {
	ID                string `json:"tenant_id"`
	QuotaPermille     int64  `json:"quota_permille"`
	GuaranteePermille int64  `json:"guarantee_permille"`
	BurstPermille     *int64 `json:"burst_permille"`
	Priority          int    `json:"priority"`
}

type rawNodeV1 struct {
	ID            string   `json:"node_id"`
	CapacityCores int64    `json:"capacity_cores"`
	NodeTenants   []string `json:"allowed_tenants"`
	RackID        string   `json:"rack_id"`
}

type rawEventV1 struct {
	Seq            int       `json:"seq"`
	AtSeconds      int64     `json:"at_seconds"`
	Type           EventType `json:"type"`
	TenantID       string    `json:"tenant_id"`
	NodeID         string    `json:"node_id"`
	AmountCores    int64     `json:"amount_cores"`
	WeightPermille int64     `json:"weight_permille"`
}

type rawProjectV1 struct {
	SchemaVersion string        `json:"schema_version"`
	Seed          int64         `json:"seed"`
	Tenants       []rawTenantV1 `json:"tenants"`
	Nodes         []rawNodeV1   `json:"nodes"`
	Events        []rawEventV1  `json:"events"`
}

type rawTenantV2 struct {
	ID               string `json:"tenant_id"`
	QuotaBasisPoints int64  `json:"quota_basis_points"`
	GuaranteeBP      int64  `json:"guarantee_basis_points"`
	BurstBasisPoints *int64 `json:"burst_basis_points"`
	Priority         int    `json:"priority"`
}

type rawNodeV2 struct {
	ID            string   `json:"node_id"`
	CapacityMilli int64    `json:"capacity_millicores"`
	NodeTenants   []string `json:"allowed_tenants"`
	RackID        string   `json:"rack_id"`
}

type rawEventV2 struct {
	Seq            int       `json:"seq"`
	AtMillis       int64     `json:"at_millis"`
	Type           EventType `json:"type"`
	TenantID       string    `json:"tenant_id"`
	NodeID         string    `json:"node_id"`
	AmountMilli    int64     `json:"amount_millicores"`
	WeightPermille int64     `json:"weight_permille"`
}

type rawProjectV2 struct {
	SchemaVersion string        `json:"schema_version"`
	Seed          int64         `json:"seed"`
	Tenants       []rawTenantV2 `json:"tenants"`
	Nodes         []rawNodeV2   `json:"nodes"`
	Events        []rawEventV2  `json:"events"`
}

type rawTenantV3 struct {
	ID          string `json:"id"`
	QuotaBP     int64  `json:"quota_bp"`
	GuaranteeBP int64  `json:"guarantee_bp"`
	BurstBP     *int64 `json:"burst_bp"`
	Priority    int    `json:"priority"`
}

type rawNodeV3 struct {
	ID         string   `json:"id"`
	CapacityMC int64    `json:"capacity_mc"`
	Tenants    []string `json:"tenants"`
}

type rawRackV3 struct {
	ID    string      `json:"id"`
	Nodes []rawNodeV3 `json:"nodes"`
}

type rawEventV3 struct {
	Seq      int       `json:"seq"`
	TimeMS   int64     `json:"time_ms"`
	Type     EventType `json:"type"`
	TenantID string    `json:"tenant_id"`
	NodeID   string    `json:"node_id"`
	DeltaMC  int64     `json:"delta_mc"`
	Weight   int64     `json:"weight"`
}

type rawProjectV3 struct {
	SchemaVersion string        `json:"schema_version"`
	Seed          int64         `json:"seed"`
	Tenants       []rawTenantV3 `json:"tenants"`
	Racks         []rawRackV3   `json:"racks"`
	Events        []rawEventV3  `json:"events"`
}

// Load detects a known schema version and maps it onto Project. It never
// mutates the input byte slice.
func Load(data []byte) (*Project, error) {
	return LoadWithOptions(data, LoadOptions{})
}

func LoadWithOptions(data []byte, opts LoadOptions) (*Project, error) {
	var envelope rawEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, &BusinessError{Code: CodeSchemaUnknown, Op: "load", Detail: "invalid schema envelope", Err: err}
	}
	version := envelope.SchemaVersion
	if version == "" {
		return nil, NewError(CodeSchemaUnknown, "load", "schema_version", "missing schema version")
	}
	if opts.ExpectedVersion != "" && version != opts.ExpectedVersion {
		return nil, &BusinessError{
			Code: CodeVersionMismatch, Op: "load", Path: "schema_version",
			Detail: fmt.Sprintf("got %s want %s", version, opts.ExpectedVersion),
		}
	}

	var p *Project
	var err error
	switch version {
	case "v1":
		p, err = loadV1(data)
	case "v2":
		p, err = loadV2(data)
	case "v3":
		p, err = loadV3(data)
	default:
		return nil, NewError(CodeSchemaUnknown, "load", "schema_version", "unsupported schema version "+version)
	}
	if err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

func decodeStrict(data []byte, target any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}

func loadV1(data []byte) (*Project, error) {
	var raw rawProjectV1
	if err := decodeStrict(data, &raw); err != nil {
		return nil, &BusinessError{Code: CodeSchemaField, Op: "load_v1", Detail: "v1 shape mismatch", Err: err}
	}
	p := &Project{SchemaVersion: "v1", Seed: raw.Seed}
	for i, t := range raw.Tenants {
		path := fmt.Sprintf("tenants[%d].quota_permille", i)
		if err := checkTenantPermille(t.QuotaPermille, t.GuaranteePermille, t.BurstPermille, path, t.ID); err != nil {
			return nil, err
		}
		converted, err := convertPermilleTenant(t, i)
		if err != nil {
			return nil, err
		}
		p.Tenants = append(p.Tenants, converted)
	}
	for i, n := range raw.Nodes {
		path := fmt.Sprintf("nodes[%d].capacity_cores", i)
		c, err := ConvertUnit(n.CapacityCores, MilliPerCore, path, "capacity_cores * 1000")
		if err != nil {
			return nil, err
		}
		p.Nodes = append(p.Nodes, Node{
			ID: n.ID, CapacityMilli: c.Result, RackID: n.RackID,
			AllowedTenants: append([]string(nil), n.NodeTenants...),
		})
	}
	for i, e := range raw.Events {
		path := fmt.Sprintf("events[%d].at_seconds", i)
		tc, err := ConvertUnit(e.AtSeconds, MillisPerSecond, path, "at_seconds * 1000")
		if err != nil {
			return nil, err
		}
		canonical := Event{Seq: e.Seq, TimeMilli: tc.Result, Type: e.Type, TenantID: e.TenantID, NodeID: e.NodeID, WeightPermille: e.WeightPermille}
		if e.Type == EventRequest || e.Type == EventRelease {
			ac, err := ConvertUnit(e.AmountCores, MilliPerCore, fmt.Sprintf("events[%d].amount_cores", i), "amount_cores * 1000")
			if err != nil {
				return nil, err
			}
			canonical.AmountMilli = ac.Result
		}
		p.Events = append(p.Events, canonical)
	}
	sortEvents(p.Events)
	return p, nil
}

func checkTenantPermille(quota, guarantee int64, burst *int64, path, tenantID string) error {
	vals := []struct {
		name  string
		value int64
	}{{"quota", quota}, {"guarantee", guarantee}}
	if burst != nil {
		vals = append(vals, struct {
			name  string
			value int64
		}{"burst", *burst})
	}
	for _, v := range vals {
		if v.value < 0 {
			e := NewError(CodeNegativeQuota, "load", path, v.name+" permille is negative")
			e.TenantID = tenantID
			return e
		}
		if v.value > RatioScalePermille {
			e := NewError(CodeQuotaRange, "load", path, v.name+" permille exceeds 1000")
			e.TenantID = tenantID
			return e
		}
	}
	return nil
}

func convertPermilleTenant(t rawTenantV1, idx int) (Tenant, error) {
	path := fmt.Sprintf("tenants[%d].quota_permille", idx)
	q, err := ScaleRatio(t.QuotaPermille, RatioScalePermille, RatioScaleBasisPoints, path)
	if err != nil {
		return Tenant{}, err
	}
	g, err := ScaleRatio(t.GuaranteePermille, RatioScalePermille, RatioScaleBasisPoints, fmt.Sprintf("tenants[%d].guarantee_permille", idx))
	if err != nil {
		return Tenant{}, err
	}
	var burst int64
	if t.BurstPermille != nil {
		b, err := ScaleRatio(*t.BurstPermille, RatioScalePermille, RatioScaleBasisPoints, fmt.Sprintf("tenants[%d].burst_permille", idx))
		if err != nil {
			return Tenant{}, err
		}
		burst = b.Result
	}
	return Tenant{ID: t.ID, QuotaBP: q.Result, GuaranteeBP: g.Result, BurstBP: burst, Priority: t.Priority}, nil
}

func loadV2(data []byte) (*Project, error) {
	var raw rawProjectV2
	if err := decodeStrict(data, &raw); err != nil {
		return nil, &BusinessError{Code: CodeSchemaField, Op: "load_v2", Detail: "v2 shape mismatch", Err: err}
	}
	p := &Project{SchemaVersion: "v2", Seed: raw.Seed}
	for i, t := range raw.Tenants {
		path := fmt.Sprintf("tenants[%d]", i)
		if err := checkTenantBP(t.QuotaBasisPoints, t.GuaranteeBP, t.BurstBasisPoints, path, t.ID); err != nil {
			return nil, err
		}
		var burst int64
		if t.BurstBasisPoints != nil {
			burst = *t.BurstBasisPoints
		}
		p.Tenants = append(p.Tenants, Tenant{ID: t.ID, QuotaBP: t.QuotaBasisPoints, GuaranteeBP: t.GuaranteeBP, BurstBP: burst, Priority: t.Priority})
	}
	for _, n := range raw.Nodes {
		p.Nodes = append(p.Nodes, Node{ID: n.ID, CapacityMilli: n.CapacityMilli, RackID: n.RackID, AllowedTenants: append([]string(nil), n.NodeTenants...)})
	}
	for _, e := range raw.Events {
		p.Events = append(p.Events, Event{Seq: e.Seq, TimeMilli: e.AtMillis, Type: e.Type, TenantID: e.TenantID, NodeID: e.NodeID, AmountMilli: e.AmountMilli, WeightPermille: e.WeightPermille})
	}
	sortEvents(p.Events)
	return p, nil
}

func loadV3(data []byte) (*Project, error) {
	var raw rawProjectV3
	if err := decodeStrict(data, &raw); err != nil {
		return nil, &BusinessError{Code: CodeSchemaField, Op: "load_v3", Detail: "v3 shape mismatch", Err: err}
	}
	p := &Project{SchemaVersion: "v3", Seed: raw.Seed}
	for i, t := range raw.Tenants {
		if err := checkTenantBP(t.QuotaBP, t.GuaranteeBP, t.BurstBP, fmt.Sprintf("tenants[%d]", i), t.ID); err != nil {
			return nil, err
		}
		var burst int64
		if t.BurstBP != nil {
			burst = *t.BurstBP
		}
		p.Tenants = append(p.Tenants, Tenant{ID: t.ID, QuotaBP: t.QuotaBP, GuaranteeBP: t.GuaranteeBP, BurstBP: burst, Priority: t.Priority})
	}
	for _, rack := range raw.Racks {
		for _, n := range rack.Nodes {
			p.Nodes = append(p.Nodes, Node{ID: n.ID, CapacityMilli: n.CapacityMC, RackID: rack.ID, AllowedTenants: append([]string(nil), n.Tenants...)})
		}
	}
	for _, e := range raw.Events {
		p.Events = append(p.Events, Event{Seq: e.Seq, TimeMilli: e.TimeMS, Type: e.Type, TenantID: e.TenantID, NodeID: e.NodeID, AmountMilli: e.DeltaMC, WeightPermille: e.Weight})
	}
	sortEvents(p.Events)
	return p, nil
}

func checkTenantBP(quota, guarantee int64, burst *int64, path, tenantID string) error {
	if quota < 0 || guarantee < 0 || (burst != nil && *burst < 0) {
		e := NewError(CodeNegativeQuota, "load", path, "negative basis-point quota")
		e.TenantID = tenantID
		return e
	}
	if quota > RatioScaleBasisPoints || guarantee > RatioScaleBasisPoints || (burst != nil && *burst > RatioScaleBasisPoints) {
		e := NewError(CodeQuotaRange, "load", path, "basis-point ratio exceeds 10000")
		e.TenantID = tenantID
		return e
	}
	return nil
}

func sortEvents(events []Event) {
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].TimeMilli != events[j].TimeMilli {
			return events[i].TimeMilli < events[j].TimeMilli
		}
		return events[i].Seq < events[j].Seq
	})
}
