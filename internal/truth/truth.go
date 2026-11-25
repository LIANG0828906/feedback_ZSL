package truth

import "maintenanceorchestrator/internal/domain"

type InstanceTruth struct {
	InstanceID   string `json:"instance_id"`
	Movable      bool   `json:"movable"`
	TargetNodeID string `json:"target_node_id,omitempty"`
	ReasonCode   string `json:"reason_code,omitempty"`
	ConstraintID string `json:"constraint_id,omitempty"`
}

type NodeFeasibilityTruth struct {
	NodeID         string          `json:"node_id"`
	EvacuableCount int             `json:"evacuable_count"`
	Instances      []InstanceTruth `json:"instances"`
}

type TickTruth struct {
	Tick               domain.Tick            `json:"tick"`
	MarkedNodes        []string               `json:"marked_nodes"`
	EventPendingNodes  []string               `json:"event_pending_nodes"`
	ActiveLeaseIDs     []string               `json:"active_lease_ids"`
	BorrowedByBorrower map[string]int64       `json:"borrowed_by_borrower"`
	Feasibility        []NodeFeasibilityTruth `json:"feasibility"`
}

type MigrationTruth struct {
	InstanceID string      `json:"instance_id"`
	Target     string      `json:"target_node_id"`
	Wave       int         `json:"wave"`
	Start      domain.Tick `json:"start_tick"`
	End        domain.Tick `json:"end_tick"`
}

type SpillTruth struct {
	InstanceID string `json:"instance_id"`
	FromWave   int    `json:"from_wave"`
	ToWave     int    `json:"to_wave"`
	ReasonCode string `json:"reason_code"`
}

type WaveTruth struct {
	Wave       int              `json:"wave"`
	Start      domain.Tick      `json:"start_tick"`
	End        domain.Tick      `json:"end_tick"`
	Migrations []MigrationTruth `json:"migrations"`
	Spillovers []SpillTruth     `json:"spillovers"`
}

type LeaseEventTruth struct {
	Tick       domain.Tick `json:"tick"`
	LeaseID    string      `json:"lease_id"`
	Kind       string      `json:"kind"`
	ReasonCode string      `json:"reason_code,omitempty"`
}

type DegradationTruth struct {
	Tick        domain.Tick `json:"tick"`
	NodeID      string      `json:"node_id"`
	InstanceIDs []string    `json:"instance_ids"`
}

type GateTruth struct {
	Tick                domain.Tick `json:"tick"`
	NodeID              string      `json:"node_id"`
	Schedulable         bool        `json:"schedulable"`
	OutstandingLeaseIDs []string    `json:"outstanding_lease_ids"`
	PendingReturnCount  int         `json:"pending_return_count"`
	NotEvacuatedCount   int         `json:"not_evacuated_count"`
}

type Truth struct {
	SchemaVersion string             `json:"schema_version"`
	Seed          int64              `json:"seed"`
	ProjectID     string             `json:"project_id"`
	ConstraintIDs map[string]string  `json:"constraint_ids"`
	Ticks         []TickTruth        `json:"ticks"`
	Waves         []WaveTruth        `json:"waves"`
	LeaseEvents   []LeaseEventTruth  `json:"lease_events"`
	Degradations  []DegradationTruth `json:"degradations"`
	Gates         []GateTruth        `json:"gates"`
}

func Construction(p domain.Project) Truth {
	marked := []string{"m-block", "m-fast", "m-slot", "m-wave2"}
	feasibility := baselineFeasibility()

	var ticks []TickTruth
	for _, t := range p.ProbeTicks {
		activeLeaseIDs, borrowed := leaseSnapshot(p, t)
		ticks = append(ticks, TickTruth{
			Tick: t, MarkedNodes: append([]string(nil), marked...),
			EventPendingNodes:  eventPending(p, t),
			ActiveLeaseIDs:     activeLeaseIDs,
			BorrowedByBorrower: borrowed,
			Feasibility:        feasibility,
		})
	}

	return Truth{
		SchemaVersion: "construction-truth/v1",
		Seed:          p.Seed,
		ProjectID:     p.ProjectID,
		ConstraintIDs: map[string]string{
			"capacity":  domain.EConstraintCapacity,
			"watermark": domain.EConstraintWatermark,
			"wave":      domain.EConstraintWaveConcurrency,
			"duration":  domain.EConstraintMoveDuration,
			"window":    domain.EConstraintWindow,
			"lease":     domain.EConstraintLoanExclusive,
			"borrow":    domain.EConstraintLoanBorrowCap,
			"gate":      domain.EConstraintReturnGate,
		},
		Ticks: ticks,
		Waves: []WaveTruth{
			{Wave: 1, Start: 10, End: 11, Migrations: []MigrationTruth{
				{InstanceID: "i-fast-1", Target: "t-1", Wave: 1, Start: 10, End: 11},
				{InstanceID: "i-slot-1", Target: "t-2", Wave: 1, Start: 10, End: 11},
			}, Spillovers: []SpillTruth{
				{InstanceID: "i-fast-2", FromWave: 1, ToWave: 2, ReasonCode: "R_WAVE_CONCURRENCY_FULL"},
			}},
			{Wave: 2, Start: 11, End: 12, Migrations: []MigrationTruth{
				{InstanceID: "i-wave2-1", Target: "t-4", Wave: 2, Start: 11, End: 12},
				{InstanceID: "i-fast-2", Target: "t-3", Wave: 2, Start: 11, End: 12},
			}},
			{Wave: 3, Start: 12, End: 13, Migrations: []MigrationTruth{
				{InstanceID: "i-block-1", Target: "t-3", Wave: 3, Start: 12, End: 13},
				{InstanceID: "i-block-2", Target: "t-over", Wave: 3, Start: 12, End: 13},
			}, Spillovers: []SpillTruth{
				{InstanceID: "i-block-3", FromWave: 3, ToWave: 4, ReasonCode: "R_WAVE_CONCURRENCY_FULL"},
				{InstanceID: "i-block-4", FromWave: 3, ToWave: 4, ReasonCode: "R_WAVE_CONCURRENCY_FULL"},
				{InstanceID: "i-block-5", FromWave: 3, ToWave: 4, ReasonCode: "R_WAVE_CONCURRENCY_FULL"},
			}},
			{Wave: 4, Start: 13, End: 14, Migrations: []MigrationTruth{
				{InstanceID: "i-block-3", Target: "t-over", Wave: 4, Start: 13, End: 14},
			}},
		},
		LeaseEvents: []LeaseEventTruth{
			{Tick: 13, LeaseID: "loan-wave2-over", Kind: "recalled", ReasonCode: "R_BORROWER_LIMIT_RECALL"},
			{Tick: 17, LeaseID: "loan-fast-t3", Kind: "expired"},
			{Tick: 19, LeaseID: "loan-slot-over", Kind: "expired"},
		},
		Degradations: []DegradationTruth{
			{Tick: 17, NodeID: "t-3", InstanceIDs: []string{"w-t3-degrade"}},
		},
		Gates: []GateTruth{
			{Tick: 22, NodeID: "m-fast", Schedulable: true, OutstandingLeaseIDs: []string{}, PendingReturnCount: 0},
			{Tick: 22, NodeID: "m-slot", Schedulable: true, OutstandingLeaseIDs: []string{}, PendingReturnCount: 0},
			{Tick: 22, NodeID: "m-wave2", Schedulable: true, OutstandingLeaseIDs: []string{}, PendingReturnCount: 0},
			{Tick: 22, NodeID: "m-block", Schedulable: false, OutstandingLeaseIDs: []string{}, PendingReturnCount: 0, NotEvacuatedCount: 2},
		},
	}
}

func baselineFeasibility() []NodeFeasibilityTruth {
	wm := "C-WATERMARK-NONDISRUPTION"
	return []NodeFeasibilityTruth{
		{NodeID: "m-block", EvacuableCount: 4, Instances: []InstanceTruth{
			{InstanceID: "i-block-1", Movable: true, TargetNodeID: "t-1"},
			{InstanceID: "i-block-2", Movable: true, TargetNodeID: "t-2"},
			{InstanceID: "i-block-3", Movable: true, TargetNodeID: "t-4"},
			{InstanceID: "i-block-4", Movable: true, TargetNodeID: "t-over"},
			{InstanceID: "i-block-5", Movable: false, ReasonCode: "R_TARGET_WATERMARK_FULL", ConstraintID: wm},
		}},
		{NodeID: "m-fast", EvacuableCount: 2, Instances: []InstanceTruth{
			{InstanceID: "i-fast-1", Movable: true, TargetNodeID: "t-1"},
			{InstanceID: "i-fast-2", Movable: true, TargetNodeID: "t-2"},
		}},
		{NodeID: "m-slot", EvacuableCount: 1, Instances: []InstanceTruth{
			{InstanceID: "i-slot-1", Movable: true, TargetNodeID: "t-1"},
		}},
		{NodeID: "m-wave2", EvacuableCount: 1, Instances: []InstanceTruth{
			{InstanceID: "i-wave2-1", Movable: true, TargetNodeID: "t-1"},
		}},
	}
}

func eventPending(p domain.Project, t domain.Tick) []string {
	active := map[string]bool{}
	for _, e := range p.Events {
		switch e.Kind {
		case domain.EventMarkPending:
			if e.Tick <= t {
				active[e.NodeID] = true
			}
		case domain.EventReturned:
			if e.Tick <= t {
				delete(active, e.NodeID)
			}
		}
	}
	out := make([]string, 0, len(active))
	for id := range active {
		out = append(out, id)
	}
	return sortedStrings(out)
}

func leaseSnapshot(p domain.Project, t domain.Tick) ([]string, map[string]int64) {
	recalledAt := map[string]domain.Tick{"loan-wave2-over": 13}
	borrowed := map[string]int64{}
	var ids []string
	for _, l := range p.Loans {
		if !(l.StartTick <= t && t < l.ExpiryTick) {
			continue
		}
		if at, ok := recalledAt[l.ID]; ok && at <= t {
			continue
		}
		ids = append(ids, l.ID)
		borrowed[l.BorrowerNodeID] += l.AmountMilli
	}
	sortedStrings(ids)
	return ids, borrowed
}

func sortedStrings(in []string) []string {
	for i := 0; i < len(in); i++ {
		for j := i + 1; j < len(in); j++ {
			if in[j] < in[i] {
				in[i], in[j] = in[j], in[i]
			}
		}
	}
	return in
}
