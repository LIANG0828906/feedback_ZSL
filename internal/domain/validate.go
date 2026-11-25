package domain

const (
	MinThreadBudget = 1
	MaxThreadBudget = 64
	MinQueueLimit   = 1
	MaxQueueLimit   = 4096
)

func MaintenanceNodeSet(p Project) map[string]bool {
	out := map[string]bool{}
	for _, b := range p.Batches {
		for _, id := range b.NodeIDs {
			out[id] = true
		}
	}
	return out
}

func ValidateConcurrency(threads, queue int) error {
	if threads < MinThreadBudget || threads > MaxThreadBudget {
		return NewError(EThreadBudgetInvalid, "$.policies.thread_budget",
			"thread budget %d outside [%d,%d]", threads, MinThreadBudget, MaxThreadBudget).
			WithDetailsSafe(map[string]any{"value": threads, "min": MinThreadBudget, "max": MaxThreadBudget})
	}
	if queue < MinQueueLimit || queue > MaxQueueLimit {
		return NewError(EQueueBudgetInvalid, "$.policies.per_node_queue_limit",
			"per-node queue limit %d outside [%d,%d]", queue, MinQueueLimit, MaxQueueLimit).
			WithDetailsSafe(map[string]any{"value": queue, "min": MinQueueLimit, "max": MaxQueueLimit})
	}
	return nil
}

func (e *Error) WithDetailsSafe(details map[string]any) *Error {
	return WithDetails(e, details)
}

func ValidateProject(p Project) error {
	if p.Window.End <= p.Window.Start {
		return NewError(EInvalidWindow, "$.window", "global window end must be after start")
	}

	nodeIDs := map[string]bool{}
	maintenance := MaintenanceNodeSet(p)
	for i, n := range p.Nodes {
		np := "$.nodes[" + itoa(i) + "]"
		if n.ID == "" {
			return NewError(EInvalidCapacity, JoinPath(np, "id"), "node id is required")
		}
		if nodeIDs[n.ID] {
			return NewError(EInvalidCapacity, JoinPath(np, "id"), "duplicate node id %q", n.ID)
		}
		nodeIDs[n.ID] = true
		if n.CapacityMilli <= 0 {
			return NewError(EInvalidCapacity, JoinPath(np, "capacity_milli"), "capacity must be positive")
		}
		if n.WatermarkBasisPoints < 0 {
			return NewError(ENegativeWatermark, JoinPath(np, "watermark_basis_points"),
				"watermark must not be negative: %d", n.WatermarkBasisPoints)
		}
		if n.WatermarkBasisPoints > 10000 {
			return NewError(EWatermarkOutOfRange, JoinPath(np, "watermark_basis_points"),
				"watermark basis points must be <= 10000")
		}
		if n.InitialUsedMilli < 0 || n.InitialUsedMilli > n.CapacityMilli {
			return NewError(EInvalidCapacity, JoinPath(np, "initial_used_milli"),
				"used capacity must be in [0,capacity]")
		}
		if n.Window.End != 0 || n.Window.Start != 0 {
			if n.Window.End <= n.Window.Start {
				return NewError(EInvalidWindow, JoinPath(np, "window"), "node window end must be after start")
			}
		}
	}

	instanceIDs := map[string]bool{}
	for i, in := range p.Instances {
		ip := "$.instances[" + itoa(i) + "]"
		if in.ID == "" || instanceIDs[in.ID] {
			return NewError(EInvalidCapacity, JoinPath(ip, "id"), "instance id is empty or duplicate")
		}
		instanceIDs[in.ID] = true
		n, ok := p.Node(in.NodeID)
		if !ok {
			return NewError(EInvalidCapacity, JoinPath(ip, "node_id"), "unknown node %q", in.NodeID)
		}
		if in.MilliCores <= 0 {
			return NewError(EInvalidCapacity, JoinPath(ip, "milli_cores"), "instance size must be positive")
		}
		if in.MigrationTicks <= 0 {
			return NewError(EInvalidCapacity, JoinPath(ip, "migration_ticks"), "migration ticks must be positive")
		}
		window := p.Window
		if n.Window.Start != 0 || n.Window.End != 0 {
			window = n.Window
		}
		if in.MigrationTicks > window.End-window.Start {
			return ConstraintError(EMigrationExceedsWindow, JoinPath(ip, "migration_ticks"),
				EConstraintWindow, "migration duration %d exceeds window length %d",
				in.MigrationTicks, window.End-window.Start).
				WithDetailsSafe(map[string]any{"instance_id": in.ID, "window_start": window.Start, "window_end": window.End})
		}
	}

	for i, loan := range p.Loans {
		lp := "$.loans[" + itoa(i) + "]"
		if loan.ExpiryTick < loan.StartTick {
			return NewError(ELoanStartAfterExpiry, JoinPath(lp, "expiry_tick"),
				"expiry %d is earlier than start %d", loan.ExpiryTick, loan.StartTick)
		}
		borrower, ok := p.Node(loan.BorrowerNodeID)
		if !ok {
			return NewError(ELoanTargetMaintenance, JoinPath(lp, "borrower_node_id"),
				"unknown borrower %q", loan.BorrowerNodeID)
		}
		if maintenance[loan.BorrowerNodeID] {
			return ConstraintError(ELoanTargetMaintenance, JoinPath(lp, "borrower_node_id"),
				EConstraintLoanExclusive, "loan borrower %q is marked for maintenance", loan.BorrowerNodeID)
		}
		if loan.AmountMilli <= 0 {
			return NewError(EInvalidCapacity, JoinPath(lp, "amount_milli"), "loan amount must be positive")
		}
		if borrower.BorrowCapMilli > 0 && loan.AmountMilli > borrower.BorrowCapMilli {
			return ConstraintError(ELoanBorrowLimitExceeded, JoinPath(lp, "amount_milli"),
				EConstraintLoanBorrowCap, "loan %d exceeds borrower cap %d",
				loan.AmountMilli, borrower.BorrowCapMilli)
		}
		if _, ok := p.Node(loan.LenderNodeID); !ok {
			return NewError(EInvalidCapacity, JoinPath(lp, "lender_node_id"), "unknown lender")
		}
	}

	if err := ValidateConcurrency(p.Policies.ThreadBudget, p.Policies.PerNodeQueueLimit); err != nil {
		return err
	}
	if p.Policies.MaxConcurrentMigrations <= 0 {
		return NewError(EInvalidCapacity, "$.policies.max_concurrent_migrations", "must be positive")
	}
	if p.Policies.MaxMigrationTicks <= 0 {
		return NewError(EInvalidCapacity, "$.policies.max_migration_ticks", "must be positive")
	}
	return nil
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
