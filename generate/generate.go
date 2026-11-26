// Package generate contains the fixed-seed local sample generator.
package generate

import (
	"fmt"
	"math"
	"math/rand"
	"sort"

	"quotakernel/engine"
	"quotakernel/fixed"
	"quotakernel/model"
	"quotakernel/schema"
)

const MiB = int64(1024 * 1024)

type GeneratedProject struct {
	Name      string
	Canonical *model.Project
	V1        *schema.V1Document
	V2        *schema.V2Document
	V3        *schema.V3Document
	Truth     *model.Truth
}

type Bundle struct {
	Seed     int64                        `json:"seed"`
	Projects []*GeneratedProject          `json:"-"`
	Critical []model.ConversionDiagnostic `json:"criticalConversions"`
}

type nodeSpec struct {
	id       string
	rack     string
	capacity int64
	tenants  []string
}

type tenantSpec struct {
	id                 string
	rack               string
	home               string
	priority           int
	weight             int64
	quotaPermille      int64
	guaranteePerMyriad int64
	burst              int64
}

type eventSpec struct {
	id        string
	tick      model.Tick
	kind      model.EventKind
	tenant    string
	node      string
	amountMiB int64
	weight    int64
}

// Generate builds the deterministic acceptance corpus. The seed is retained
// and used to deterministically permute submission-order checks; all declared
// values are scripted so critical outcomes remain reviewable.
func Generate(seed int64) (*Bundle, error) {
	rng := rand.New(rand.NewSource(seed))
	_ = rng.Intn(100) // seed stream is intentionally established for future corpus expansion

	main, err := buildMain(seed)
	if err != nil {
		return nil, err
	}
	contention, err := buildContention(seed)
	if err != nil {
		return nil, err
	}
	reconcile, err := buildReconcile(seed)
	if err != nil {
		return nil, err
	}
	bundle := &Bundle{
		Seed:     seed,
		Projects: []*GeneratedProject{main, contention, reconcile},
		Critical: CriticalDiagnostics(),
	}
	return bundle, nil
}

func buildMain(seed int64) (*GeneratedProject, error) {
	nodes := []nodeSpec{
		{id: "node-a", rack: "rack-a", capacity: 120 * MiB, tenants: []string{"t-alpha", "t-bravo"}},
		{id: "node-b", rack: "rack-b", capacity: 180 * MiB, tenants: []string{"t-charlie", "t-delta"}},
	}
	tenants := []tenantSpec{
		{id: "t-alpha", rack: "rack-a", home: "node-a", priority: 8, weight: 100, quotaPermille: 400, guaranteePerMyriad: 1500, burst: 20 * MiB},
		{id: "t-bravo", rack: "rack-a", home: "node-a", priority: 8, weight: 100, quotaPermille: 300, guaranteePerMyriad: 1000, burst: 20 * MiB},
		{id: "t-charlie", rack: "rack-b", home: "node-b", priority: 6, weight: 100, quotaPermille: 200, guaranteePerMyriad: 800, burst: 10 * MiB},
		{id: "t-delta", rack: "rack-b", home: "node-b", priority: 4, weight: 100, quotaPermille: 100, guaranteePerMyriad: 700, burst: 10 * MiB},
	}
	events := []eventSpec{
		{"evt-01-alpha-guarantee", 1, model.EventApply, "t-alpha", "node-a", 45, 0},
		{"evt-02-bravo-guarantee", 1, model.EventApply, "t-bravo", "node-a", 30, 0},
		{"evt-03-charlie-guarantee", 1, model.EventApply, "t-charlie", "node-b", 24, 0},
		{"evt-04-delta-guarantee", 1, model.EventApply, "t-delta", "node-b", 21, 0},
		{"evt-05-alpha-quota-rest", 2, model.EventApply, "t-alpha", "node-a", 75, 0},
		{"evt-06-bravo-quota-rest", 2, model.EventApply, "t-bravo", "node-a", 60, 0},
		{"evt-07-alpha-release-30", 3, model.EventRelease, "t-alpha", "node-a", 30, 0},
		{"evt-08-bravo-weight-up", 4, model.EventWeight, "t-bravo", "", 0, 300},
		{"evt-09-alpha-release-60", 5, model.EventRelease, "t-alpha", "node-a", 60, 0},
		{"evt-10-alpha-fill-guarantee", 6, model.EventApply, "t-alpha", "node-a", 15, 0},
		{"evt-11-bravo-release-15", 7, model.EventRelease, "t-bravo", "node-a", 15, 0},
		{"evt-12-charlie-burst", 8, model.EventBurst, "t-charlie", "node-b", 60, 0},
		{"evt-13-alpha-burst", 8, model.EventBurst, "t-alpha", "node-a", 20, 0},
		{"evt-14-bravo-burst", 8, model.EventBurst, "t-bravo", "node-a", 20, 0},
		{"evt-15-bravo-release-20", 9, model.EventRelease, "t-bravo", "node-a", 20, 0},
	}
	return buildProject("cluster-main", seed, nodes, tenants, events)
}

func buildContention(seed int64) (*GeneratedProject, error) {
	nodes := []nodeSpec{
		{id: "contend-node", rack: "rack-c", capacity: 1000 * MiB, tenants: []string{"c-alpha", "c-bravo", "c-charlie", "c-delta"}},
	}
	tenants := []tenantSpec{
		{id: "c-alpha", rack: "rack-c", home: "contend-node", priority: 10, weight: 100, quotaPermille: 400, guaranteePerMyriad: 2000, burst: 100 * MiB},
		{id: "c-bravo", rack: "rack-c", home: "contend-node", priority: 9, weight: 100, quotaPermille: 300, guaranteePerMyriad: 1000, burst: 100 * MiB},
		{id: "c-charlie", rack: "rack-c", home: "contend-node", priority: 8, weight: 100, quotaPermille: 200, guaranteePerMyriad: 1000, burst: 100 * MiB},
		{id: "c-delta", rack: "rack-c", home: "contend-node", priority: 7, weight: 100, quotaPermille: 100, guaranteePerMyriad: 500, burst: 50 * MiB},
	}
	events := []eventSpec{
		{"con-01-alpha-quota", 1, model.EventApply, "c-alpha", "contend-node", 400, 0},
		{"con-02-bravo-quota", 1, model.EventApply, "c-bravo", "contend-node", 300, 0},
		{"con-03-charlie-quota", 1, model.EventApply, "c-charlie", "contend-node", 200, 0},
		{"con-04-delta-guarantee", 2, model.EventApply, "c-delta", "contend-node", 50, 0},
		{"con-05-alpha-burst", 2, model.EventBurst, "c-alpha", "contend-node", 100, 0},
		{"con-06-bravo-burst", 2, model.EventBurst, "c-bravo", "contend-node", 100, 0},
		{"con-07-charlie-burst", 2, model.EventBurst, "c-charlie", "contend-node", 100, 0},
	}
	return buildProject("burst-contention", seed+1, nodes, tenants, events)
}

func buildReconcile(seed int64) (*GeneratedProject, error) {
	nodes := []nodeSpec{
		{id: "reconcile-node", rack: "rack-r", capacity: 100 * MiB, tenants: []string{"r-low", "r-high"}},
	}
	tenants := []tenantSpec{
		{id: "r-low", rack: "rack-r", home: "reconcile-node", priority: 1, weight: 100, quotaPermille: 1000, guaranteePerMyriad: 0, burst: 0},
		{id: "r-high", rack: "rack-r", home: "reconcile-node", priority: 10, weight: 100, quotaPermille: 1000, guaranteePerMyriad: 0, burst: 0},
	}
	events := []eventSpec{
		{"rec-01-low-fast-hold", 1, model.EventApply, "r-low", "reconcile-node", 60, 0},
		{"rec-02-high-priority", 1, model.EventApply, "r-high", "reconcile-node", 60, 0},
	}
	return buildProject("gate-reconcile", seed+2, nodes, tenants, events)
}

func buildProject(name string, seed int64, nodeSpecs []nodeSpec, tenantSpecs []tenantSpec, eventSpecs []eventSpec) (*GeneratedProject, error) {
	nodes := make([]model.Node, 0, len(nodeSpecs))
	var total int64
	for _, n := range nodeSpecs {
		allowed := append([]string(nil), n.tenants...)
		sort.Strings(allowed)
		nodes = append(nodes, model.Node{ID: n.id, RackID: n.rack, CapacityBytes: n.capacity, AllowedTenants: allowed})
		total += n.capacity
	}
	tenants := make([]model.Tenant, 0, len(tenantSpecs))
	for _, ts := range tenantSpecs {
		quotaPPM := fixed.PermilleToPPM(ts.quotaPermille).Value
		guaranteePPM := fixed.PerMyriadToPPM(ts.guaranteePerMyriad).Value
		quotaBytes := fixed.RateBytes(total, quotaPPM).Value
		guaranteeBytes := fixed.RateBytes(total, guaranteePPM).Value
		tenants = append(tenants, model.Tenant{
			ID: ts.id, RackID: ts.rack, HomeNodeID: ts.home, Priority: ts.priority, Weight: ts.weight,
			QuotaPPM: quotaPPM, GuaranteePPM: guaranteePPM,
			QuotaBytes: quotaBytes, GuaranteeBytes: guaranteeBytes,
			BurstFieldPresent: ts.burst > 0, BurstEnabled: ts.burst > 0, BurstCapBytes: ts.burst,
			BurstDegradation: burstDegradation(ts.burst),
			WindowMs:         1000, WindowTicks: 10, MaxWaitMs: 500, MaxWaitTicks: 5,
		})
	}
	events := make([]model.Event, 0, len(eventSpecs))
	for i, es := range eventSpecs {
		amount := es.amountMiB * MiB
		events = append(events, model.Event{
			ID: es.id, Tick: es.tick, Kind: es.kind, TenantID: es.tenant, NodeID: es.node,
			AmountBytes: amount, NewWeight: es.weight, SubmittedOrder: i,
		})
	}
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
	project := &model.Project{
		SchemaVersion: "canonical", ProjectID: name, Seed: seed, TickDurationMs: 100,
		Rules:   model.DefaultRules(),
		Nodes:   nodes,
		Tenants: tenants,
		Events:  events,
	}
	truth, err := BuildTruth(project)
	if err != nil {
		return nil, err
	}
	return &GeneratedProject{
		Name: name, Canonical: project,
		V1: ToV1(project), V2: ToV2(project), V3: ToV3(project), Truth: truth,
	}, nil
}

func burstDegradation(burst int64) string {
	if burst > 0 {
		return ""
	}
	return schema.BurstAbsentDegradation
}

func BuildTruth(project *model.Project) (*model.Truth, error) {
	result, err := engine.New(project).Run()
	if err != nil {
		return nil, err
	}
	return &model.Truth{Seed: project.Seed, ProjectFingerprint: engine.Fingerprint(project), Ticks: result.Reports}, nil
}

func ToV1(p *model.Project) *schema.V1Document {
	doc := &schema.V1Document{
		APIVersion: "quotakernel.local/v1alpha1", ProjectID: p.ProjectID, Seed: p.Seed,
		TickIntervalMs: p.TickDurationMs,
	}
	for _, n := range p.Nodes {
		doc.Nodes = append(doc.Nodes, schema.V1Node{NodeID: n.ID, Rack: n.RackID, CapacityBytes: n.CapacityBytes, AllowedTenants: n.AllowedTenants})
	}
	for _, t := range p.Tenants {
		qt := t.QuotaPPM / 1000
		gm := t.GuaranteePPM / 100
		tenant := schema.V1Tenant{
			TenantID: t.ID, Rack: t.RackID, HomeNodeID: t.HomeNodeID, Priority: t.Priority,
			SchedulingWeight: t.Weight, QuotaPermille: qt, GuaranteePerMyriad: gm,
			WindowMs: t.WindowMs, MaxQueueWaitMs: t.MaxWaitMs,
		}
		if t.BurstFieldPresent {
			v := t.BurstCapBytes
			tenant.BurstCapBytes = &v
		}
		doc.TenantsFlat = append(doc.TenantsFlat, tenant)
	}
	for _, e := range p.Events {
		doc.Events = append(doc.Events, schema.V1Event{
			EventID: e.ID, AtTick: int64(e.Tick), Type: v1EventType(e.Kind),
			TenantID: e.TenantID, NodeID: e.NodeID, AmountBytes: e.AmountBytes, NewWeight: e.NewWeight,
		})
	}
	return doc
}

func ToV2(p *model.Project) *schema.V2Document {
	doc := &schema.V2Document{
		APIVersion: "quotakernel.local/v2beta2", ProjectID: p.ProjectID, Seed: p.Seed,
		TickDurationMs: p.TickDurationMs,
	}
	rackIndex := map[string]int{}
	for _, r := range rackIDs(p) {
		rack := &schema.V2Rack{RackID: r}
		doc.RackTree = append(doc.RackTree, *rack)
		rackIndex[r] = len(doc.RackTree) - 1
	}
	for _, n := range p.Nodes {
		rack := &doc.RackTree[rackIndex[n.RackID]]
		rack.Nodes = append(rack.Nodes, schema.V2Node{
			NodeID: n.ID, CapacityKiB: n.CapacityBytes / fixed.KiB, AllowedTenantIDs: n.AllowedTenants,
		})
	}
	for _, t := range p.Tenants {
		rack := &doc.RackTree[rackIndex[t.RackID]]
		qt := t.QuotaPPM / 100
		gm := t.GuaranteePPM / 1000
		v2t := schema.V2Tenant{
			TenantID: t.ID, HomeNodeID: t.HomeNodeID, PriorityLevel: t.Priority, Weight: t.Weight,
			QuotaPerTenThousand: qt, GuaranteePerMille: gm,
			RateWindowMs: t.WindowMs, MaxWaitMillis: t.MaxWaitMs,
		}
		if t.BurstFieldPresent {
			v := t.BurstCapBytes / fixed.KiB
			v2t.MaxBurstKiB = &v
		}
		rack.TenantGroups = append(rack.TenantGroups, v2t)
	}
	for _, e := range p.Events {
		v2e := schema.V2Event{
			ID: e.ID, AtMillis: int64(e.Tick) * p.TickDurationMs, Kind: v2EventType(e.Kind),
			Tenant: e.TenantID, Node: e.NodeID, WeightValue: e.NewWeight,
		}
		if e.AmountBytes != 0 {
			v2e.AmountKiB = e.AmountBytes / fixed.KiB
		}
		doc.Timeline = append(doc.Timeline, v2e)
	}
	return doc
}

func ToV3(p *model.Project) *schema.V3Document {
	doc := &schema.V3Document{Schema: "quota-project/v3", Timeline: []schema.V3Event{}}
	doc.Metadata.ProjectName = p.ProjectID
	doc.Metadata.DeterministicSeed = p.Seed
	doc.Metadata.TickMs = p.TickDurationMs
	for _, n := range p.Nodes {
		doc.Cluster.StorageNodes = append(doc.Cluster.StorageNodes, schema.V3Node{
			Name: n.ID, RackName: n.RackID, ByteCapacity: n.CapacityBytes, TenantACL: n.AllowedTenants,
		})
	}
	for _, t := range p.Tenants {
		vt := schema.V3Tenant{
			ID: t.ID, RackName: t.RackID, PrimaryNode: t.HomeNodeID, PriorityRank: t.Priority,
			SchedWeight: t.Weight, QuotaMilli: t.QuotaPPM / 1000, AssuranceBips: t.GuaranteePPM / 100,
			WindowMilliseconds: t.WindowMs, WaitMilliseconds: t.MaxWaitMs,
		}
		if t.BurstFieldPresent {
			v := t.BurstCapBytes
			vt.BurstLimitBytes = &v
		}
		doc.Cluster.TenantTable = append(doc.Cluster.TenantTable, vt)
	}
	for _, e := range p.Events {
		doc.Timeline = append(doc.Timeline, schema.V3Event{
			EventName: e.ID, LogicalTick: int64(e.Tick), Action: v3EventType(e.Kind),
			TenantName: e.TenantID, TargetNode: e.NodeID, ByteCount: e.AmountBytes, Weight: e.NewWeight,
		})
	}
	return doc
}

// V3WithoutBurst returns a variant that omits one optional burst field. The
// loader must mark that tenant as not-enabled rather than silently using zero.
func V3WithoutBurst(p *model.Project, tenantID string) *schema.V3Document {
	doc := ToV3(p)
	for i := range doc.Cluster.TenantTable {
		if doc.Cluster.TenantTable[i].ID == tenantID {
			doc.Cluster.TenantTable[i].BurstLimitBytes = nil
		}
	}
	return doc
}

func CriticalDiagnostics() []model.ConversionDiagnostic {
	var capacity int64 = 1_024_000_000
	quotaPPM := int64(400_000)
	quota := fixed.RateBytes(int64(capacity), quotaPPM)
	guarantee := fixed.RateBytes(int64(capacity), 150_000)
	burst := fixed.KiBToBytes(20 * MiB / fixed.KiB)
	out := []model.ConversionDiagnostic{
		{TenantID: "critical-tenant", Tick: 1, FieldName: "quota_ppm_permille_400", Actual: fixed.PermilleToPPM(400).Value, Expected: 400_000, Check: fixed.CheckBound(fixed.PermilleToPPM(400), 400_000, 0)},
		{TenantID: "critical-tenant", Tick: 1, FieldName: "guarantee_ppm_permyriad_1500", Actual: fixed.PerMyriadToPPM(1500).Value, Expected: 150_000, Check: fixed.CheckBound(fixed.PerMyriadToPPM(1500), 150_000, 0)},
		{TenantID: "critical-tenant", Tick: 1, FieldName: "node_capacity_kib_to_bytes", Actual: fixed.KiBToBytes(1_000_000).Value, Expected: capacity, Check: fixed.CheckBound(fixed.KiBToBytes(1_000_000), int64(capacity), 0)},
		{TenantID: "critical-tenant", Tick: 1, FieldName: "available_quota_bytes", Actual: quota.Value, Expected: 409_600_000, Check: fixed.CheckBound(quota, 409_600_000, fixed.DefaultToleranceBytes)},
		{TenantID: "critical-tenant", Tick: 1, FieldName: "guarantee_remaining_bytes", Actual: guarantee.Value, Expected: 153_600_000, Check: fixed.CheckBound(guarantee, 153_600_000, fixed.DefaultToleranceBytes)},
		{TenantID: "critical-tenant", Tick: 1, FieldName: "burst_cap_bytes", Actual: burst.Value, Expected: 20 * MiB, Check: fixed.CheckBound(burst, 20*MiB, 0)},
	}

	overflowKiB := int64(math.MaxInt64/1024 + 1)
	overflow := fixed.KiBToBytes(overflowKiB)
	out = append(out, model.ConversionDiagnostic{
		TenantID: "critical-overflow", Tick: 2, FieldName: "capacity_kib_overflow_saturated",
		Actual: overflow.Value, Expected: math.MaxInt64,
		Check: fixed.CheckBound(overflow, math.MaxInt64, 0),
	})

	rounding := fixed.RateBytes(1, 1) // exact value is 0.000001 bytes, floors to zero
	out = append(out, model.ConversionDiagnostic{
		TenantID: "critical-rounding", Tick: 3, FieldName: "sub_byte_rate_floor",
		Actual: rounding.Value, Expected: 1,
		Check: fixed.CheckBound(rounding, 1, fixed.DefaultToleranceBytes),
	})

	drift := fixed.Result{Value: 100}
	out = append(out, model.ConversionDiagnostic{
		TenantID: "critical-drift", Tick: 4, FieldName: "injected_quota_bytes_drift_probe",
		Actual: 100, Expected: 103,
		Check: fixed.CheckBound(drift, 103, fixed.DefaultToleranceBytes),
	})
	return out
}

func rackIDs(p *model.Project) []string {
	set := map[string]struct{}{}
	for _, n := range p.Nodes {
		set[n.RackID] = struct{}{}
	}
	for _, t := range p.Tenants {
		set[t.RackID] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func v1EventType(k model.EventKind) string {
	switch k {
	case model.EventApply:
		return "capacity_request"
	case model.EventRelease:
		return "capacity_release"
	case model.EventBurst:
		return "burst_surge"
	case model.EventWeight:
		return "weight_adjustment"
	default:
		return string(k)
	}
}

func v2EventType(k model.EventKind) string { return string(k) }

func v3EventType(k model.EventKind) string {
	switch k {
	case model.EventApply:
		return "request"
	case model.EventRelease:
		return "free"
	case model.EventBurst:
		return "surge"
	case model.EventWeight:
		return "reweight"
	default:
		return string(k)
	}
}

var _ = fmt.Sprintf
