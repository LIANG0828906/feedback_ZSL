package schema

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"quotakernel/fixed"
	"quotakernel/model"
)

const (
	BurstAbsentDegradation = "RULE_BURST_FIELD_ABSENT:NOT_ENABLED"
	BurstZeroDegradation   = "RULE_BURST_EXPLICIT_ZERO:NOT_ENABLED"
)

type LoadResult struct {
	SourceVersion model.SchemaVersion
	Project       *model.Project
	Diagnostics   []model.ConversionDiagnostic
}

func LoadJSON(version model.SchemaVersion, data []byte) (*LoadResult, error) {
	switch version {
	case model.SchemaV1:
		var doc V1Document
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("parse v1: %w", err)
		}
		return LoadV1(&doc)
	case model.SchemaV2:
		var doc V2Document
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("parse v2: %w", err)
		}
		return LoadV2(&doc)
	case model.SchemaV3:
		var doc V3Document
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("parse v3: %w", err)
		}
		return LoadV3(&doc)
	default:
		return nil, fmt.Errorf("unknown schema version %q", version)
	}
}

func LoadV1(doc *V1Document) (*LoadResult, error) {
	if doc.TickIntervalMs <= 0 {
		return nil, fmt.Errorf("v1 tick_interval_ms must be positive")
	}
	nodes := make([]model.Node, 0, len(doc.Nodes))
	var totalCapacity int64
	for _, n := range doc.Nodes {
		nodes = append(nodes, model.Node{
			ID: n.NodeID, RackID: n.Rack, CapacityBytes: n.CapacityBytes,
			AllowedTenants: sortedCopy(n.AllowedTenants),
		})
		totalCapacity += n.CapacityBytes
	}
	tenants := make([]model.Tenant, 0, len(doc.TenantsFlat))
	diags := []model.ConversionDiagnostic{}
	for _, t := range doc.TenantsFlat {
		quota := fixed.PermilleToPPM(t.QuotaPermille)
		guarantee := fixed.PerMyriadToPPM(t.GuaranteePerMyriad)
		quotaBytes := fixed.RateBytes(totalCapacity, quota.Value)
		guaranteeBytes := fixed.RateBytes(totalCapacity, guarantee.Value)
		waitTicks, err := msToTicks(t.MaxQueueWaitMs, doc.TickIntervalMs)
		if err != nil {
			return nil, fmt.Errorf("tenant %s max wait: %w", t.TenantID, err)
		}
		windowTicks, err := msToTicks(t.WindowMs, doc.TickIntervalMs)
		if err != nil {
			return nil, fmt.Errorf("tenant %s window: %w", t.TenantID, err)
		}
		tenant := model.Tenant{
			ID: t.TenantID, RackID: t.Rack, HomeNodeID: t.HomeNodeID,
			Priority: t.Priority, Weight: t.SchedulingWeight,
			QuotaPPM: quota.Value, GuaranteePPM: guarantee.Value,
			QuotaBytes: quotaBytes.Value, GuaranteeBytes: guaranteeBytes.Value,
			WindowMs: t.WindowMs, WindowTicks: int64(windowTicks),
			MaxWaitMs: t.MaxQueueWaitMs, MaxWaitTicks: waitTicks,
		}
		applyBurstBytes(&tenant, t.BurstCapBytes)
		tenants = append(tenants, tenant)
		diags = appendRateDiagnostics(diags, t.TenantID, quota, guarantee, quotaBytes, guaranteeBytes)
	}
	events, err := v1Events(doc.Events)
	if err != nil {
		return nil, err
	}
	project := newProject(doc.ProjectID, doc.Seed, doc.TickIntervalMs, nodes, tenants, events)
	return &LoadResult{SourceVersion: model.SchemaV1, Project: project, Diagnostics: diags}, nil
}

func LoadV2(doc *V2Document) (*LoadResult, error) {
	if doc.TickDurationMs <= 0 {
		return nil, fmt.Errorf("v2 tickDurationMs must be positive")
	}
	nodes := []model.Node{}
	rawTenants := []V2Tenant{}
	rackByTenant := map[string]string{}
	var totalCapacity int64
	for _, rack := range doc.RackTree {
		for _, n := range rack.Nodes {
			capacity := fixed.KiBToBytes(n.CapacityKiB)
			if capacity.Overflow {
				return nil, fmt.Errorf("node %s capacity: %w", n.NodeID, fixed.ErrOverflow)
			}
			nodes = append(nodes, model.Node{
				ID: n.NodeID, RackID: rack.RackID, CapacityBytes: capacity.Value,
				AllowedTenants: sortedCopy(n.AllowedTenantIDs),
			})
			totalCapacity += capacity.Value
		}
		for _, t := range rack.TenantGroups {
			rawTenants = append(rawTenants, t)
			rackByTenant[t.TenantID] = rack.RackID
		}
	}
	tenants := make([]model.Tenant, 0, len(rawTenants))
	diags := []model.ConversionDiagnostic{}
	for _, t := range rawTenants {
		quota := fixed.PerMyriadToPPM(t.QuotaPerTenThousand)
		guarantee := fixed.PermilleToPPM(t.GuaranteePerMille)
		quotaBytes := fixed.RateBytes(totalCapacity, quota.Value)
		guaranteeBytes := fixed.RateBytes(totalCapacity, guarantee.Value)
		waitTicks, err := msToTicks(t.MaxWaitMillis, doc.TickDurationMs)
		if err != nil {
			return nil, fmt.Errorf("tenant %s maxWait: %w", t.TenantID, err)
		}
		windowTicks, err := msToTicks(t.RateWindowMs, doc.TickDurationMs)
		if err != nil {
			return nil, fmt.Errorf("tenant %s window: %w", t.TenantID, err)
		}
		tenant := model.Tenant{
			ID: t.TenantID, RackID: rackByTenant[t.TenantID], HomeNodeID: t.HomeNodeID,
			Priority: t.PriorityLevel, Weight: t.Weight,
			QuotaPPM: quota.Value, GuaranteePPM: guarantee.Value,
			QuotaBytes: quotaBytes.Value, GuaranteeBytes: guaranteeBytes.Value,
			WindowMs: t.RateWindowMs, WindowTicks: int64(windowTicks),
			MaxWaitMs: t.MaxWaitMillis, MaxWaitTicks: waitTicks,
		}
		if t.MaxBurstKiB == nil {
			applyBurstBytes(&tenant, nil)
		} else {
			converted := fixed.KiBToBytes(*t.MaxBurstKiB)
			if converted.Overflow {
				return nil, fmt.Errorf("tenant %s burst: %w", t.TenantID, fixed.ErrOverflow)
			}
			v := converted.Value
			applyBurstBytes(&tenant, &v)
		}
		tenants = append(tenants, tenant)
		diags = appendRateDiagnostics(diags, t.TenantID, quota, guarantee, quotaBytes, guaranteeBytes)
	}
	events, err := v2Events(doc.Timeline, doc.TickDurationMs)
	if err != nil {
		return nil, err
	}
	project := newProject(doc.ProjectID, doc.Seed, doc.TickDurationMs, nodes, tenants, events)
	return &LoadResult{SourceVersion: model.SchemaV2, Project: project, Diagnostics: diags}, nil
}

func LoadV3(doc *V3Document) (*LoadResult, error) {
	tickMs := doc.Metadata.TickMs
	if tickMs <= 0 {
		return nil, fmt.Errorf("v3 metadata.tickMs must be positive")
	}
	nodes := make([]model.Node, 0, len(doc.Cluster.StorageNodes))
	var totalCapacity int64
	for _, n := range doc.Cluster.StorageNodes {
		nodes = append(nodes, model.Node{
			ID: n.Name, RackID: n.RackName, CapacityBytes: n.ByteCapacity,
			AllowedTenants: sortedCopy(n.TenantACL),
		})
		totalCapacity += n.ByteCapacity
	}
	tenants := make([]model.Tenant, 0, len(doc.Cluster.TenantTable))
	diags := []model.ConversionDiagnostic{}
	for _, t := range doc.Cluster.TenantTable {
		quota := fixed.PermilleToPPM(t.QuotaMilli)
		guarantee := fixed.PerMyriadToPPM(t.AssuranceBips)
		quotaBytes := fixed.RateBytes(totalCapacity, quota.Value)
		guaranteeBytes := fixed.RateBytes(totalCapacity, guarantee.Value)
		waitTicks, err := msToTicks(t.WaitMilliseconds, tickMs)
		if err != nil {
			return nil, fmt.Errorf("tenant %s wait: %w", t.ID, err)
		}
		windowTicks, err := msToTicks(t.WindowMilliseconds, tickMs)
		if err != nil {
			return nil, fmt.Errorf("tenant %s window: %w", t.ID, err)
		}
		tenant := model.Tenant{
			ID: t.ID, RackID: t.RackName, HomeNodeID: t.PrimaryNode,
			Priority: t.PriorityRank, Weight: t.SchedWeight,
			QuotaPPM: quota.Value, GuaranteePPM: guarantee.Value,
			QuotaBytes: quotaBytes.Value, GuaranteeBytes: guaranteeBytes.Value,
			WindowMs: t.WindowMilliseconds, WindowTicks: int64(windowTicks),
			MaxWaitMs: t.WaitMilliseconds, MaxWaitTicks: waitTicks,
		}
		applyBurstBytes(&tenant, t.BurstLimitBytes)
		tenants = append(tenants, tenant)
		diags = appendRateDiagnostics(diags, t.ID, quota, guarantee, quotaBytes, guaranteeBytes)
	}
	events, err := v3Events(doc.Timeline)
	if err != nil {
		return nil, err
	}
	project := newProject(doc.Metadata.ProjectName, doc.Metadata.DeterministicSeed, tickMs, nodes, tenants, events)
	return &LoadResult{SourceVersion: model.SchemaV3, Project: project, Diagnostics: diags}, nil
}

func applyBurstBytes(tenant *model.Tenant, explicit *int64) {
	if explicit == nil {
		tenant.BurstFieldPresent = false
		tenant.BurstEnabled = false
		tenant.BurstCapBytes = 0
		tenant.BurstDegradation = BurstAbsentDegradation
		return
	}
	tenant.BurstFieldPresent = true
	tenant.BurstCapBytes = *explicit
	if *explicit > 0 {
		tenant.BurstEnabled = true
		return
	}
	tenant.BurstEnabled = false
	tenant.BurstDegradation = BurstZeroDegradation
}

func newProject(id string, seed, tickMs int64, nodes []model.Node, tenants []model.Tenant, events []model.Event) *model.Project {
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	sort.Slice(tenants, func(i, j int) bool { return tenants[i].ID < tenants[j].ID })
	sort.Slice(events, func(i, j int) bool {
		if events[i].Tick != events[j].Tick {
			return events[i].Tick < events[j].Tick
		}
		return events[i].ID < events[j].ID
	})
	for i := range events {
		events[i].SubmittedOrder = i
	}
	return &model.Project{
		SchemaVersion:  "canonical",
		ProjectID:      id,
		Seed:           seed,
		TickDurationMs: tickMs,
		Rules:          model.DefaultRules(),
		Nodes:          nodes,
		Tenants:        tenants,
		Events:         events,
	}
}

func msToTicks(ms, tickMs int64) (model.Tick, error) {
	if tickMs <= 0 {
		return 0, fmt.Errorf("tick duration must be positive")
	}
	if ms < 0 || ms%tickMs != 0 {
		return 0, fmt.Errorf("milliseconds %d is not aligned to tick duration %d", ms, tickMs)
	}
	return model.Tick(ms / tickMs), nil
}

func normalizedKind(raw string) (model.EventKind, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "apply", "capacity_request", "request", "reserve":
		return model.EventApply, nil
	case "release", "capacity_release", "free":
		return model.EventRelease, nil
	case "burst", "burst_surge", "surge":
		return model.EventBurst, nil
	case "weight", "weight_adjust", "weight_adjustment", "reweight":
		return model.EventWeight, nil
	default:
		return "", fmt.Errorf("unknown event type %q", raw)
	}
}

func v1Events(in []V1Event) ([]model.Event, error) {
	out := make([]model.Event, 0, len(in))
	for _, e := range in {
		kind, err := normalizedKind(e.Type)
		if err != nil {
			return nil, fmt.Errorf("event %s: %w", e.EventID, err)
		}
		out = append(out, model.Event{
			ID: e.EventID, Tick: model.Tick(e.AtTick), Kind: kind,
			TenantID: e.TenantID, NodeID: e.NodeID, AmountBytes: e.AmountBytes,
			NewWeight: e.NewWeight,
		})
	}
	return out, nil
}

func v2Events(in []V2Event, tickMs int64) ([]model.Event, error) {
	out := make([]model.Event, 0, len(in))
	for _, e := range in {
		kind, err := normalizedKind(e.Kind)
		if err != nil {
			return nil, fmt.Errorf("event %s: %w", e.ID, err)
		}
		tick, err := msToTicks(e.AtMillis, tickMs)
		if err != nil {
			return nil, fmt.Errorf("event %s: %w", e.ID, err)
		}
		amount := int64(0)
		if e.AmountKiB != 0 {
			converted := fixed.KiBToBytes(e.AmountKiB)
			if converted.Overflow {
				return nil, fmt.Errorf("event %s amount: %w", e.ID, fixed.ErrOverflow)
			}
			amount = converted.Value
		}
		out = append(out, model.Event{
			ID: e.ID, Tick: tick, Kind: kind, TenantID: e.Tenant, NodeID: e.Node,
			AmountBytes: amount, NewWeight: e.WeightValue,
		})
	}
	return out, nil
}

func v3Events(in []V3Event) ([]model.Event, error) {
	out := make([]model.Event, 0, len(in))
	for _, e := range in {
		kind, err := normalizedKind(e.Action)
		if err != nil {
			return nil, fmt.Errorf("event %s: %w", e.EventName, err)
		}
		out = append(out, model.Event{
			ID: e.EventName, Tick: model.Tick(e.LogicalTick), Kind: kind,
			TenantID: e.TenantName, NodeID: e.TargetNode,
			AmountBytes: e.ByteCount, NewWeight: e.Weight,
		})
	}
	return out, nil
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func appendRateDiagnostics(in []model.ConversionDiagnostic, tenantID string, quota, guarantee, quotaBytes, guaranteeBytes fixed.Result) []model.ConversionDiagnostic {
	in = append(in,
		model.ConversionDiagnostic{TenantID: tenantID, FieldName: "quota_ppm", Actual: quota.Value, Check: quota},
		model.ConversionDiagnostic{TenantID: tenantID, FieldName: "guarantee_ppm", Actual: guarantee.Value, Check: guarantee},
		model.ConversionDiagnostic{TenantID: tenantID, FieldName: "quota_bytes", Actual: quotaBytes.Value, Check: quotaBytes},
		model.ConversionDiagnostic{TenantID: tenantID, FieldName: "guarantee_bytes", Actual: guaranteeBytes.Value, Check: guaranteeBytes},
	)
	return in
}
