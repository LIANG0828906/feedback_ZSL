// Package gate implements the bounded-latency admission front door.
package gate

import (
	"fmt"
	"time"

	"quotakernel/model"
)

const (
	ConclusionAllow  = "ALLOW"
	ConclusionQueue  = "QUEUE"
	ConclusionReject = "REJECT"
)

// Input is the immutable fast-path view for one decision.
type Input struct {
	Tick                   model.Tick
	EventID                string
	TenantID               string
	NodeID                 string
	RequestedBytes         int64
	BudgetNs               int64
	NodeCapacityBytes      int64
	NodeUsedBytes          int64
	NodePreDeductedBytes   int64
	TenantAllowedRemaining int64
	TenantAbsoluteMaxBytes int64
	Now                    func() time.Time
}

type Decision struct {
	Record            model.GateRecord
	ObservedLatencyNs int64
}

type FastGate struct {
	Budget time.Duration
	Now    func() time.Time
}

func New(budget time.Duration) *FastGate {
	if budget <= 0 {
		budget = time.Duration(model.DefaultRules().GateBudgetNs)
	}
	return &FastGate{Budget: budget, Now: time.Now}
}

// Decide performs only O(1) arithmetic. It never borrows quota assigned to
// another tenant: TenantAllowedRemaining is owned by the requesting tenant.
func (g *FastGate) Decide(in Input) Decision {
	now := g.Now
	if now == nil {
		now = time.Now
	}
	budget := g.Budget
	if in.BudgetNs > 0 {
		budget = time.Duration(in.BudgetNs)
	}
	wallStart := time.Now()
	start := now()

	record := model.GateRecord{
		DecisionID: fmt.Sprintf("gate-%s", in.EventID),
		EventID:    in.EventID,
		Tick:       in.Tick,
		TenantID:   in.TenantID,
		NodeID:     in.NodeID,
		BudgetNs:   budget.Nanoseconds(),
	}

	nodeFastAvailable := in.NodeCapacityBytes - in.NodeUsedBytes - in.NodePreDeductedBytes
	if nodeFastAvailable < 0 {
		nodeFastAvailable = 0
	}
	record.NodeReserveBytes = in.RequestedBytes
	record.TenantSafeRemaining = in.TenantAllowedRemaining

	switch {
	case in.RequestedBytes <= 0:
		record.Conclusion = ConclusionReject
		record.FastConclusion = ConclusionReject
		record.ReasonCode = "REJECT_EMPTY_REQUEST"
		record.ConstraintID = model.ConstraintQuotaHard
	case in.TenantAbsoluteMaxBytes > 0 && in.RequestedBytes > in.TenantAbsoluteMaxBytes:
		record.Conclusion = ConclusionReject
		record.FastConclusion = ConclusionReject
		record.ReasonCode = model.CodeRejectedQuota
		record.ConstraintID = model.ConstraintQuotaHard
	case in.RequestedBytes > in.NodeCapacityBytes:
		record.Conclusion = ConclusionReject
		record.FastConclusion = ConclusionReject
		record.ReasonCode = model.CodeRejectedNode
		record.ConstraintID = model.ConstraintNodeCapacity
	case in.RequestedBytes > in.TenantAllowedRemaining:
		record.Conclusion = ConclusionQueue
		record.FastConclusion = ConclusionQueue
		record.ReasonCode = "FAST_TENANT_QUOTA_WAIT"
		record.ConstraintID = model.ConstraintQuotaHard
	case in.RequestedBytes > nodeFastAvailable:
		record.Conclusion = ConclusionQueue
		record.FastConclusion = ConclusionQueue
		record.ReasonCode = "FAST_NODE_RESERVE_WAIT"
		record.ConstraintID = model.ConstraintNodeCapacity
	default:
		record.Conclusion = ConclusionAllow
		record.FastConclusion = ConclusionAllow
		record.ReasonCode = model.CodeAdmitted
		record.FastPath = true
	}

	virtualElapsed := now().Sub(start)
	observed := time.Since(wallStart).Nanoseconds()
	if observed < virtualElapsed.Nanoseconds() {
		observed = virtualElapsed.Nanoseconds()
	}
	if virtualElapsed > budget || time.Duration(observed) > budget {
		record.Conclusion = ConclusionQueue
		record.FastConclusion = ConclusionQueue
		record.FastPath = false
		record.ReasonCode = model.CodeGateLatencyDegradedQueue
		record.ConstraintID = model.ConstraintLatencyBudget
		record.NodeReserveBytes = 0
	}
	return Decision{Record: record, ObservedLatencyNs: observed}
}

// WithinBudget is a small helper used by diagnostics and tests.
func WithinBudget(observedNs, budgetNs int64) bool {
	return observedNs <= budgetNs
}
