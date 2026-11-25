package hckernel

import (
	"fmt"
	"sort"
)

func (e *Engine) collectCandidates(nowMs int64) ([]candidate, []Suppression) {
	queued := map[string]bool{}
	for _, entry := range e.queue {
		queued[entry.InstanceID] = true
	}

	var candidates []candidate
	var suppressed []Suppression
	for _, id := range e.sortedInstanceIDs() {
		rt := e.states[id]
		inst := rt.Instance
		if queued[id] {
			continue
		}
		if rt.Outstanding != nil {
			suppressed = append(suppressed, Suppression{
				InstanceID: id,
				Reason:     "probe_in_flight_timeout",
				Detail:     fmt.Sprintf("deadline_ms=%d", rt.Outstanding.DeadlineMs),
			})
			continue
		}
		if !inst.ActiveProbe {
			suppressed = append(suppressed, Suppression{InstanceID: id, Reason: "active_probe_disabled"})
			continue
		}
		if nowMs < rt.BackoffUntil {
			suppressed = append(suppressed, Suppression{
				InstanceID: id,
				Reason:     "backoff_active",
				Detail:     fmt.Sprintf("until_ms=%d", rt.BackoffUntil),
			})
			continue
		}
		halfOpenDue := (rt.Health == StateUnhealthy || rt.Health == StateHalfOpen) && nowMs >= rt.HalfOpenAt
		if rt.Health == StateUnhealthy && !halfOpenDue {
			suppressed = append(suppressed, Suppression{
				InstanceID: id,
				Reason:     "half_open_wait",
				Detail:     fmt.Sprintf("until_ms=%d", rt.HalfOpenAt),
			})
			continue
		}
		if !halfOpenDue && nowMs < rt.NextPeriodAt {
			suppressed = append(suppressed, Suppression{
				InstanceID: id,
				Reason:     "interval_not_due",
				Detail:     fmt.Sprintf("next_ms=%d", rt.NextPeriodAt),
			})
			continue
		}
		candidates = append(candidates, candidate{
			instanceID: id,
			rackID:     inst.RackID,
			weightPPM:  inst.WeightPPM,
			halfOpen:   halfOpenDue,
		})
	}
	return candidates, suppressed
}

func (e *Engine) allocate(fresh []candidate, nowMs int64, round int, report *RoundReport) {
	freshByID := map[string]candidate{}
	freshIDs := make([]string, 0, len(fresh))
	for _, cand := range fresh {
		freshByID[cand.instanceID] = cand
		freshIDs = append(freshIDs, cand.instanceID)
	}
	rankedFreshIDs := RankByWeight(freshIDs, func(id string) int64 { return freshByID[id].weightPPM })

	deferredCandidates := make([]candidate, 0, len(e.queue))
	for _, entry := range e.queue {
		rt := e.states[entry.InstanceID]
		if rt == nil {
			continue
		}
		entryCopy := entry
		deferredCandidates = append(deferredCandidates, candidate{
			instanceID: entry.InstanceID,
			rackID:     rt.Instance.RackID,
			weightPPM:  rt.Instance.WeightPPM,
			halfOpen:   rt.Health == StateUnhealthy || rt.Health == StateHalfOpen,
			deferred:   &entryCopy,
		})
	}

	var ordered []candidate
	switch e.project.SlotPriority {
	case PriorityPeriodicFirst:
		for _, id := range rankedFreshIDs {
			ordered = append(ordered, freshByID[id])
		}
		ordered = append(ordered, deferredCandidates...)
	default:
		ordered = append(ordered, deferredCandidates...)
		for _, id := range rankedFreshIDs {
			ordered = append(ordered, freshByID[id])
		}
	}

	acceptedSeq := map[int64]bool{}
	dispatchOrder := 0
	for _, cand := range ordered {
		rt := e.states[cand.instanceID]
		if rt == nil {
			continue
		}
		if e.project.GlobalConcurrency == 0 {
			s := Suppression{
				InstanceID:   cand.instanceID,
				Code:         ErrBudgetZero,
				Reason:       "budget_zero_rejected",
				ConstraintID: ConstraintGlobalBudgetZero,
			}
			report.Rejected = append(report.Rejected, s)
			report.Suppressed = append(report.Suppressed, s)
			continue
		}
		rack, ok := e.project.rackMap()[cand.rackID]
		if !ok {
			s := Suppression{
				InstanceID:   cand.instanceID,
				Code:         ErrRackReferenceMissing,
				Reason:       "rack_reference_missing",
				ConstraintID: RackConcurrencyConstraint(cand.rackID),
			}
			report.Rejected = append(report.Rejected, s)
			report.Suppressed = append(report.Suppressed, s)
			continue
		}
		if rack.Concurrency == 0 {
			constraint := RackBudgetZeroConstraint(cand.rackID)
			s := Suppression{
				InstanceID:   cand.instanceID,
				Code:         ErrBudgetZero,
				Reason:       "rack_budget_zero_rejected",
				ConstraintID: constraint,
			}
			report.Rejected = append(report.Rejected, s)
			report.Suppressed = append(report.Suppressed, s)
			continue
		}
		if e.globalActive >= e.project.GlobalConcurrency {
			e.blockOrQueue(cand, round, ConstraintGlobalConcurrency, "global_concurrency_exceeded", report, acceptedSeq)
			continue
		}
		if e.rackActive[cand.rackID] >= rack.Concurrency {
			e.blockOrQueue(cand, round, RackConcurrencyConstraint(cand.rackID), "rack_concurrency_exceeded", report, acceptedSeq)
			continue
		}

		dispatchOrder++
		probe := Probe{
			ID:         fmt.Sprintf("P-R%03d-%s", round, cand.instanceID),
			Round:      round,
			Order:      dispatchOrder,
			InstanceID: cand.instanceID,
			RackID:     cand.rackID,
			HalfOpen:   cand.halfOpen,
			WeightPPM:  cand.weightPPM,
		}
		if cand.halfOpen {
			e.enterHalfOpen(rt, probe, nowMs)
		}
		rt.Outstanding = &activeProbe{Probe: probe, DeadlineMs: nowMs + rt.Instance.TimeoutMs}
		rt.NextPeriodAt = nowMs + rt.Instance.IntervalMs
		e.globalActive++
		e.rackActive[cand.rackID]++
		report.Dispatched = append(report.Dispatched, probe)

		if cand.deferred != nil {
			acceptedSeq[cand.deferred.QueueSeq] = true
			report.Rescheduled = append(report.Rescheduled, RescheduledProbe{
				InstanceID:    cand.instanceID,
				QueueSeq:      cand.deferred.QueueSeq,
				Round:         round,
				OriginalRound: cand.deferred.EnqueuedRound,
			})
		}
	}

	if len(acceptedSeq) > 0 {
		remaining := e.queue[:0]
		for _, entry := range e.queue {
			if !acceptedSeq[entry.QueueSeq] {
				remaining = append(remaining, entry)
			}
		}
		e.queue = remaining
	}
}

func (e *Engine) blockOrQueue(cand candidate, round int, constraint, reason string, report *RoundReport, _ map[int64]bool) {
	if cand.deferred != nil {
		seq := cand.deferred.QueueSeq
		for i := range e.queue {
			if e.queue[i].QueueSeq == seq {
				e.queue[i].LastRound = round
				e.queue[i].ConstraintID = constraint
				e.queue[i].Reason = reason
			}
		}
		report.Suppressed = append(report.Suppressed, Suppression{
			InstanceID:   cand.instanceID,
			Reason:       "deferred_waiting",
			ConstraintID: constraint,
			QueueSeq:     seq,
			Detail:       reason,
		})
		return
	}

	e.queueSeq++
	entry := DeferredEntry{
		InstanceID:    cand.instanceID,
		QueueSeq:      e.queueSeq,
		EnqueuedRound: round,
		EnqueuedAtMs:  e.nowMs,
		LastRound:     round,
		RackID:        cand.rackID,
		WeightPPM:     cand.weightPPM,
		ConstraintID:  constraint,
		Reason:        reason,
	}
	e.queue = append(e.queue, entry)
	report.DeferredNow = append(report.DeferredNow, entry)
	report.Suppressed = append(report.Suppressed, Suppression{
		InstanceID:   cand.instanceID,
		Reason:       "budget_deferred",
		ConstraintID: constraint,
		QueueSeq:     entry.QueueSeq,
		Detail:       reason,
	})
}

func (e *Engine) expireDeferred(nowMs int64, round int, report *RoundReport) {
	if e.project.MaxDeferMs <= 0 {
		return
	}
	remaining := e.queue[:0]
	for _, entry := range e.queue {
		if nowMs-entry.EnqueuedAtMs > e.project.MaxDeferMs {
			drop := DroppedProbe{
				InstanceID: entry.InstanceID,
				QueueSeq:   entry.QueueSeq,
				Round:      round,
				AtMs:       nowMs,
				Code:       ErrDeferTimeout,
				Reason:     "max_defer_exceeded",
			}
			report.Dropped = append(report.Dropped, drop)
			continue
		}
		remaining = append(remaining, entry)
	}
	e.queue = remaining
}

func (e *Engine) processTimeouts(nowMs int64) {
	for _, id := range e.sortedInstanceIDs() {
		rt := e.states[id]
		if rt.Outstanding == nil || nowMs < rt.Outstanding.DeadlineMs {
			continue
		}
		probe := rt.Outstanding.Probe
		e.releaseOutstanding(rt)
		rt.LastEventID = "TIMEOUT:" + probe.ID
		e.applyOutcome(rt, ResultTimeout, rt.LastEventID, nowMs, probe.ID)
	}
}

func (e *Engine) releaseOutstanding(rt *InstanceState) {
	if rt.Outstanding == nil {
		return
	}
	rack := rt.Outstanding.RackID
	e.globalActive--
	if e.rackActive[rack] > 0 {
		e.rackActive[rack]--
	}
	rt.Outstanding = nil
}

func (e *Engine) applyResultEvent(ev Event) error {
	rt, ok := e.states[ev.InstanceID]
	if !ok {
		return newError(ErrInstanceNotFound, "result for missing instance", withInstance(ev.InstanceID), withEvent(ev.ID))
	}
	if rt.Outstanding == nil {
		return newError(ErrProbeNotFound, "result injected without outstanding probe", withInstance(ev.InstanceID), withEvent(ev.ID))
	}
	if ev.ProbeID != "" && ev.ProbeID != rt.Outstanding.ID {
		return newError(ErrProbeNotFound, "result probe id does not match outstanding probe", withInstance(ev.InstanceID), withEvent(ev.ID), withPath(ev.ProbeID))
	}
	probeID := rt.Outstanding.ID
	e.releaseOutstanding(rt)
	rt.LastEventID = ev.ID
	e.applyOutcome(rt, ev.Result, ev.ID, ev.AtMs, probeID)
	return nil
}

// sortedReports is a helper currently used by tests and deterministic output.
func sortedReports(reports []RoundReport) {
	sort.SliceStable(reports, func(i, j int) bool { return reports[i].Round < reports[j].Round })
}
