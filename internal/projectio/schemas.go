package projectio

import (
	"strings"

	"quota-cluster/internal/bizerror"
	"quota-cluster/internal/fixed"
	"quota-cluster/internal/model"
)

const defaultBurstPermilleOfQuota = 500

type policyDTOV1 struct {
	TickMillis        int64 `json:"tick_millis"`
	RatioBasisBytes   int64 `json:"ratio_basis_bytes"`
	ToleranceBytes    int64 `json:"tolerance_bytes"`
	DefaultBudgetMS   int64 `json:"default_budget_ms"`
	WorkerBudget      int   `json:"worker_budget"`
	TaskQueueCapacity int   `json:"task_queue_capacity"`
	TenantQueueLimit  int   `json:"tenant_queue_limit"`
	RetryBackoffTicks int64 `json:"retry_backoff_ticks"`
}

type docV1 struct {
	SchemaVersion string      `json:"schema_version"`
	ProjectID     string      `json:"project_id"`
	Seed          int64       `json:"seed"`
	LogicalTicks  int64       `json:"logical_ticks"`
	Policy        policyDTOV1 `json:"policy"`
	Nodes         []nodeV1    `json:"nodes"`
	Tenants       []tenantV1  `json:"tenants"`
	Events        []eventV1   `json:"events"`
}

type nodeV1 struct {
	ID             string   `json:"id"`
	Rack           string   `json:"rack"`
	CapacityBytes  int64    `json:"capacity_bytes"`
	AllowedTenants []string `json:"allowed_tenants"`
}

type tenantV1 struct {
	ID                string `json:"id"`
	Priority          int    `json:"priority"`
	InitialWeight     int64  `json:"initial_weight"`
	QuotaPermille     int64  `json:"quota_permille"`
	GuaranteePermille int64  `json:"guarantee_permille"`
	HomeNodeID        string `json:"home_node_id"`
}

type eventV1 struct {
	Sequence         int    `json:"sequence"`
	AtTick           int64  `json:"at_tick"`
	Type             string `json:"type"`
	RequestID        string `json:"request_id"`
	NodeID           string `json:"node_id"`
	TenantID         string `json:"tenant_id"`
	AmountBytes      int64  `json:"amount_bytes"`
	NewWeight        int64  `json:"new_weight"`
	DecisionBudgetMS int64  `json:"decision_budget_ms"`
	ReleaseRequestID string `json:"release_request_id"`
}

type docV2 struct {
	APIVersion        string     `json:"apiVersion"`
	ProjectName       string     `json:"projectName"`
	DeterministicSeed int64      `json:"deterministicSeed"`
	LogicalTickCount  int64      `json:"logicalTickCount"`
	Timing            timingV2   `json:"timing"`
	Scale             scaleV2    `json:"scale"`
	Runtime           runtimeV2  `json:"runtime"`
	Topology          topologyV2 `json:"topology"`
	TenantProfiles    []tenantV2 `json:"tenantProfiles"`
	EventLog          []eventV2  `json:"eventLog"`
}

type timingV2 struct {
	TickDurationMillis int64 `json:"tickDurationMillis"`
}

type scaleV2 struct {
	RatioBasisBytes int64 `json:"ratioBasisBytes"`
	ToleranceBytes  int64 `json:"toleranceBytes"`
}

type runtimeV2 struct {
	DefaultBudgetMillis int64 `json:"defaultBudgetMillis"`
	WorkerBudget        int   `json:"workerBudget"`
	TaskQueueCapacity   int   `json:"taskQueueCapacity"`
	TenantQueueLimit    int   `json:"tenantQueueLimit"`
	RetryBackoffTicks   int64 `json:"retryBackoffTicks"`
}

type topologyV2 struct {
	Racks []rackV2 `json:"racks"`
}

type rackV2 struct {
	RackID string   `json:"rackId"`
	Nodes  []nodeV2 `json:"nodes"`
}

type nodeV2 struct {
	NodeID          string   `json:"nodeId"`
	CapacityKiB     int64    `json:"capacityKiB"`
	TenantAllowlist []string `json:"tenantAllowlist"`
}

type tenantV2 struct {
	TenantID             string `json:"tenantId"`
	PriorityClass        int    `json:"priorityClass"`
	Weight               int64  `json:"weight"`
	QuotaBasisPoints     int64  `json:"quotaBasisPoints"`
	GuaranteeBasisPoints int64  `json:"guaranteeBasisPoints"`
	BurstLimitKiB        *int64 `json:"burstLimitKiB,omitempty"`
	HomeNodeID           string `json:"homeNodeId"`
}

type eventV2 struct {
	SequenceNumber       int    `json:"sequenceNumber"`
	AtMillis             int64  `json:"atMillis"`
	EventType            string `json:"eventType"`
	RequestID            string `json:"requestId"`
	NodeID               string `json:"nodeId"`
	TenantID             string `json:"tenantId"`
	AmountKiB            int64  `json:"amountKiB"`
	NewWeight            int64  `json:"newWeight"`
	DecisionBudgetMillis int64  `json:"decisionBudgetMillis"`
	ReleaseRequestID     string `json:"releaseRequestId"`
}

type docV3 struct {
	Spec            string     `json:"spec"`
	Metadata        metadataV3 `json:"metadata"`
	Configuration   configV3   `json:"configuration"`
	NodeInventory   []nodeV3   `json:"node_inventory"`
	TenantInventory []tenantV3 `json:"tenant_inventory"`
	Timeline        []eventV3  `json:"timeline"`
}

type metadataV3 struct {
	ID           string `json:"id"`
	Seed         int64  `json:"seed"`
	LogicalTicks int64  `json:"logical_ticks"`
}

type configV3 struct {
	TickMS            int64 `json:"tick_ms"`
	RatioBasisBytes   int64 `json:"ratio_basis_bytes"`
	ToleranceBytes    int64 `json:"tolerance_bytes"`
	DefaultBudgetMS   int64 `json:"default_budget_ms"`
	WorkerBudget      int   `json:"worker_budget"`
	TaskQueueCapacity int   `json:"task_queue_capacity"`
	TenantQueueLimit  int   `json:"tenant_queue_limit"`
	RetryBackoffTicks int64 `json:"retry_backoff_ticks"`
}

type nodeV3 struct {
	NodeRef         string   `json:"node_ref"`
	RackRef         string   `json:"rack_ref"`
	CapacityKiB     int64    `json:"capacity_kib"`
	TenantAllowlist []string `json:"tenant_allowlist"`
}

type tenantV3 struct {
	TenantRef          string `json:"tenant_ref"`
	PriorityRank       int    `json:"priority_rank"`
	BaseWeight         int64  `json:"base_weight"`
	QuotaPermyriad     int64  `json:"quota_permyriad"`
	GuaranteePermyriad int64  `json:"guarantee_permyriad"`
	BurstCapKiB        *int64 `json:"burst_cap_kib,omitempty"`
	HomeNodeRef        string `json:"home_node_ref"`
}

type eventV3 struct {
	Seq               int    `json:"seq"`
	TimestampMS       int64  `json:"timestamp_ms"`
	Action            string `json:"action"`
	RequestRef        string `json:"request_ref"`
	NodeRef           string `json:"node_ref"`
	TenantRef         string `json:"tenant_ref"`
	QuantityKiB       int64  `json:"quantity_kib"`
	WeightValue       int64  `json:"weight_value"`
	BudgetMS          int64  `json:"budget_ms"`
	ReleaseRequestRef string `json:"release_request_ref"`
}

func mapV1(doc *docV1) (*model.Project, error) {
	p := &model.Project{
		ProjectID:    doc.ProjectID,
		Seed:         doc.Seed,
		SourceSchema: model.SchemaV1,
		LogicalTicks: doc.LogicalTicks,
		Policy: model.Policy{
			TickMillis: doc.Policy.TickMillis, RatioBasisBytes: doc.Policy.RatioBasisBytes,
			ToleranceBytes: doc.Policy.ToleranceBytes, DefaultBudgetMS: doc.Policy.DefaultBudgetMS,
			WorkerBudget: doc.Policy.WorkerBudget, TaskQueueCapacity: doc.Policy.TaskQueueCapacity,
			TenantQueueLimit: doc.Policy.TenantQueueLimit, RetryBackoffTicks: doc.Policy.RetryBackoffTicks,
		},
	}
	if err := requireNonEmpty(p.ProjectID, "/project_id"); err != nil {
		return nil, err
	}
	if err := positive(p.LogicalTicks, "/logical_ticks"); err != nil {
		return nil, err
	}

	for i, n := range doc.Nodes {
		path := "/nodes/" + itoa(i)
		if err := requireNonEmpty(n.ID, path+"/id"); err != nil {
			return nil, err
		}
		if err := positive(n.CapacityBytes, path+"/capacity_bytes"); err != nil {
			return nil, err
		}
		if err := requireNonEmpty(n.Rack, path+"/rack"); err != nil {
			return nil, err
		}
		p.Nodes = append(p.Nodes, model.Node{ID: n.ID, Rack: n.Rack, CapacityBytes: n.CapacityBytes, AllowedTenants: append([]string(nil), n.AllowedTenants...)})
	}
	for i, t := range doc.Tenants {
		path := "/tenants/" + itoa(i)
		if err := requireNonEmpty(t.ID, path+"/id"); err != nil {
			return nil, err
		}
		quota := fixed.RatioToBytes(t.QuotaPermille, 1000, p.Policy.RatioBasisBytes)
		if err := checkOverflow(quota, path+"/quota_permille"); err != nil {
			return nil, err
		}
		guarantee := fixed.RatioToBytes(t.GuaranteePermille, 1000, p.Policy.RatioBasisBytes)
		if err := checkOverflow(guarantee, path+"/guarantee_permille"); err != nil {
			return nil, err
		}
		burst := fixed.RatioToBytes(defaultBurstPermilleOfQuota, 1000, quota.Value)
		if err := checkOverflow(burst, path+"/burst_default"); err != nil {
			return nil, err
		}
		p.Tenants = append(p.Tenants, model.Tenant{ID: t.ID, Priority: t.Priority, InitialWeight: t.InitialWeight, QuotaBytes: quota.Value, GuaranteeBytes: guarantee.Value, BurstLimitBytes: burst.Value, HomeNodeID: t.HomeNodeID})
	}
	for i, e := range doc.Events {
		ev, err := canonicalEvent(e.Sequence, e.AtTick, 0, p.Policy.TickMillis, e.Type, e.RequestID, e.NodeID, e.TenantID, e.AmountBytes, 0, e.NewWeight, e.DecisionBudgetMS, e.ReleaseRequestID, "/events/"+itoa(i))
		if err != nil {
			return nil, err
		}
		p.Events = append(p.Events, ev)
	}
	if err := validateProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func mapV2(doc *docV2) (*model.Project, error) {
	p := &model.Project{
		ProjectID: doc.ProjectName, Seed: doc.DeterministicSeed, SourceSchema: model.SchemaV2, LogicalTicks: doc.LogicalTickCount,
		Policy: model.Policy{TickMillis: doc.Timing.TickDurationMillis, RatioBasisBytes: doc.Scale.RatioBasisBytes, ToleranceBytes: doc.Scale.ToleranceBytes, DefaultBudgetMS: doc.Runtime.DefaultBudgetMillis, WorkerBudget: doc.Runtime.WorkerBudget, TaskQueueCapacity: doc.Runtime.TaskQueueCapacity, TenantQueueLimit: doc.Runtime.TenantQueueLimit, RetryBackoffTicks: doc.Runtime.RetryBackoffTicks},
	}
	if err := requireNonEmpty(p.ProjectID, "/projectName"); err != nil {
		return nil, err
	}
	for ri, rack := range doc.Topology.Racks {
		rpath := "/topology/racks/" + itoa(ri)
		if err := requireNonEmpty(rack.RackID, rpath+"/rackId"); err != nil {
			return nil, err
		}
		for ni, n := range rack.Nodes {
			path := rpath + "/nodes/" + itoa(ni)
			if err := requireNonEmpty(n.NodeID, path+"/nodeId"); err != nil {
				return nil, err
			}
			capacity := fixed.KiBToBytes(n.CapacityKiB)
			if err := checkOverflow(capacity, path+"/capacityKiB"); err != nil {
				return nil, err
			}
			p.Nodes = append(p.Nodes, model.Node{ID: n.NodeID, Rack: rack.RackID, CapacityBytes: capacity.Value, AllowedTenants: append([]string(nil), n.TenantAllowlist...)})
		}
	}
	for i, t := range doc.TenantProfiles {
		path := "/tenantProfiles/" + itoa(i)
		if err := requireNonEmpty(t.TenantID, path+"/tenantId"); err != nil {
			return nil, err
		}
		quota := fixed.RatioToBytes(t.QuotaBasisPoints, 10000, p.Policy.RatioBasisBytes)
		if err := checkOverflow(quota, path+"/quotaBasisPoints"); err != nil {
			return nil, err
		}
		guarantee := fixed.RatioToBytes(t.GuaranteeBasisPoints, 10000, p.Policy.RatioBasisBytes)
		if err := checkOverflow(guarantee, path+"/guaranteeBasisPoints"); err != nil {
			return nil, err
		}
		burst, err := burstFromKiB(t.BurstLimitKiB, quota.Value, path+"/burstLimitKiB")
		if err != nil {
			return nil, err
		}
		p.Tenants = append(p.Tenants, model.Tenant{ID: t.TenantID, Priority: t.PriorityClass, InitialWeight: t.Weight, QuotaBytes: quota.Value, GuaranteeBytes: guarantee.Value, BurstLimitBytes: burst, HomeNodeID: t.HomeNodeID})
	}
	for i, e := range doc.EventLog {
		amount := fixed.KiBToBytes(e.AmountKiB)
		if err := checkOverflow(amount, "/eventLog/"+itoa(i)+"/amountKiB"); err != nil {
			return nil, err
		}
		ev, err := canonicalEvent(e.SequenceNumber, 0, e.AtMillis, p.Policy.TickMillis, e.EventType, e.RequestID, e.NodeID, e.TenantID, amount.Value, e.AmountKiB, e.NewWeight, e.DecisionBudgetMillis, e.ReleaseRequestID, "/eventLog/"+itoa(i))
		if err != nil {
			return nil, err
		}
		p.Events = append(p.Events, ev)
	}
	if err := validateProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func mapV3(doc *docV3) (*model.Project, error) {
	p := &model.Project{
		ProjectID: doc.Metadata.ID, Seed: doc.Metadata.Seed, SourceSchema: model.SchemaV3, LogicalTicks: doc.Metadata.LogicalTicks,
		Policy: model.Policy{TickMillis: doc.Configuration.TickMS, RatioBasisBytes: doc.Configuration.RatioBasisBytes, ToleranceBytes: doc.Configuration.ToleranceBytes, DefaultBudgetMS: doc.Configuration.DefaultBudgetMS, WorkerBudget: doc.Configuration.WorkerBudget, TaskQueueCapacity: doc.Configuration.TaskQueueCapacity, TenantQueueLimit: doc.Configuration.TenantQueueLimit, RetryBackoffTicks: doc.Configuration.RetryBackoffTicks},
	}
	if err := requireNonEmpty(p.ProjectID, "/metadata/id"); err != nil {
		return nil, err
	}
	for i, n := range doc.NodeInventory {
		path := "/node_inventory/" + itoa(i)
		capacity := fixed.KiBToBytes(n.CapacityKiB)
		if err := checkOverflow(capacity, path+"/capacity_kib"); err != nil {
			return nil, err
		}
		p.Nodes = append(p.Nodes, model.Node{ID: n.NodeRef, Rack: n.RackRef, CapacityBytes: capacity.Value, AllowedTenants: append([]string(nil), n.TenantAllowlist...)})
	}
	for i, t := range doc.TenantInventory {
		path := "/tenant_inventory/" + itoa(i)
		quota := fixed.RatioToBytes(t.QuotaPermyriad, 10000, p.Policy.RatioBasisBytes)
		if err := checkOverflow(quota, path+"/quota_permyriad"); err != nil {
			return nil, err
		}
		guarantee := fixed.RatioToBytes(t.GuaranteePermyriad, 10000, p.Policy.RatioBasisBytes)
		if err := checkOverflow(guarantee, path+"/guarantee_permyriad"); err != nil {
			return nil, err
		}
		burst, err := burstFromKiB(t.BurstCapKiB, quota.Value, path+"/burst_cap_kib")
		if err != nil {
			return nil, err
		}
		p.Tenants = append(p.Tenants, model.Tenant{ID: t.TenantRef, Priority: t.PriorityRank, InitialWeight: t.BaseWeight, QuotaBytes: quota.Value, GuaranteeBytes: guarantee.Value, BurstLimitBytes: burst, HomeNodeID: t.HomeNodeRef})
	}
	for i, e := range doc.Timeline {
		amount := fixed.KiBToBytes(e.QuantityKiB)
		if err := checkOverflow(amount, "/timeline/"+itoa(i)+"/quantity_kib"); err != nil {
			return nil, err
		}
		ev, err := canonicalEvent(e.Seq, 0, e.TimestampMS, p.Policy.TickMillis, e.Action, e.RequestRef, e.NodeRef, e.TenantRef, amount.Value, e.QuantityKiB, e.WeightValue, e.BudgetMS, e.ReleaseRequestRef, "/timeline/"+itoa(i))
		if err != nil {
			return nil, err
		}
		p.Events = append(p.Events, ev)
	}
	if err := validateProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func burstFromKiB(kib *int64, quotaBytes int64, path string) (int64, error) {
	if kib == nil {
		r := fixed.RatioToBytes(defaultBurstPermilleOfQuota, 1000, quotaBytes)
		return r.Value, checkOverflow(r, path)
	}
	r := fixed.KiBToBytes(*kib)
	return r.Value, checkOverflow(r, path)
}

func canonicalEvent(sequence int, tick int64, millis int64, tickMillis int64, kind string, requestID, nodeID, tenantID string, amountBytes int64, amountKiB int64, weight, budgetMS int64, releaseRequestID, path string) (model.Event, error) {
	if sequence <= 0 {
		return model.Event{}, bizerror.New(bizerror.CodeFieldRequired, path+"/sequence", "positive sequence is required")
	}
	if tick == 0 && millis > 0 {
		if err := positive(tickMillis, "/policy/tick_millis"); err != nil {
			return model.Event{}, err
		}
		if millis%tickMillis != 0 {
			return model.Event{}, bizerror.New(bizerror.CodeValueOutOfRange, path+"/time", "millisecond timestamp %d is not aligned to tick duration %d", millis, tickMillis)
		}
		tick = millis / tickMillis
	}
	normalizedKind, err := parseKind(kind, path+"/type")
	if err != nil {
		return model.Event{}, err
	}
	ev := model.Event{Sequence: sequence, Tick: tick, Kind: normalizedKind, RequestID: requestID, NodeID: nodeID, TenantID: tenantID, AmountBytes: amountBytes, Weight: weight, LatencyBudgetMS: budgetMS, ReleaseRequestID: releaseRequestID}
	if normalizedKind == model.EventApply {
		if err := requireNonEmpty(requestID, path+"/request_id"); err != nil {
			return ev, err
		}
		if err := requireNonEmpty(nodeID, path+"/node_id"); err != nil {
			return ev, err
		}
		if err := requireNonEmpty(tenantID, path+"/tenant_id"); err != nil {
			return ev, err
		}
		if amountBytes <= 0 {
			return ev, bizerror.New(bizerror.CodeValueOutOfRange, path+"/amount", "apply amount must be positive")
		}
		if budgetMS <= 0 {
			ev.LatencyBudgetMS = 5
		}
	}
	if normalizedKind == model.EventRelease {
		if err := requireNonEmpty(releaseRequestID, path+"/release_request_id"); err != nil {
			return ev, err
		}
	}
	if normalizedKind == model.EventWeight {
		if err := requireNonEmpty(tenantID, path+"/tenant_id"); err != nil {
			return ev, err
		}
		if weight <= 0 {
			return ev, bizerror.New(bizerror.CodeValueOutOfRange, path+"/new_weight", "weight must be positive")
		}
	}
	return ev, nil
}

func parseKind(kind, path string) (model.EventKind, error) {
	switch strings.ToUpper(strings.TrimSpace(kind)) {
	case "APPLY", "APPLICATION", "REQUEST", "ALLOCATE", "容量申请":
		return model.EventApply, nil
	case "RELEASE", "FREE", "RELEASED":
		return model.EventRelease, nil
	case "WEIGHT_ADJUST", "WEIGHT", "ADJUST_WEIGHT", "WEIGHT_CHANGE":
		return model.EventWeight, nil
	default:
		return "", bizerror.New(bizerror.CodeFieldInvalidType, path, "unknown event kind %q", kind)
	}
}

func itoa(i int) string { return strings.TrimSpace(strings.Repeat("0", 0)) + intToString(i) }

func intToString(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
