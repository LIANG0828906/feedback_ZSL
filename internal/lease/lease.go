package lease

import (
	"sort"

	"maintenanceorchestrator/internal/domain"
	"maintenanceorchestrator/internal/solver"
)

const (
	StatusScheduled = "scheduled"
	StatusActive    = "active"
	StatusExpired   = "expired"
	StatusRecalled  = "recalled"

	RecallBorrowerLimit = "R_BORROWER_LIMIT_RECALL"
	RecallDoubleBooked  = "R_DOUBLE_BOOKING_RECALL"
)

type LeaseState struct {
	ID             string      `json:"id"`
	LotID          string      `json:"lot_id"`
	LenderNodeID   string      `json:"lender_node_id"`
	BorrowerNodeID string      `json:"borrower_node_id"`
	AmountMilli    int64       `json:"amount_milli"`
	StartTick      domain.Tick `json:"start_tick"`
	ExpiryTick     domain.Tick `json:"expiry_tick"`
	Priority       int         `json:"priority"`
	DeclaredOrder  int         `json:"declared_order"`
	Status         string      `json:"status"`
	ReturnedTick   domain.Tick `json:"returned_tick,omitempty"`
	RecallReason   string      `json:"recall_reason,omitempty"`
	ConstraintID   string      `json:"constraint_id,omitempty"`
}

type LeaseEvent struct {
	Tick         domain.Tick `json:"tick"`
	LeaseID      string      `json:"lease_id"`
	LenderNodeID string      `json:"lender_node_id"`
	BorrowerID   string      `json:"borrower_node_id"`
	AmountMilli  int64       `json:"amount_milli"`
	ReasonCode   string      `json:"reason_code"`
	ConstraintID string      `json:"constraint_id,omitempty"`
	Message      string      `json:"message,omitempty"`
}

type DegradationEvent struct {
	Tick         domain.Tick `json:"tick"`
	NodeID       string      `json:"node_id"`
	InstanceIDs  []string    `json:"instance_ids"`
	RemovedMilli int64       `json:"removed_milli"`
	ReasonCode   string      `json:"reason_code"`
	ConstraintID string      `json:"constraint_id"`
}

type TickReport struct {
	Tick               domain.Tick        `json:"tick"`
	ActiveLeases       []LeaseState       `json:"active_leases"`
	LentByLender       map[string]int64   `json:"lent_by_lender"`
	BorrowedByBorrower map[string]int64   `json:"borrowed_by_borrower"`
	ExpiryEvents       []LeaseEvent       `json:"expiry_events"`
	RecallEvents       []LeaseEvent       `json:"recall_events"`
	Degradations       []DegradationEvent `json:"degradations"`
}

type Timeline struct {
	Ticks             []TickReport       `json:"ticks"`
	Leases            []LeaseState       `json:"leases"`
	Expiries          []LeaseEvent       `json:"expiries"`
	Recalls           []LeaseEvent       `json:"recalls"`
	Degradations      []DegradationEvent `json:"degradations"`
	DegradedInstances map[string]bool    `json:"degraded_instances"`
	ReturnedLeaseIDs  map[string]bool    `json:"returned_lease_ids"`
}

type Options struct {
	Project         domain.Project
	Placement       map[string]string
	Plan            *solver.WavePlan
	Returns         []ReturnCompletion
	FreedCapacity   map[string]int64
	Ticks           []domain.Tick
	InitialDegraded map[string]bool
}

type ReturnCompletion struct {
	Tick       domain.Tick `json:"tick"`
	InstanceID string      `json:"instance_id"`
	HomeNodeID string      `json:"home_node_id"`
	FromNodeID string      `json:"from_node_id"`
	MilliCores int64       `json:"milli_cores"`
}

func Settle(opts Options) (*Timeline, error) {
	p := opts.Project
	tl := &Timeline{DegradedInstances: map[string]bool{}, ReturnedLeaseIDs: map[string]bool{}}
	if opts.InitialDegraded != nil {
		for k, v := range opts.InitialDegraded {
			tl.DegradedInstances[k] = v
		}
	}
	states := map[string]*LeaseState{}
	for _, spec := range p.Loans {
		st := &LeaseState{
			ID: spec.ID, LotID: spec.LotID, LenderNodeID: spec.LenderNodeID,
			BorrowerNodeID: spec.BorrowerNodeID, AmountMilli: spec.AmountMilli,
			StartTick: spec.StartTick, ExpiryTick: spec.ExpiryTick,
			Priority: spec.Priority, DeclaredOrder: spec.DeclaredOrder,
			Status: StatusScheduled,
		}
		states[spec.ID] = st
		tl.Leases = append(tl.Leases, *st)
	}

	usage, currentNode := initialOccupancy(p, opts)
	appliedMigrations := map[string]bool{}
	appliedReturns := map[string]bool{}
	ticks := append([]domain.Tick(nil), opts.Ticks...)
	if len(ticks) == 0 {
		for t := p.Window.Start; t <= p.Window.End; t++ {
			ticks = append(ticks, t)
		}
	}

	for _, tick := range ticks {
		report := TickReport{Tick: tick, LentByLender: map[string]int64{}, BorrowedByBorrower: map[string]int64{}}
		reserved := advanceOccupancy(p, opts, tick, usage, currentNode, appliedMigrations, appliedReturns)

		for _, id := range sortedStateIDs(states) {
			st := states[id]
			if st.Status == StatusActive && st.ExpiryTick == tick {
				st.Status = StatusExpired
				st.ReturnedTick = tick
				tl.ReturnedLeaseIDs[id] = true
				ev := LeaseEvent{Tick: tick, LeaseID: st.ID, LenderNodeID: st.LenderNodeID,
					BorrowerID: st.BorrowerNodeID, AmountMilli: st.AmountMilli,
					ReasonCode: StatusExpired, Message: "lease reached declared expiry"}
				report.ExpiryEvents = append(report.ExpiryEvents, ev)
				tl.Expiries = append(tl.Expiries, ev)
			}
		}

		for _, spec := range sortLoanSpecs(p.Loans) {
			st := states[spec.ID]
			if st.Status == StatusScheduled && st.StartTick == tick {
				st.Status = StatusActive
			}
		}

		if err := enforceCoverage(p, states, opts.FreedCapacity, tick, &report, tl); err != nil {
			return nil, err
		}
		recallBorrowerLimits(p, states, tick, &report, tl)
		degradeAfterReclaim(p, states, usage, reserved, currentNode, tl, tick, &report)

		for _, id := range sortedStateIDs(states) {
			st := states[id]
			if st.Status == StatusActive {
				cp := *st
				report.ActiveLeases = append(report.ActiveLeases, cp)
				report.LentByLender[st.LenderNodeID] += st.AmountMilli
				report.BorrowedByBorrower[st.BorrowerNodeID] += st.AmountMilli
			}
		}
		tl.Ticks = append(tl.Ticks, report)
	}

	tl.Leases = tl.Leases[:0]
	for _, id := range sortedStateIDs(states) {
		tl.Leases = append(tl.Leases, *states[id])
	}
	return tl, nil
}

func enforceCoverage(p domain.Project, states map[string]*LeaseState, freed map[string]int64, tick domain.Tick, report *TickReport, tl *Timeline) error {
	lent := map[string]int64{}
	for _, id := range sortedStateIDs(states) {
		st := states[id]
		if st.Status != StatusActive {
			continue
		}
		lent[st.LenderNodeID] += st.AmountMilli
	}
	for lender, amount := range lent {
		capacity := freed[lender]
		if capacity == 0 {
			capacity = freed["*"]
		}
		if amount > capacity {
			err := domain.ConstraintError(domain.ELoanDoubleBooked, "$.loans",
				domain.EConstraintLoanExclusive,
				"active leases from lender %q total %d > freed capacity %d at tick %d",
				lender, amount, capacity, tick).
				WithDetailsSafe(map[string]any{"lender": lender, "tick": int64(tick), "active_milli": amount, "freed_milli": capacity})
			return err
		}
	}
	return nil
}

func initialOccupancy(p domain.Project, opts Options) (map[string]int64, map[string]string) {
	current := map[string]string{}
	for _, in := range p.Instances {
		current[in.ID] = in.NodeID
	}
	if opts.Plan != nil {
		return solver.UsageFromPlacements(p, nil), current
	}
	usage := solver.UsageFromPlacements(p, opts.Placement)
	for id, nodeID := range opts.Placement {
		current[id] = nodeID
	}
	return usage, current
}

func advanceOccupancy(p domain.Project, opts Options, tick domain.Tick, usage map[string]int64, currentNode map[string]string, appliedMigrations, appliedReturns map[string]bool) map[string]int64 {
	reserved := map[string]int64{}
	if opts.Plan != nil {
		for _, m := range opts.Plan.Migrations {
			size := migrationSize(p, m.InstanceID)
			if !appliedMigrations[m.InstanceID] && m.EndTick <= tick {
				usage[m.SourceNodeID] -= size
				usage[m.TargetNodeID] += size
				currentNode[m.InstanceID] = m.TargetNodeID
				appliedMigrations[m.InstanceID] = true
			}
			if m.StartTick <= tick && tick < m.EndTick && !appliedMigrations[m.InstanceID] {
				reserved[m.TargetNodeID] += size
			}
		}
	}
	for _, r := range opts.Returns {
		if r.Tick > tick || appliedReturns[r.InstanceID] {
			continue
		}
		from := r.FromNodeID
		if from == "" {
			from = currentNode[r.InstanceID]
		}
		size := r.MilliCores
		if size == 0 {
			size = migrationSize(p, r.InstanceID)
		}
		if from != "" {
			usage[from] -= size
		}
		usage[r.HomeNodeID] += size
		currentNode[r.InstanceID] = r.HomeNodeID
		appliedReturns[r.InstanceID] = true
	}
	for id, nodeID := range currentNode {
		_ = id
		_ = nodeID
	}
	return reserved
}

func migrationSize(p domain.Project, id string) int64 {
	for _, in := range p.Instances {
		if in.ID == id {
			return in.MilliCores
		}
	}
	return 0
}

func recallBorrowerLimits(p domain.Project, states map[string]*LeaseState, tick domain.Tick, report *TickReport, tl *Timeline) {
	activeByBorrower := map[string][]*LeaseState{}
	for _, id := range sortedStateIDs(states) {
		st := states[id]
		if st.Status == StatusActive {
			activeByBorrower[st.BorrowerNodeID] = append(activeByBorrower[st.BorrowerNodeID], st)
		}
	}
	for _, borrowerID := range sortedBorrowerIDs(activeByBorrower) {
		node, ok := p.Node(borrowerID)
		if !ok || node.BorrowCapMilli <= 0 {
			continue
		}
		active := activeByBorrower[borrowerID]
		total := int64(0)
		for _, st := range active {
			total += st.AmountMilli
		}
		sortRecallCandidates(active)
		for total > node.BorrowCapMilli && len(active) > 0 {
			victim := active[0]
			active = active[1:]
			total -= victim.AmountMilli
			victim.Status = StatusRecalled
			victim.ReturnedTick = tick
			victim.RecallReason = RecallBorrowerLimit
			victim.ConstraintID = domain.EConstraintLoanBorrowCap
			tl.ReturnedLeaseIDs[victim.ID] = true
			ev := LeaseEvent{Tick: tick, LeaseID: victim.ID, LenderNodeID: victim.LenderNodeID,
				BorrowerID: victim.BorrowerNodeID, AmountMilli: victim.AmountMilli,
				ReasonCode: RecallBorrowerLimit, ConstraintID: domain.EConstraintLoanBorrowCap,
				Message: "borrower active lease total exceeded declared borrow cap"}
			report.RecallEvents = append(report.RecallEvents, ev)
			tl.Recalls = append(tl.Recalls, ev)
		}
	}
}

func degradeAfterReclaim(p domain.Project, states map[string]*LeaseState, usage, reserved map[string]int64, currentNode map[string]string, tl *Timeline, tick domain.Tick, report *TickReport) {
	loanByBorrower := map[string]int64{}
	for _, st := range states {
		if st.Status == StatusActive {
			loanByBorrower[st.BorrowerNodeID] += st.AmountMilli
		}
	}
	for _, n := range p.Nodes {
		effectiveUsed := usage[n.ID] + reserved[n.ID]
		var candidates []domain.Instance
		for _, in := range p.Instances {
			if currentNode[in.ID] != n.ID {
				continue
			}
			if tl.DegradedInstances[in.ID] {
				effectiveUsed -= degradationAmount(in)
				continue
			}
			candidates = append(candidates, in)
		}
		ceiling := n.SafeLimitMilli() + loanByBorrower[n.ID]
		if effectiveUsed <= ceiling {
			continue
		}
		sortDegradationCandidates(candidates)
		ev := DegradationEvent{Tick: tick, NodeID: n.ID, ReasonCode: "R_WATERMARK_DEGRADE", ConstraintID: domain.EConstraintWatermark}
		for _, in := range candidates {
			if effectiveUsed <= ceiling {
				break
			}
			amount := degradationAmount(in)
			tl.DegradedInstances[in.ID] = true
			effectiveUsed -= amount
			ev.InstanceIDs = append(ev.InstanceIDs, in.ID)
			ev.RemovedMilli += amount
		}
		if len(ev.InstanceIDs) > 0 {
			report.Degradations = append(report.Degradations, ev)
			tl.Degradations = append(tl.Degradations, ev)
		}
	}
}

func degradationAmount(in domain.Instance) int64 {
	if in.DegradeMilliCores > 0 {
		return in.DegradeMilliCores
	}
	return in.MilliCores
}

func sortLoanSpecs(in []domain.LoanSpec) []domain.LoanSpec {
	out := append([]domain.LoanSpec(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].StartTick != out[j].StartTick {
			return out[i].StartTick < out[j].StartTick
		}
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		if out[i].DeclaredOrder != out[j].DeclaredOrder {
			return out[i].DeclaredOrder < out[j].DeclaredOrder
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func sortedStateIDs(states map[string]*LeaseState) []string {
	out := make([]string, 0, len(states))
	for id := range states {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func sortedBorrowerIDs(in map[string][]*LeaseState) []string {
	out := make([]string, 0, len(in))
	for id := range in {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func sortRecallCandidates(in []*LeaseState) {
	sort.SliceStable(in, func(i, j int) bool {
		a, b := in[i], in[j]
		// Larger priority number means lower service priority and is reclaimed first.
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		if a.DeclaredOrder != b.DeclaredOrder {
			return a.DeclaredOrder > b.DeclaredOrder
		}
		return a.ID > b.ID
	})
}

func sortDegradationCandidates(in []domain.Instance) {
	sort.SliceStable(in, func(i, j int) bool {
		a, b := in[i], in[j]
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		if a.DeclaredOrder != b.DeclaredOrder {
			return a.DeclaredOrder > b.DeclaredOrder
		}
		return a.ID < b.ID
	})
}

func ActiveAt(tl *Timeline, tick domain.Tick) []LeaseState {
	for _, r := range tl.Ticks {
		if r.Tick == tick {
			return r.ActiveLeases
		}
	}
	return nil
}
