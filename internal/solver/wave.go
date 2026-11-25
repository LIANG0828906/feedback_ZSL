package solver

import (
	"sort"

	"maintenanceorchestrator/internal/domain"
)

type Migration struct {
	InstanceID    string      `json:"instance_id"`
	SourceNodeID  string      `json:"source_node_id"`
	TargetNodeID  string      `json:"target_node_id"`
	Wave          int         `json:"wave"`
	StartTick     domain.Tick `json:"start_tick"`
	EndTick       domain.Tick `json:"end_tick"`
	DurationTicks domain.Tick `json:"duration_ticks"`
	Priority      int         `json:"priority"`
	DeclaredOrder int         `json:"declared_order"`
}

type Spillover struct {
	InstanceID   string `json:"instance_id"`
	SourceNodeID string `json:"source_node_id"`
	FromWave     int    `json:"from_wave"`
	ToWave       int    `json:"to_wave"`
	ReasonCode   string `json:"reason_code"`
	ConstraintID string `json:"constraint_id"`
	Message      string `json:"message"`
}

type BlockedMigration struct {
	InstanceID   string `json:"instance_id"`
	SourceNodeID string `json:"source_node_id"`
	Wave         int    `json:"wave"`
	ReasonCode   string `json:"reason_code"`
	ConstraintID string `json:"constraint_id"`
	Message      string `json:"message"`
}

type WaveResult struct {
	Wave         int         `json:"wave"`
	StartTick    domain.Tick `json:"start_tick"`
	EndTick      domain.Tick `json:"end_tick"`
	MigrationIDs []string    `json:"migration_ids"`
	SpilloverIDs []string    `json:"spillover_ids"`
}

type WavePlan struct {
	StartTick  domain.Tick        `json:"start_tick"`
	EndTick    domain.Tick        `json:"end_tick"`
	Waves      []WaveResult       `json:"waves"`
	Migrations []Migration        `json:"migrations"`
	Spillovers []Spillover        `json:"spillovers"`
	Blocked    []BlockedMigration `json:"blocked"`
	Placement  map[string]string  `json:"placement"`
}

type candidate struct {
	inst domain.Instance
	wave int
}

type ResumeState struct {
	CommittedWave int
	Tick          domain.Tick
	Placement     map[string]string
	Migrations    []Migration
}

func PlanWaves(p domain.Project) WavePlan {
	return PlanWavesFrom(p, ResumeState{})
}

func PlanWavesFrom(p domain.Project, resume ResumeState) WavePlan {
	placement := map[string]string{}
	for k, v := range resume.Placement {
		placement[k] = v
	}
	plan := WavePlan{StartTick: p.Window.Start, Placement: placement}
	plan.Migrations = append(plan.Migrations, resume.Migrations...)
	nodesByWave := map[int][]string{}
	maxDeclaredWave := 0
	for _, w := range p.Waves {
		if w.Seq > maxDeclaredWave {
			maxDeclaredWave = w.Seq
		}
		for _, bid := range w.BatchIDs {
			b, ok := p.Batch(bid)
			if !ok {
				continue
			}
			nodesByWave[w.Seq] = append(nodesByWave[w.Seq], b.NodeIDs...)
		}
	}

	currentStart := resume.Tick
	if currentStart == 0 {
		currentStart = p.Window.Start
	}
	waveSeq := 1
	var deferred []candidate
	if resume.CommittedWave > 0 {
		waveSeq = resume.CommittedWave + 1
		committedIDs := map[string]bool{}
		for _, m := range resume.Migrations {
			committedIDs[m.InstanceID] = true
		}
		for seq := 1; seq <= resume.CommittedWave; seq++ {
			for _, nodeID := range nodesByWave[seq] {
				for _, in := range p.InstancesForNode(nodeID) {
					if !committedIDs[in.ID] {
						deferred = append(deferred, candidate{inst: in, wave: seq + 1})
					}
				}
			}
		}
	}
	hardStopWave := maxDeclaredWave + len(p.Instances) + 3

	for waveSeq <= hardStopWave {
		candidates := append([]candidate(nil), deferred...)
		for _, nodeID := range nodesByWave[waveSeq] {
			for _, in := range p.InstancesForNode(nodeID) {
				candidates = append(candidates, candidate{inst: in, wave: waveSeq})
			}
		}
		if len(candidates) == 0 {
			break
		}
		sortCandidates(candidates)

		result := WaveResult{Wave: waveSeq, StartTick: currentStart}
		reserved := map[string]int64{}
		used := UsageFromPlacements(p, placement)
		pending := domain.MaintenanceNodeSet(p)
		selected := 0
		var nextDeferred []candidate
		waveEnd := currentStart

		for _, c := range candidates {
			in := c.inst
			if selected >= p.Policies.MaxConcurrentMigrations {
				sp := Spillover{
					InstanceID: in.ID, SourceNodeID: in.NodeID, FromWave: waveSeq,
					ToWave: waveSeq + 1, ReasonCode: ReasonWaveConcurrency,
					ConstraintID: domain.EConstraintWaveConcurrency,
					Message:      "wave migration concurrency slots exhausted",
				}
				plan.Spillovers = append(plan.Spillovers, sp)
				result.SpilloverIDs = append(result.SpilloverIDs, in.ID)
				nextDeferred = append(nextDeferred, candidate{inst: in, wave: waveSeq + 1})
				continue
			}
			if in.MigrationTicks > p.Policies.MaxMigrationTicks {
				plan.Blocked = append(plan.Blocked, BlockedMigration{
					InstanceID: in.ID, SourceNodeID: in.NodeID, Wave: waveSeq,
					ReasonCode: ReasonMoveDuration, ConstraintID: domain.EConstraintMoveDuration,
					Message: "instance migration duration exceeds policy maximum",
				})
				continue
			}
			endTick := currentStart + in.MigrationTicks
			node, _ := p.Node(in.NodeID)
			window := p.Window
			if node.Window.End > 0 {
				window = node.Window
			}
			if endTick > window.End {
				plan.Blocked = append(plan.Blocked, BlockedMigration{
					InstanceID: in.ID, SourceNodeID: in.NodeID, Wave: waveSeq,
					ReasonCode: domain.EMigrationExceedsWindow, ConstraintID: domain.EConstraintWindow,
					Message: "scheduled migration completion is after maintenance window end",
				})
				continue
			}

			fit, ok := selectWaveTarget(p, in.MilliCores, currentStart, used, reserved, pending)
			if !ok {
				reason := fit.ReasonCode
				constraint := fit.ConstraintID
				if reason == "" {
					reason = ReasonTargetWatermark
					constraint = domain.EConstraintWatermark
				}
				sp := Spillover{
					InstanceID: in.ID, SourceNodeID: in.NodeID, FromWave: waveSeq,
					ToWave: waveSeq + 1, ReasonCode: reason, ConstraintID: constraint,
					Message: fit.Message,
				}
				plan.Spillovers = append(plan.Spillovers, sp)
				result.SpilloverIDs = append(result.SpilloverIDs, in.ID)
				nextDeferred = append(nextDeferred, candidate{inst: in, wave: waveSeq + 1})
				continue
			}

			reserved[fit.NodeID] += in.MilliCores
			selected++
			if endTick > waveEnd {
				waveEnd = endTick
			}
			m := Migration{
				InstanceID: in.ID, SourceNodeID: in.NodeID, TargetNodeID: fit.NodeID,
				Wave: waveSeq, StartTick: currentStart, EndTick: endTick,
				DurationTicks: in.MigrationTicks, Priority: in.Priority, DeclaredOrder: in.DeclaredOrder,
			}
			plan.Migrations = append(plan.Migrations, m)
			result.MigrationIDs = append(result.MigrationIDs, in.ID)
			placement[in.ID] = fit.NodeID
		}

		if selected == 0 && len(nextDeferred) > 0 {
			for _, c := range nextDeferred {
				plan.Blocked = append(plan.Blocked, BlockedMigration{
					InstanceID: c.inst.ID, SourceNodeID: c.inst.NodeID, Wave: waveSeq,
					ReasonCode: "R_WAVE_PROGRESS_EXHAUSTED", ConstraintID: domain.EConstraintWaveConcurrency,
					Message: "candidate could not be selected and no later declared wave remains",
				})
			}
			deferred = nil
		} else {
			deferred = nextDeferred
		}
		if waveEnd == currentStart {
			waveEnd = currentStart + 1
		}
		result.EndTick = waveEnd
		plan.Waves = append(plan.Waves, result)
		plan.EndTick = waveEnd
		currentStart = waveEnd
		waveSeq++
		if len(deferred) == 0 && waveSeq > maxDeclaredWave {
			break
		}
	}
	sort.SliceStable(plan.Migrations, func(i, j int) bool {
		if plan.Migrations[i].Wave != plan.Migrations[j].Wave {
			return plan.Migrations[i].Wave < plan.Migrations[j].Wave
		}
		if plan.Migrations[i].Priority != plan.Migrations[j].Priority {
			return plan.Migrations[i].Priority < plan.Migrations[j].Priority
		}
		if plan.Migrations[i].DeclaredOrder != plan.Migrations[j].DeclaredOrder {
			return plan.Migrations[i].DeclaredOrder < plan.Migrations[j].DeclaredOrder
		}
		return plan.Migrations[i].InstanceID < plan.Migrations[j].InstanceID
	})
	return plan
}

func sortCandidates(candidates []candidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i].inst, candidates[j].inst
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if a.DeclaredOrder != b.DeclaredOrder {
			return a.DeclaredOrder < b.DeclaredOrder
		}
		return a.ID < b.ID
	})
}

func selectWaveTarget(p domain.Project, request int64, tick domain.Tick, used, reserved map[string]int64, pending map[string]bool) (TargetFit, bool) {
	fits := targetFitsWithLoans(p, request, tick, used, reserved, nil, pending)
	var candidates []TargetFit
	for _, f := range fits {
		if f.Fits {
			candidates = append(candidates, f)
		}
	}
	if len(candidates) == 0 {
		if len(fits) > 0 {
			return fits[0], false
		}
		return TargetFit{ReasonCode: ReasonNoOnlineTarget, ConstraintID: domain.EConstraintCapacity}, false
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].WatermarkHeadroomMilli != candidates[j].WatermarkHeadroomMilli {
			return candidates[i].WatermarkHeadroomMilli < candidates[j].WatermarkHeadroomMilli
		}
		return candidates[i].NodeID < candidates[j].NodeID
	})
	return candidates[0], true
}

func targetFitsWithLoans(p domain.Project, request int64, tick domain.Tick, used, reserved, consumed map[string]int64, pending map[string]bool) []TargetFit {
	var out []TargetFit
	for _, n := range p.Nodes {
		if pending[n.ID] || n.State == domain.StateMaintenance {
			continue
		}
		loan := ActiveLoanMilli(p, n.ID, tick)
		effectiveUsed := used[n.ID] + reserved[n.ID] + consumed[n.ID]
		hardCeiling := n.CapacityMilli + loan
		watermarkCeiling := n.SafeLimitMilli() + loan
		hardFree := hardCeiling - effectiveUsed
		wmHeadroom := watermarkCeiling - effectiveUsed
		fit := TargetFit{NodeID: n.ID, HardFreeMilli: hardFree, WatermarkHeadroomMilli: wmHeadroom}
		switch {
		case hardFree < request:
			fit.ReasonCode = ReasonTargetCapacity
			fit.ConstraintID = domain.EConstraintCapacity
			fit.Message = "target hard capacity and active loan cannot hold instance"
		case wmHeadroom < request:
			fit.ReasonCode = ReasonTargetWatermark
			fit.ConstraintID = domain.EConstraintWatermark
			fit.Message = "target nondisruption watermark and active loan are full"
		default:
			fit.Fits = true
		}
		out = append(out, fit)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

func ActiveLoanMilli(p domain.Project, borrowerID string, tick domain.Tick) int64 {
	type candidate struct {
		amount   int64
		priority int
		order    int
		id       string
	}
	var active []candidate
	cap := int64(0)
	if n, ok := p.Node(borrowerID); ok {
		cap = n.BorrowCapMilli
	}
	for _, l := range p.Loans {
		if l.BorrowerNodeID == borrowerID && l.StartTick <= tick && tick < l.ExpiryTick {
			active = append(active, candidate{amount: l.AmountMilli, priority: l.Priority, order: l.DeclaredOrder, id: l.ID})
		}
	}
	sort.SliceStable(active, func(i, j int) bool {
		if active[i].priority != active[j].priority {
			return active[i].priority < active[j].priority
		}
		if active[i].order != active[j].order {
			return active[i].order < active[j].order
		}
		return active[i].id < active[j].id
	})
	var total int64
	for _, l := range active {
		// The lease settler recalls lower-priority excess when the active
		// total crosses the borrower cap. Wave planning uses that same
		// post-recall effective capacity.
		if cap > 0 && total+l.amount > cap {
			continue
		}
		total += l.amount
	}
	return total
}
