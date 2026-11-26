// Package model contains the version-independent quota kernel domain model.
package model

import "quotakernel/fixed"

type SchemaVersion string

const (
	SchemaV1 SchemaVersion = "v1"
	SchemaV2 SchemaVersion = "v2"
	SchemaV3 SchemaVersion = "v3"
)

type Tick int64

type EventKind string

const (
	EventApply   EventKind = "apply"
	EventRelease EventKind = "release"
	EventBurst   EventKind = "burst"
	EventWeight  EventKind = "weight_adjust"
)

const (
	ClassGuarantee = "guarantee"
	ClassQuota     = "quota"
	ClassBurst     = "burst"
)

// Declared constraint identifiers make queue/rejection diagnostics locatable.
const (
	ConstraintQuotaHard         = "C-QUOTA-HARD"
	ConstraintGuaranteeReserve  = "C-GUARANTEE-RESERVE"
	ConstraintNodeCapacity      = "C-NODE-CAPACITY"
	ConstraintLatencyBudget     = "C-LATENCY-BUDGET"
	ConstraintReserveCover      = "C-RESERVE-COVER"
	ConstraintQueueBound        = "C-QUEUE-BOUND"
	ConstraintWaitTTL           = "C-WAIT-TTL"
	ConstraintSingleNode        = "C-TENANT-SINGLE-NODE"
	ConstraintNodeACL           = "C-NODE-ACL"
	ConstraintFixedOverflow     = "C-FIXED-OVERFLOW"
	ConstraintCheckpointVersion = "C-CHECKPOINT-VERSION"
)

// Observable business codes. Backpressure codes are stable and must never be
// represented as an ordinary admission.
const (
	CodeAdmitted                 = "OK_ADMITTED"
	CodeQueued                   = "OK_QUEUED"
	CodeRejectedQuota            = "REJECT_QUOTA_HARD"
	CodeRejectedNode             = "REJECT_NODE_CAPACITY"
	CodeRejectedSingleNode       = "REJECT_TENANT_SINGLE_NODE"
	CodeWaitTTLExceeded          = "REJECT_WAIT_TTL"
	CodeBackpressureQueueFull    = "BP_TENANT_QUEUE_FULL"
	CodeBackpressureWorkers      = "BP_WORKER_BUDGET_EXHAUSTED"
	CodeGateLatencyDegradedQueue = "GATE_LATENCY_DEGRADED_QUEUE"
	CodeVersionMismatch          = "RECOVERY_VERSION_MISMATCH"
)

type Rules struct {
	FixedToleranceBytes int64 `json:"fixedToleranceBytes"`
	GateBudgetNs        int64 `json:"gateBudgetNs"`
	WorkerBudget        int   `json:"workerBudget"`
	MaxQueuePerTenant   int   `json:"maxQueuePerTenant"`
	MaxWaitTicks        Tick  `json:"maxWaitTicks"`
}

func DefaultRules() Rules {
	return Rules{
		FixedToleranceBytes: fixed.DefaultToleranceBytes,
		GateBudgetNs:        int64(10_000_000), // 10 ms declared single-decision budget
		WorkerBudget:        4,
		MaxQueuePerTenant:   8,
		MaxWaitTicks:        5,
	}
}

type Node struct {
	ID             string   `json:"id"`
	RackID         string   `json:"rackId"`
	CapacityBytes  int64    `json:"capacityBytes"`
	AllowedTenants []string `json:"allowedTenants"`
}

type Tenant struct {
	ID             string `json:"id"`
	RackID         string `json:"rackId"`
	HomeNodeID     string `json:"homeNodeId"`
	Priority       int    `json:"priority"`
	Weight         int64  `json:"weight"`
	QuotaPPM       int64  `json:"quotaPpm"`
	GuaranteePPM   int64  `json:"guaranteePpm"`
	QuotaBytes     int64  `json:"quotaBytes"`
	GuaranteeBytes int64  `json:"guaranteeBytes"`
	BurstCapBytes  int64  `json:"burstCapBytes"`

	// BurstFieldPresent distinguishes an explicit zero from an omitted field.
	BurstFieldPresent bool   `json:"burstFieldPresent"`
	BurstEnabled      bool   `json:"burstEnabled"`
	BurstDegradation  string `json:"burstDegradation,omitempty"`

	WindowMs     int64 `json:"windowMs"`
	WindowTicks  int64 `json:"windowTicks"`
	MaxWaitMs    int64 `json:"maxWaitMs"`
	MaxWaitTicks Tick  `json:"maxWaitTicks"`
}

type Event struct {
	ID             string    `json:"id"`
	Tick           Tick      `json:"tick"`
	Kind           EventKind `json:"kind"`
	TenantID       string    `json:"tenantId"`
	NodeID         string    `json:"nodeId,omitempty"`
	AmountBytes    int64     `json:"amountBytes,omitempty"`
	NewWeight      int64     `json:"newWeight,omitempty"`
	SubmittedOrder int       `json:"submittedOrder"`
}

type Project struct {
	SchemaVersion  SchemaVersion `json:"schemaVersion"`
	ProjectID      string        `json:"projectId"`
	Seed           int64         `json:"seed"`
	TickDurationMs int64         `json:"tickDurationMs"`
	Rules          Rules         `json:"rules"`
	Nodes          []Node        `json:"nodes"`
	Tenants        []Tenant      `json:"tenants"`
	Events         []Event       `json:"events"`
}

// FingerprintCanonical is the version under which persisted state is valid.
const StateFormatVersion = 1

type TenantState struct {
	Tenant
	UsedBytes      int64  `json:"usedBytes"`
	QuotaUsedBytes int64  `json:"quotaUsedBytes"`
	BurstUsedBytes int64  `json:"burstUsedBytes"`
	PinnedNodeID   string `json:"pinnedNodeId,omitempty"`
}

type NodeState struct {
	Node
	UsedBytes        int64 `json:"usedBytes"`
	PreDeductedBytes int64 `json:"preDeductedBytes"`
}

type QueueItem struct {
	ID                   string    `json:"id"`
	EventID              string    `json:"eventId"`
	Kind                 EventKind `json:"kind"`
	TenantID             string    `json:"tenantId"`
	NodeID               string    `json:"nodeId"`
	OriginalAmountBytes  int64     `json:"originalAmountBytes"`
	RemainingAmountBytes int64     `json:"remainingAmountBytes"`
	EnqueuedTick         Tick      `json:"enqueuedTick"`
	LastAttemptTick      Tick      `json:"lastAttemptTick"`
	ExpiresTick          Tick      `json:"expiresTick"`
	Priority             int       `json:"priority"`
	Weight               int64     `json:"weight"`
	GuaranteeBytes       int64     `json:"guaranteeBytes"`
	ReasonCode           string    `json:"reasonCode"`
	ConstraintID         string    `json:"constraintId"`
	EncroachingTenantID  string    `json:"encroachingTenantId,omitempty"`
	VictimTenantID       string    `json:"victimTenantId,omitempty"`
}

type Allocation struct {
	Class           string `json:"class"`
	Bytes           int64  `json:"bytes"`
	NodeUsedAfter   int64  `json:"nodeUsedAfter"`
	TenantUsedAfter int64  `json:"tenantUsedAfter"`
}

type Admission struct {
	EventID        string       `json:"eventId"`
	TenantID       string       `json:"tenantId"`
	NodeID         string       `json:"nodeId"`
	Kind           EventKind    `json:"kind"`
	RequestedBytes int64        `json:"requestedBytes"`
	AdmittedBytes  int64        `json:"admittedBytes"`
	QueuedBytes    int64        `json:"queuedBytes"`
	RejectedBytes  int64        `json:"rejectedBytes"`
	FullyAdmitted  bool         `json:"fullyAdmitted"`
	Tranches       []Allocation `json:"tranches"`
	GateConclusion string       `json:"gateConclusion"`
}

type ReleaseRecord struct {
	EventID            string `json:"eventId"`
	TenantID           string `json:"tenantId"`
	NodeID             string `json:"nodeId"`
	ReleasedBytes      int64  `json:"releasedBytes"`
	QuotaReleasedBytes int64  `json:"quotaReleasedBytes"`
	BurstReleasedBytes int64  `json:"burstReleasedBytes"`
}

type Rejection struct {
	EventID             string `json:"eventId"`
	TenantID            string `json:"tenantId"`
	NodeID              string `json:"nodeId,omitempty"`
	RequestedBytes      int64  `json:"requestedBytes"`
	RejectedBytes       int64  `json:"rejectedBytes"`
	Code                string `json:"code"`
	Reason              string `json:"reason"`
	ConstraintID        string `json:"constraintId"`
	EncroachingTenantID string `json:"encroachingTenantId,omitempty"`
	VictimTenantID      string `json:"victimTenantId,omitempty"`
	SuggestedRetryTick  Tick   `json:"suggestedRetryTick"`
}

type TenantSnapshot struct {
	TenantID                string `json:"tenantId"`
	Priority                int    `json:"priority"`
	Weight                  int64  `json:"weight"`
	QuotaBytes              int64  `json:"quotaBytes"`
	GuaranteeBytes          int64  `json:"guaranteeBytes"`
	BurstCapBytes           int64  `json:"burstCapBytes"`
	BurstEnabled            bool   `json:"burstEnabled"`
	UsedBytes               int64  `json:"usedBytes"`
	QuotaUsedBytes          int64  `json:"quotaUsedBytes"`
	BurstUsedBytes          int64  `json:"burstUsedBytes"`
	AvailableQuotaBytes     int64  `json:"availableQuotaBytes"`
	GuaranteeRemainingBytes int64  `json:"guaranteeRemainingBytes"`
	BurstRemainingBytes     int64  `json:"burstRemainingBytes"`
	PinnedNodeID            string `json:"pinnedNodeId,omitempty"`
	QueueLength             int    `json:"queueLength"`
}

type NodeSnapshot struct {
	NodeID                  string `json:"nodeId"`
	CapacityBytes           int64  `json:"capacityBytes"`
	UsedBytes               int64  `json:"usedBytes"`
	AvailableBytes          int64  `json:"availableBytes"`
	PreDeductedBytes        int64  `json:"preDeductedBytes"`
	ProtectedGuaranteeBytes int64  `json:"protectedGuaranteeBytes"`
}

type GateRecord struct {
	DecisionID          string `json:"decisionId"`
	EventID             string `json:"eventId"`
	Tick                Tick   `json:"tick"`
	TenantID            string `json:"tenantId"`
	NodeID              string `json:"nodeId"`
	Conclusion          string `json:"conclusion"`
	FastConclusion      string `json:"fastConclusion"`
	ReasonCode          string `json:"reasonCode"`
	ConstraintID        string `json:"constraintId,omitempty"`
	FastPath            bool   `json:"fastPath"`
	Reconciled          bool   `json:"reconciled"`
	BudgetNs            int64  `json:"budgetNs"`
	NodeReserveBytes    int64  `json:"nodeReserveBytes"`
	TenantSafeRemaining int64  `json:"tenantSafeRemainingBytes"`
}

type ReserveRecord struct {
	DecisionID    string `json:"decisionId"`
	EventID       string `json:"eventId"`
	Tick          Tick   `json:"tick"`
	TenantID      string `json:"tenantId"`
	NodeID        string `json:"nodeId"`
	ReservedBytes int64  `json:"reservedBytes"`
	SettledBytes  int64  `json:"settledBytes"`
	DeltaBytes    int64  `json:"deltaBytes"`
	Status        string `json:"status"`
}

type Reconciliation struct {
	Records       []ReserveRecord `json:"records"`
	TotalReserved int64           `json:"totalReservedBytes"`
	TotalSettled  int64           `json:"totalSettledBytes"`
	TotalDelta    int64           `json:"totalDeltaBytes"`
	Closed        bool            `json:"closed"`
}

type Dependencies struct {
	TenantIDs []string `json:"tenantIds"`
	NodeIDs   []string `json:"nodeIds"`
	EventIDs  []string `json:"eventIds"`
}

type TickReport struct {
	ProjectID      string           `json:"projectId"`
	Seed           int64            `json:"seed"`
	Tick           Tick             `json:"tick"`
	CanonicalOrder []string         `json:"canonicalOrder"`
	Admissions     []Admission      `json:"admissions"`
	Releases       []ReleaseRecord  `json:"releases"`
	Queue          []QueueItem      `json:"queue"`
	Rejections     []Rejection      `json:"rejections"`
	Gate           []GateRecord     `json:"gate"`
	Ledgers        []TenantSnapshot `json:"ledgers"`
	Nodes          []NodeSnapshot   `json:"nodes"`
	Reconciliation Reconciliation   `json:"reconciliation"`
	Dependencies   Dependencies     `json:"dependencies"`
	StateHash      string           `json:"stateHash"`
}

type Snapshot struct {
	FormatVersion      int             `json:"formatVersion"`
	ProjectID          string          `json:"projectId"`
	ProjectFingerprint string          `json:"projectFingerprint"`
	CommittedTick      Tick            `json:"committedTick"`
	Tenants            []TenantState   `json:"tenants"`
	Nodes              []NodeState     `json:"nodes"`
	Queue              []QueueItem     `json:"queue"`
	OpenReserves       []ReserveRecord `json:"openReserves"`
	StateHash          string          `json:"stateHash"`
}

type Truth struct {
	Seed               int64        `json:"seed"`
	ProjectFingerprint string       `json:"projectFingerprint"`
	Ticks              []TickReport `json:"ticks"`
}

type ConversionDiagnostic struct {
	TenantID  string       `json:"tenantId"`
	Tick      Tick         `json:"tick,omitempty"`
	FieldName string       `json:"fieldName"`
	Expected  int64        `json:"expected"`
	Actual    int64        `json:"actual"`
	Check     fixed.Result `json:"check"`
}
