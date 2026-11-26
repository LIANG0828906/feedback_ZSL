// Package model contains the schema-independent quota domain model.
package model

const (
	SchemaV1 = "quota.project.v1"
	SchemaV2 = "quota.project.v2"
	SchemaV3 = "quota.project.v3"

	FormatVersion = "checkpoint-v1"
)

type EventKind string

const (
	EventApply   EventKind = "APPLY"
	EventRelease EventKind = "RELEASE"
	EventWeight  EventKind = "WEIGHT_ADJUST"
)

type Verdict string

const (
	VerdictAllow   Verdict = "ALLOW"
	VerdictQueue   Verdict = "QUEUE"
	VerdictReject  Verdict = "REJECT"
	VerdictPending Verdict = "PENDING"
)

// Node is a storage node after all schema unit and topology differences are
// normalized.
type Node struct {
	ID             string   `json:"id"`
	Rack           string   `json:"rack"`
	CapacityBytes  int64    `json:"capacity_bytes"`
	AllowedTenants []string `json:"allowed_tenants"`
}

// Tenant is a quota principal in canonical byte units.
type Tenant struct {
	ID              string `json:"id"`
	Priority        int    `json:"priority"`
	InitialWeight   int64  `json:"initial_weight"`
	QuotaBytes      int64  `json:"quota_bytes"`
	GuaranteeBytes  int64  `json:"guarantee_bytes"`
	BurstLimitBytes int64  `json:"burst_limit_bytes"`
	HomeNodeID      string `json:"home_node_id,omitempty"`
}

// Event is one ordered logical-time input.
type Event struct {
	Sequence         int       `json:"sequence"`
	Tick             int64     `json:"tick"`
	Kind             EventKind `json:"kind"`
	RequestID        string    `json:"request_id,omitempty"`
	NodeID           string    `json:"node_id,omitempty"`
	TenantID         string    `json:"tenant_id,omitempty"`
	AmountBytes      int64     `json:"amount_bytes,omitempty"`
	Weight           int64     `json:"weight,omitempty"`
	LatencyBudgetMS  int64     `json:"latency_budget_ms,omitempty"`
	ReleaseRequestID string    `json:"release_request_id,omitempty"`
}

// Policy contains deterministic runtime bounds and declared verification
// tolerances.
type Policy struct {
	TickMillis        int64 `json:"tick_millis"`
	RatioBasisBytes   int64 `json:"ratio_basis_bytes"`
	ToleranceBytes    int64 `json:"tolerance_bytes"`
	DefaultBudgetMS   int64 `json:"default_budget_ms"`
	FastCostMS        int64 `json:"fast_cost_ms"`
	PreciseCostMS     int64 `json:"precise_cost_ms"`
	WorkerBudget      int   `json:"worker_budget"`
	TaskQueueCapacity int   `json:"task_queue_capacity"`
	TenantQueueLimit  int   `json:"tenant_queue_limit"`
	RetryBackoffTicks int64 `json:"retry_backoff_ticks"`
}

// EditKind identifies an incremental declaration/event mutation.
type EditKind string

const (
	EditNodeCapacity    EditKind = "NODE_CAPACITY"
	EditTenantQuota     EditKind = "TENANT_QUOTA"
	EditTenantGuarantee EditKind = "TENANT_GUARANTEE"
	EditWeightEvent     EditKind = "WEIGHT_EVENT"
)

// Edit is a deterministic mutation used by incremental invalidation.
type Edit struct {
	Kind          EditKind `json:"kind"`
	TargetID      string   `json:"target_id,omitempty"`
	EventSequence int      `json:"event_sequence,omitempty"`
	AtTick        int64    `json:"at_tick"`
	Value         int64    `json:"value"`
}

// Project is the canonical internal engineering declaration.
type Project struct {
	ProjectID    string   `json:"project_id"`
	Seed         int64    `json:"seed"`
	SourceSchema string   `json:"source_schema,omitempty"`
	LogicalTicks int64    `json:"logical_ticks"`
	Policy       Policy   `json:"policy"`
	Nodes        []Node   `json:"nodes"`
	Tenants      []Tenant `json:"tenants"`
	Events       []Event  `json:"events"`
}

// QueueMember is a deterministic wait-queue entry.
type QueueMember struct {
	RequestID          string `json:"request_id"`
	TenantID           string `json:"tenant_id"`
	NodeID             string `json:"node_id"`
	RemainingBytes     int64  `json:"remaining_bytes"`
	OriginalBytes      int64  `json:"original_bytes"`
	EnqueuedTick       int64  `json:"enqueued_tick"`
	Position           int    `json:"position"`
	Priority           int    `json:"priority"`
	Weight             int64  `json:"weight"`
	GuaranteeEligible  bool   `json:"guarantee_eligible"`
	EncroacherTenantID string `json:"encroacher_tenant_id,omitempty"`
	Constraint         string `json:"constraint,omitempty"`
}

// TenantSnapshot is the per-logical-tick ledger truth for one tenant.
type TenantSnapshot struct {
	TenantID                string        `json:"tenant_id"`
	Weight                  int64         `json:"weight"`
	AvailableQuotaBytes     int64         `json:"available_quota_bytes"`
	UsedBytes               int64         `json:"used_bytes"`
	QuotaRemainingBytes     int64         `json:"quota_remaining_bytes"`
	UsableRemainingBytes    int64         `json:"usable_remaining_bytes"`
	GuaranteeBytes          int64         `json:"guarantee_bytes"`
	GuaranteeRemainingBytes int64         `json:"guarantee_remaining_bytes"`
	BurstLimitBytes         int64         `json:"burst_limit_bytes"`
	BurstUsedBytes          int64         `json:"burst_used_bytes"`
	HeldBytes               int64         `json:"held_bytes"`
	QueuedBytes             int64         `json:"queued_bytes"`
	Queue                   []QueueMember `json:"queue"`
}

// NodeSnapshot reports node occupancy and pre-deduction holds.
type NodeSnapshot struct {
	NodeID        string `json:"node_id"`
	CapacityBytes int64  `json:"capacity_bytes"`
	UsedBytes     int64  `json:"used_bytes"`
	HeldBytes     int64  `json:"held_bytes"`
	FreeBytes     int64  `json:"free_bytes"`
}

// Decision is one admission/gate outcome with two-phase accounting values.
type Decision struct {
	RequestID            string  `json:"request_id"`
	Tick                 int64   `json:"tick"`
	Sequence             int     `json:"sequence"`
	TenantID             string  `json:"tenant_id"`
	NodeID               string  `json:"node_id"`
	AskedBytes           int64   `json:"asked_bytes"`
	AdmittedBytes        int64   `json:"admitted_bytes"`
	QueuedBytes          int64   `json:"queued_bytes"`
	Verdict              Verdict `json:"verdict"`
	PreheldBytes         int64   `json:"preheld_bytes"`
	SettlementCorrection int64   `json:"settlement_correction_bytes"`
	LatencyMS            int64   `json:"latency_ms"`
	BudgetMS             int64   `json:"budget_ms"`
	ReasonCode           string  `json:"reason_code,omitempty"`
	Constraint           string  `json:"constraint,omitempty"`
	EncroacherTenantID   string  `json:"encroacher_tenant_id,omitempty"`
	RetryTick            int64   `json:"retry_tick,omitempty"`
	OrderKey             string  `json:"order_key"`
}

// TickReport contains all state observable after one logical tick.
type TickReport struct {
	Tick          int64                     `json:"tick"`
	TenantStates  map[string]TenantSnapshot `json:"tenant_states"`
	NodeStates    map[string]NodeSnapshot   `json:"node_states"`
	Decisions     []Decision                `json:"decisions"`
	Queue         []QueueMember             `json:"queue"`
	AppliedEvents []int                     `json:"applied_event_sequences"`
}

// Report is a full deterministic run result.
type Report struct {
	ProjectID    string       `json:"project_id"`
	Seed         int64        `json:"seed"`
	FinalTick    int64        `json:"final_tick"`
	Ticks        []TickReport `json:"ticks"`
	Decisions    []Decision   `json:"decisions"`
	WorkerBudget int          `json:"worker_budget"`
	BatchSize    int          `json:"batch_size"`
}

// TruthFile is generated alongside the three schema documents.
type TruthFile struct {
	SchemaVersion  string       `json:"schema_version"`
	Seed           int64        `json:"seed"`
	ProjectID      string       `json:"project_id"`
	ToleranceBytes int64        `json:"tolerance_bytes"`
	Ticks          []TickReport `json:"ticks"`
	Decisions      []Decision   `json:"decisions"`
	FairnessOrder  []string     `json:"fairness_order"`
}
