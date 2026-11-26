package schema

// V1Document uses snake_case fields, flat tenant table, byte capacities,
// per-mille quotas, per-ten-thousand guarantees and logical event ticks.
type V1Document struct {
	APIVersion     string     `json:"apiVersion"`
	ProjectID      string     `json:"project_id"`
	Seed           int64      `json:"seed"`
	TickIntervalMs int64      `json:"tick_interval_ms"`
	Nodes          []V1Node   `json:"nodes"`
	TenantsFlat    []V1Tenant `json:"tenants_flat"`
	Events         []V1Event  `json:"events"`
}

type V1Node struct {
	NodeID         string   `json:"node_id"`
	Rack           string   `json:"rack"`
	CapacityBytes  int64    `json:"capacity_bytes"`
	AllowedTenants []string `json:"allowed_tenants"`
}

type V1Tenant struct {
	TenantID           string `json:"tenant_id"`
	Rack               string `json:"rack"`
	HomeNodeID         string `json:"home_node_id"`
	Priority           int    `json:"priority"`
	SchedulingWeight   int64  `json:"scheduling_weight"`
	QuotaPermille      int64  `json:"quota_permille"`
	GuaranteePerMyriad int64  `json:"guarantee_permyriad"`
	BurstCapBytes      *int64 `json:"burst_cap_bytes,omitempty"`
	WindowMs           int64  `json:"window_ms"`
	MaxQueueWaitMs     int64  `json:"max_queue_wait_ms"`
}

type V1Event struct {
	EventID     string `json:"event_id"`
	AtTick      int64  `json:"at_tick"`
	Type        string `json:"type"`
	TenantID    string `json:"tenant_id"`
	NodeID      string `json:"node_id,omitempty"`
	AmountBytes int64  `json:"amount_bytes,omitempty"`
	NewWeight   int64  `json:"new_weight,omitempty"`
}

// V2Document uses camelCase fields, a rack-grouped nested tree, KiB
// capacities, per-ten-thousand quotas, per-mille guarantees and ms event time.
type V2Document struct {
	APIVersion     string    `json:"apiVersion"`
	ProjectID      string    `json:"projectId"`
	Seed           int64     `json:"seed"`
	TickDurationMs int64     `json:"tickDurationMs"`
	RackTree       []V2Rack  `json:"rackTree"`
	Timeline       []V2Event `json:"timeline"`
}

type V2Rack struct {
	RackID       string     `json:"rackId"`
	Nodes        []V2Node   `json:"nodes"`
	TenantGroups []V2Tenant `json:"tenantGroups"`
}

type V2Node struct {
	NodeID           string   `json:"nodeId"`
	CapacityKiB      int64    `json:"capacityKiB"`
	AllowedTenantIDs []string `json:"allowedTenantIds"`
}

type V2Tenant struct {
	TenantID            string `json:"tenantId"`
	HomeNodeID          string `json:"homeNodeId"`
	PriorityLevel       int    `json:"priorityLevel"`
	Weight              int64  `json:"weight"`
	QuotaPerTenThousand int64  `json:"quotaPerTenThousand"`
	GuaranteePerMille   int64  `json:"guaranteePerMille"`
	MaxBurstKiB         *int64 `json:"maxBurstKiB,omitempty"`
	RateWindowMs        int64  `json:"rateWindowMs"`
	MaxWaitMillis       int64  `json:"maxWaitMillis"`
}

type V2Event struct {
	ID          string `json:"id"`
	AtMillis    int64  `json:"atMillis"`
	Kind        string `json:"kind"`
	Tenant      string `json:"tenant"`
	Node        string `json:"node,omitempty"`
	AmountKiB   int64  `json:"amountKiB,omitempty"`
	WeightValue int64  `json:"weightValue,omitempty"`
}

// V3Document uses metadata/cluster envelopes, flat tenant table, bytes,
// mixed explicit scales and logical ticks.
type V3Document struct {
	Schema   string     `json:"schema"`
	Metadata V3Metadata `json:"metadata"`
	Cluster  V3Cluster  `json:"cluster"`
	Timeline []V3Event  `json:"timeline"`
}

type V3Metadata struct {
	ProjectName       string `json:"projectName"`
	DeterministicSeed int64  `json:"deterministicSeed"`
	TickMs            int64  `json:"tickMs"`
}

type V3Cluster struct {
	StorageNodes []V3Node   `json:"storage_nodes"`
	TenantTable  []V3Tenant `json:"tenant_table"`
}

type V3Node struct {
	Name         string   `json:"name"`
	RackName     string   `json:"rack_name"`
	ByteCapacity int64    `json:"byte_capacity"`
	TenantACL    []string `json:"tenant_acl"`
}

type V3Tenant struct {
	ID                 string `json:"id"`
	RackName           string `json:"rack_name"`
	PrimaryNode        string `json:"primary_node"`
	PriorityRank       int    `json:"priority_rank"`
	SchedWeight        int64  `json:"sched_weight"`
	QuotaMilli         int64  `json:"quota_milli"`
	AssuranceBips      int64  `json:"assurance_bips"`
	BurstLimitBytes    *int64 `json:"burst_limit_bytes,omitempty"`
	WindowMilliseconds int64  `json:"window_milliseconds"`
	WaitMilliseconds   int64  `json:"wait_milliseconds"`
}

type V3Event struct {
	EventName   string `json:"event_name"`
	LogicalTick int64  `json:"logical_tick"`
	Action      string `json:"action"`
	TenantName  string `json:"tenant_name"`
	TargetNode  string `json:"target_node,omitempty"`
	ByteCount   int64  `json:"byte_count,omitempty"`
	Weight      int64  `json:"weight,omitempty"`
}
