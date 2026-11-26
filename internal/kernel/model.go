package kernel

import "sort"

type ID string

type SchemaVersion string

const (
	SchemaV1 SchemaVersion = "v1"
	SchemaV2 SchemaVersion = "v2"
	SchemaV3 SchemaVersion = "v3"

	EventTypeApply   = "apply"
	EventTypeRelease = "release"
	EventTypeWeight  = "weight"

	DecisionAdmit   = "admit"
	DecisionQueue   = "queue"
	DecisionReject  = "reject"
	DecisionPending = "pending"

	PathExact     = "exact"
	PathPreDeduct = "pre_deduct"
	PathParallel  = "parallel"
)

type Project struct {
	SchemaVersion SchemaVersion `json:"schema_version"`
	ProjectID     string        `json:"project_id"`
	Seed          int64         `json:"seed"`
	TickMS        int64         `json:"tick_ms"`
	Policy        FixedPolicy   `json:"fixed_policy"`
	Nodes         []Node        `json:"nodes"`
	Tenants       []Tenant      `json:"tenants"`
	Events        []Event       `json:"events"`
}

type Node struct {
	ID             ID     `json:"id"`
	Rack           string `json:"rack"`
	CapacityBytes  int64  `json:"capacity_bytes"`
	AllowedTenants []ID   `json:"allowed_tenants"`
}

type Tenant struct {
	ID              ID    `json:"id"`
	Priority        int   `json:"priority"`
	WeightPermyriad int64 `json:"weight_permyriad"`
	QuotaBytes      int64 `json:"quota_bytes"`
	GuaranteeBytes  int64 `json:"guarantee_bytes"`
	BurstLimitBytes int64 `json:"burst_limit_bytes"`
	GuaranteeNodeID ID    `json:"guarantee_node_id"`
}

type Event struct {
	ID              string   `json:"id"`
	Tick            int64    `json:"tick"`
	Type            string   `json:"type"`
	TenantID        ID       `json:"tenant_id,omitempty"`
	NodeID          ID       `json:"node_id,omitempty"`
	AmountBytes     int64    `json:"amount_bytes,omitempty"`
	WeightPermyriad int64    `json:"weight_permyriad,omitempty"`
	BudgetMS        int64    `json:"decision_budget_ms,omitempty"`
	Refs            []string `json:"causal_refs,omitempty"`
}

type Budgets struct {
	Workers         int  `json:"workers"`
	PerTenantQueue  int  `json:"per_tenant_queue"`
	FailFastWorkers bool `json:"fail_fast_workers"`
}

func DefaultBudgets() Budgets {
	return Budgets{Workers: 4, PerTenantQueue: 32}
}

func (b Budgets) Validate() error {
	if b.Workers < 1 || b.Workers > 64 {
		return NewBizError(CodeWorkerBudget, "workers must be in [1,64], got %d", b.Workers).
			WithDetail("min", 1).WithDetail("max", 64)
	}
	if b.PerTenantQueue < 1 || b.PerTenantQueue > 4096 {
		return NewBizError(CodeQueueBudget, "per-tenant queue must be in [1,4096], got %d", b.PerTenantQueue).
			WithDetail("min", 1).WithDetail("max", 4096)
	}
	return nil
}

type TenantState struct {
	TenantID                ID    `json:"tenant_id"`
	Tick                    int64 `json:"tick"`
	QuotaBytes              int64 `json:"quota_bytes"`
	GuaranteeBytes          int64 `json:"guarantee_bytes"`
	BurstLimitBytes         int64 `json:"burst_limit_bytes"`
	UsedBytes               int64 `json:"used_bytes"`
	HeldBytes               int64 `json:"held_bytes"`
	AvailableQuotaBytes     int64 `json:"available_quota_bytes"`
	GuaranteeRemainingBytes int64 `json:"guarantee_remaining_bytes"`
	BurstRemainingBytes     int64 `json:"burst_remaining_bytes"`
	HardHeadroomBytes       int64 `json:"hard_headroom_bytes"`
	EffectivePriority       int   `json:"effective_priority"`
	EffectiveWeightBP       int64 `json:"effective_weight_bp"`
}

type NodeState struct {
	NodeID                  ID    `json:"node_id"`
	Tick                    int64 `json:"tick"`
	CapacityBytes           int64 `json:"capacity_bytes"`
	OccupiedBytes           int64 `json:"occupied_bytes"`
	HeldBytes               int64 `json:"held_bytes"`
	AvailableBytes          int64 `json:"available_bytes"`
	ProtectedGuaranteeBytes int64 `json:"protected_guarantee_bytes"`
}

type QueueItem struct {
	RequestID          string `json:"request_id"`
	TenantID           ID     `json:"tenant_id"`
	NodeID             ID     `json:"node_id"`
	AmountRemaining    int64  `json:"amount_remaining"`
	EnqueuedTick       int64  `json:"enqueued_tick"`
	BudgetMS           int64  `json:"budget_ms"`
	RetryAfterTick     int64  `json:"retry_after_tick"`
	ReasonCode         string `json:"reason_code"`
	ConstraintID       string `json:"constraint_id"`
	InvaderTenantID    ID     `json:"invader_tenant_id,omitempty"`
	ProtectedTenantIDs []ID   `json:"protected_tenant_ids,omitempty"`
}

type AdmissionOrderEntry struct {
	Order     int    `json:"order"`
	RequestID string `json:"request_id"`
	Tranche   string `json:"tranche"`
	TenantID  ID     `json:"tenant_id"`
	NodeID    ID     `json:"node_id"`
	Layer     int    `json:"layer"`
	Requested int64  `json:"requested_bytes"`
	Granted   int64  `json:"granted_bytes"`
}

type Allocation struct {
	RequestID          string `json:"request_id"`
	Tick               int64  `json:"tick"`
	TenantID           ID     `json:"tenant_id"`
	NodeID             ID     `json:"node_id"`
	RequestedBytes     int64  `json:"requested_bytes"`
	GrantedBytes       int64  `json:"granted_bytes"`
	QueuedBytes        int64  `json:"queued_bytes"`
	RejectedBytes      int64  `json:"rejected_bytes"`
	Decision           string `json:"decision"`
	Path               string `json:"path"`
	PreHoldBytes       int64  `json:"pre_hold_bytes"`
	DelayMS            int64  `json:"delay_ms"`
	BudgetMS           int64  `json:"budget_ms"`
	WithinBudget       bool   `json:"within_budget"`
	ReasonCode         string `json:"reason_code,omitempty"`
	ConstraintID       string `json:"constraint_id,omitempty"`
	InvaderTenantID    ID     `json:"invader_tenant_id,omitempty"`
	ProtectedTenantIDs []ID   `json:"protected_tenant_ids,omitempty"`
	RetryAfterTick     int64  `json:"retry_after_tick,omitempty"`
	WorkerSlot         int    `json:"worker_slot,omitempty"`
	FinalSettled       bool   `json:"final_settled"`
}

type TickReport struct {
	Tick           int64                 `json:"tick"`
	AdmissionOrder []AdmissionOrderEntry `json:"admission_order"`
	Allocations    []Allocation          `json:"allocations"`
	Ledgers        []TenantState         `json:"ledgers"`
	Nodes          []NodeState           `json:"nodes"`
	Queues         []QueueItem           `json:"queues"`
}

type RunReport struct {
	ProjectID  string       `json:"project_id"`
	Seed       int64        `json:"seed"`
	Path       string       `json:"path"`
	Budgets    Budgets      `json:"budgets"`
	Ticks      []TickReport `json:"ticks"`
	BusyErrors []*BizError  `json:"busy_errors,omitempty"`
}

func (r *RunReport) Tick(tick int64) *TickReport {
	for i := range r.Ticks {
		if r.Ticks[i].Tick == tick {
			return &r.Ticks[i]
		}
	}
	return nil
}

func (r *RunReport) Allocation(requestID string) (Allocation, bool) {
	for _, t := range r.Ticks {
		for _, a := range t.Allocations {
			if a.RequestID == requestID {
				return a, true
			}
		}
	}
	return Allocation{}, false
}

func SortedTenantIDs(p *Project) []ID {
	ids := make([]ID, 0, len(p.Tenants))
	for _, t := range p.Tenants {
		ids = append(ids, t.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func SortedNodeIDs(p *Project) []ID {
	ids := make([]ID, 0, len(p.Nodes))
	for _, n := range p.Nodes {
		ids = append(ids, n.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
