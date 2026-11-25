package engine

import (
	"sort"
	"strconv"

	"tlscheck/internal/bizerr"
	"tlscheck/internal/model"
)

type admissionPlan struct {
	results    []ChainResult
	selected   *ChainResult
	fallback   *FallbackBasis
	requiredBP int64
	violations []Violation
	primary    string
}

func processEvents(
	project model.Project,
	ep model.Endpoint,
	events []model.Event,
	st *EndpointState,
	reqByID map[string]*model.Request,
	byID map[string]*RequestResult,
	results *[]*RequestResult,
	t int64,
	opts Options,
) error {
	for _, ev := range events {
		if ev.AtMs != t || st.ProcessedEvents[ev.Sequence] {
			continue
		}
		if ev.Update != nil {
			upd := ev.Update
			st.Frozen = true
			st.FrozenReason = "pending chain update " + upd.ID
			cp := *upd
			st.PendingUpdates[upd.ID] = &cp
			if err := commitDueUpdates(project, ep, st, t, opts); err != nil {
				return err
			}
			st.ProcessedEvents[ev.Sequence] = true
			continue
		}
		if ev.Request == nil {
			st.ProcessedEvents[ev.Sequence] = true
			continue
		}
		req := ev.Request
		res := byID[req.ID]
		if res == nil {
			res = newRequestResult(ev)
			byID[req.ID] = res
			*results = append(*results, res)
		}
		if active := st.Active[req.ID]; active != nil {
			if !active.ValidationComplete {
				if err := continueActive(project, ep, st, active, res, opts); err != nil {
					return err
				}
			}
			st.ProcessedEvents[ev.Sequence] = true
			continue
		}
		resetResultForReevaluation(res)
		if code := protocolFailure(*req, ep); code != "" {
			res.Status = StatusCompleted
			res.CompletedAtMs = t
			res.HandshakeConclusion = code
			res.GateCategory = GateOK
			res.Provenance.Sources = []string{"protocol_policy"}
			st.ProcessedEvents[ev.Sequence] = true
			continue
		}
		plan := buildAdmissionPlan(project, ep, st, *req, t)
		if plan.primary == GateChainInvalid {
			res.Status = StatusRejected
			res.CompletedAtMs = t
			res.GateCategory = GateChainInvalid
			res.HandshakeConclusion = GateChainInvalid
			res.ChainResults = plan.results
			res.CheckedChainIDs = chainIDs(plan.results)
			res.Violations = plan.violations
			res.ViolatedConstraints = constraintIDs(plan.violations)
			res.Fallback = plan.fallback
			res.Provenance.Sources = []string{"chain_validation_at_arrival"}
			st.ProcessedEvents[ev.Sequence] = true
			continue
		}
		if len(plan.violations) > 0 {
			if len(st.Queue) >= ep.BufferCapacity {
				res.Status = StatusRejected
				res.CompletedAtMs = t
				res.GateCategory = GateBufferFull
				res.BackpressureCode = bizerr.EBufferFull
				res.Fallback = plan.fallback
				res.Violations = plan.violations
				res.ViolatedConstraints = append(constraintIDs(plan.violations), ViolationBuffer)
				res.Provenance.Sources = []string{"endpoint_buffer_capacity"}
				st.ProcessedEvents[ev.Sequence] = true
				continue
			}
			item := &QueueItem{
				Status: StatusQueued, EndpointID: ep.ID, EndpointQueueSeq: st.NextQueueSequence,
				RequestID: req.ID, EventSequence: ev.Sequence, EnqueuedAtMs: t,
				LastAttemptAtMs: t, PrimaryGate: plan.primary,
				ViolatedConstraints: constraintIDs(plan.violations), Violations: plan.violations,
			}
			st.NextQueueSequence++
			st.Queue = append(st.Queue, item)
			res.Status = StatusQueued
			res.GateCategory = plan.primary
			res.Fallback = plan.fallback
			res.EndpointQueueSeq = item.EndpointQueueSeq
			res.Violations = plan.violations
			res.ViolatedConstraints = item.ViolatedConstraints
			res.Provenance.Sources = []string{"admission_gate"}
			st.ProcessedEvents[ev.Sequence] = true
			continue
		}
		if err := activate(project, ep, st, *req, ev.Sequence, t, plan, nil, res, opts); err != nil {
			return err
		}
		st.ProcessedEvents[ev.Sequence] = true
	}
	return nil
}

func processQueue(
	project model.Project,
	ep model.Endpoint,
	st *EndpointState,
	reqByID map[string]*model.Request,
	byID map[string]*RequestResult,
	t int64,
	opts Options,
) error {
	for len(st.Queue) > 0 {
		item := st.Queue[0]
		req := reqByID[item.RequestID]
		res := byID[item.RequestID]
		if req == nil || res == nil {
			return bizerr.Newf(bizerr.ENotFound, "queued request %q/result is missing after recovery", item.RequestID)
		}
		if code := protocolFailure(*req, ep); code != "" {
			res.Status = StatusCompleted
			res.CompletedAtMs = t
			res.HandshakeConclusion = code
			item.Status = StatusRejected
			st.Queue = st.Queue[1:]
			st.History = append(st.History, item)
			continue
		}
		plan := buildAdmissionPlan(project, ep, st, *req, t)
		if plan.primary == GateChainInvalid {
			res.Status = StatusRejected
			res.CompletedAtMs = t
			res.GateCategory = GateChainInvalid
			res.HandshakeConclusion = GateChainInvalid
			res.ChainResults = plan.results
			res.CheckedChainIDs = chainIDs(plan.results)
			res.Fallback = plan.fallback
			res.Violations = plan.violations
			res.ViolatedConstraints = constraintIDs(plan.violations)
			item.Status = StatusRejected
			item.PrimaryGate = GateChainInvalid
			item.Violations = plan.violations
			item.ViolatedConstraints = constraintIDs(plan.violations)
			st.Queue = st.Queue[1:]
			st.History = append(st.History, item)
			continue
		}
		if len(plan.violations) > 0 {
			item.LastAttemptAtMs = t
			item.PrimaryGate = plan.primary
			item.Violations = plan.violations
			item.ViolatedConstraints = constraintIDs(plan.violations)
			res.GateCategory = plan.primary
			res.Fallback = plan.fallback
			res.Violations = plan.violations
			res.ViolatedConstraints = item.ViolatedConstraints
			return nil
		}
		st.Queue = st.Queue[1:]
		item.Status = StatusAdmitted
		item.HasAdmission = true
		item.AdmittedAtMs = t
		st.History = append(st.History, item)
		if err := activate(project, ep, st, *req, item.EventSequence, t, plan, item, res, opts); err != nil {
			return err
		}
	}
	return nil
}

func resumeIncomplete(
	project model.Project,
	ep model.Endpoint,
	st *EndpointState,
	byID map[string]*RequestResult,
	t int64,
	opts Options,
) error {
	ids := make([]string, 0, len(st.Active))
	for id, active := range st.Active {
		if !active.ValidationComplete {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		active := st.Active[id]
		res := byID[id]
		if res == nil {
			res = &RequestResult{RequestID: id, EndpointID: ep.ID}
			byID[id] = res
		}
		if err := continueActive(project, ep, st, active, res, opts); err != nil {
			return err
		}
	}
	return nil
}

func continueActive(project model.Project, ep model.Endpoint, st *EndpointState, active *ActiveHandshake, res *RequestResult, opts Options) error {
	ids := active.CheckedChainIDs
	if len(ids) == 0 {
		ids = []string{active.SelectedChainID}
	}
	plan := admissionPlan{}
	for _, cid := range ids {
		ch, ok := project.Chain(cid)
		if !ok {
			return bizerr.Newf(bizerr.ENotFound, "persisted handshake references unknown chain %q", cid)
		}
		cr := ValidateChain(ch, active.StartedAtMs, pinFor(ep, ch))
		plan.results = append(plan.results, cr)
		if cid == active.SelectedChainID {
			crCopy := cr
			plan.selected = &crCopy
		}
	}
	if active.Fallback != nil {
		plan.fallback = active.Fallback
	}
	if err := commitPlanSteps(st, active, plan, res, opts); err != nil {
		return err
	}
	active.ValidationComplete = true
	applyAdmissionResult(res, ep, active, plan, nil)
	return nil
}

func buildAdmissionPlan(project model.Project, ep model.Endpoint, st *EndpointState, req model.Request, t int64) admissionPlan {
	plan := admissionPlan{}
	if st.Frozen {
		plan.primary = GateBudgetFrozen
		plan.violations = append(plan.violations, Violation{
			ConstraintID: ViolationFreeze, Code: GateBudgetFrozen,
			Message: "endpoint budget is frozen until chain update commit",
		})
		return plan
	}
	current, ok := project.Chain(st.CurrentChainID)
	if !ok {
		plan.primary = GateChainInvalid
		plan.violations = append(plan.violations, Violation{ConstraintID: ViolationChain, Code: "CHAIN_NOT_FOUND", Message: st.CurrentChainID})
		return plan
	}
	currentResult := ValidateChain(current, t, pinFor(ep, current))
	plan.results = append(plan.results, currentResult)
	if currentResult.Valid {
		plan.selected = &plan.results[0]
		st.LastValidChainID = current.ID
	} else if st.LastValidChainID != "" && st.LastValidChainID != current.ID {
		fbChain, ok := project.Chain(st.LastValidChainID)
		if ok {
			fbResult := ValidateChain(fbChain, t, pinFor(ep, fbChain))
			plan.results = append(plan.results, fbResult)
			idx := len(plan.results) - 1
			basis := &FallbackBasis{
				AttemptedChainID: current.ID, FallbackChainID: fbChain.ID,
				LastCommittedUpdateID: st.LastCommittedUpdateID,
				CommittedAtMs:         st.LastCommitAtMs, RevalidatedAtMs: t,
				RevalidationPassed: fbResult.Valid,
			}
			if fbResult.Valid {
				plan.selected = &plan.results[idx]
				basis.Reason = "current chain failed at request time; using last committed valid chain"
			} else {
				basis.Reason = "last committed valid chain no longer passes revalidation"
			}
			plan.fallback = basis
		}
	}
	if plan.selected == nil {
		plan.primary = GateChainInvalid
		plan.violations = append(plan.violations, Violation{
			ConstraintID: ViolationChain, Code: GateChainInvalid,
			Message: "current chain is invalid and no last committed valid fallback passes revalidation",
		})
		return plan
	}
	for _, cr := range plan.results {
		plan.requiredBP += cr.ChargedCostBP
	}
	available := ep.BudgetBP - st.UsedBudgetBP - st.ReservedBudgetBP
	if plan.requiredBP > ep.BudgetBP {
		plan.primary = GateBudgetExhausted
		plan.violations = append(plan.violations, Violation{
			ConstraintID: ViolationBudget, Code: "BUDGET_REQUEST_EXCEEDS_CAPACITY",
			Message: "single validation cost exceeds endpoint budget capacity",
		})
	} else if plan.requiredBP > available {
		plan.primary = GateBudgetExhausted
		plan.violations = append(plan.violations, Violation{
			ConstraintID: ViolationBudget, Code: GateBudgetExhausted,
			Message: "committed or reserved chain budget is insufficient",
		})
	}
	if st.ActiveCount >= ep.MaxParallel {
		if plan.primary == "" {
			plan.primary = GateParallelExhausted
		}
		plan.violations = append(plan.violations, Violation{
			ConstraintID: ViolationParallel, Code: GateParallelExhausted,
			Message: "endpoint reached max parallel handshake limit",
		})
	}
	return plan
}

func activate(
	project model.Project,
	ep model.Endpoint,
	st *EndpointState,
	req model.Request,
	eventSeq int,
	t int64,
	plan admissionPlan,
	item *QueueItem,
	res *RequestResult,
	opts Options,
) error {
	if plan.selected == nil {
		return bizerr.New(bizerr.EInvalidArgument, "activate called without selected chain")
	}
	st.ReservedBudgetBP += plan.requiredBP
	timeoutAt := t + ep.TimeoutMs
	finishAt := t + req.DurationMs
	timedOut := req.DurationMs > ep.TimeoutMs
	if timedOut {
		finishAt = timeoutAt
	}
	active := &ActiveHandshake{
		RequestID: req.ID, StartedAtMs: t, FinishAtMs: finishAt, TimeoutAtMs: timeoutAt,
		TimedOut: timedOut, ChainID: plan.selected.ChainID, SelectedChainID: plan.selected.ChainID,
		ReservedBP: plan.requiredBP, Request: &req, EventSequence: eventSeq, ArrivalAtMs: t,
		Fallback: plan.fallback, CheckedChainIDs: chainIDs(plan.results),
	}
	st.Active[req.ID] = active
	st.ActiveCount++
	if err := commitPlanSteps(st, active, plan, res, opts); err != nil {
		applyAdmissionResult(res, ep, active, plan, item)
		return err
	}
	active.ValidationComplete = true
	applyAdmissionResult(res, ep, active, plan, item)
	return nil
}

func commitPlanSteps(st *EndpointState, active *ActiveHandshake, plan admissionPlan, res *RequestResult, opts Options) error {
	globalIndex := 0
	reusedCount := active.NextLevel
	for ci := range plan.results {
		cr := &plan.results[ci]
		for li := range cr.Levels {
			level := &cr.Levels[li]
			if globalIndex < reusedCount {
				level.Reused = true
				globalIndex++
				continue
			}
			cost := level.CostBP
			if cost > 0 {
				st.ReservedBudgetBP -= cost
				if st.ReservedBudgetBP < 0 {
					st.ReservedBudgetBP = 0
				}
				st.UsedBudgetBP += cost
				active.ReservedBP -= cost
				if active.ReservedBP < 0 {
					active.ReservedBP = 0
				}
				active.ChargedBP += cost
				st.Charges = append(st.Charges, BudgetCharge{
					TimeMs: active.StartedAtMs, RequestID: active.RequestID,
					ChainID: cr.ChainID, Level: level.Index, AmountBP: cost,
				})
			}
			active.Levels = append(active.Levels, *level)
			active.NextLevel = globalIndex + 1
			if opts.Faults.StepJournal != nil {
				if err := opts.Faults.StepJournal.SaveStep(active.RequestID, cr.ChainID, *level); err != nil {
					return err
				}
			}
			idx := globalIndex
			globalIndex++
			if shouldInterrupt(active.Request, opts, idx) {
				res.Status = StatusInterrupted
				res.GateCategory = "INTERRUPTED_BEFORE_COMMIT"
				res.ChainResults = partialPlanResults(plan.results, active.NextLevel)
				res.BudgetChargedBP = active.ChargedBP
				res.Provenance.Recomputed = false
				res.Provenance.Sources = []string{"step_commit:" + itoa(active.NextLevel)}
				return bizerr.Newf(bizerr.EInterrupt, "request %q interrupted after %d committed validation step(s)", active.RequestID, active.NextLevel).
					WithContext("request_id", active.RequestID).
					WithContext("next_level", active.NextLevel).
					WithContext("rolled_back_to", "last_committed_step")
			}
		}
	}
	active.NextLevel = globalIndex
	return nil
}

func shouldInterrupt(req *model.Request, opts Options, globalIndex int) bool {
	if req == nil || opts.Faults.InterruptRequestIDs == nil || !opts.Faults.InterruptRequestIDs[req.ID] {
		return false
	}
	n := req.InterruptAfterLevel
	if n <= 0 {
		n = 1
	}
	return globalIndex+1 == n
}

func applyAdmissionResult(res *RequestResult, ep model.Endpoint, active *ActiveHandshake, plan admissionPlan, item *QueueItem) {
	res.EndpointID = ep.ID
	res.Admitted = true
	res.AdmissionAtMs = active.StartedAtMs
	res.StartedAtMs = active.StartedAtMs
	res.HasFinalAdmission = true
	res.FinalAdmissionAtMs = active.StartedAtMs
	res.Status = StatusAdmitted
	res.GateCategory = GateOK
	res.SelectedChainID = active.SelectedChainID
	res.CheckedChainIDs = append([]string(nil), active.CheckedChainIDs...)
	res.Fallback = active.Fallback
	res.BudgetChargedBP = active.ChargedBP
	if len(plan.results) > 0 {
		res.ChainResults = plan.results
	}
	if item != nil {
		res.QueueNo = 0
		res.EndpointQueueSeq = item.EndpointQueueSeq
	}
	if active.ValidationComplete {
		res.Provenance.Sources = append(res.Provenance.Sources, "admission_committed")
	} else {
		res.Provenance.Sources = append(res.Provenance.Sources, "resume_pending")
	}
}

func newRequestResult(ev model.Event) *RequestResult {
	req := ev.Request
	return &RequestResult{
		RequestID: req.ID, EndpointID: req.EndpointID, EventSequence: ev.Sequence,
		ArrivalAtMs: ev.AtMs, FinalAdmissionAtMs: 0, HasFinalAdmission: false,
		Provenance: Provenance{Sources: []string{"deterministic_simulation"}},
	}
}

func resetResultForReevaluation(r *RequestResult) {
	id, ep, seq, at := r.RequestID, r.EndpointID, r.EventSequence, r.ArrivalAtMs
	prov := r.Provenance
	*r = RequestResult{
		RequestID: id, EndpointID: ep, EventSequence: seq, ArrivalAtMs: at,
		FinalAdmissionAtMs: 0, Provenance: prov,
	}
}

func chainIDs(results []ChainResult) []string {
	out := make([]string, 0, len(results))
	for _, cr := range results {
		out = append(out, cr.ChainID)
	}
	return out
}

func constraintIDs(vs []Violation) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.ConstraintID)
	}
	return out
}

func partialPlanResults(in []ChainResult, steps int) []ChainResult {
	out := make([]ChainResult, 0, len(in))
	remaining := steps
	for _, cr := range in {
		cp := cr
		cp.Levels = nil
		for _, lvl := range cr.Levels {
			if remaining <= 0 {
				break
			}
			cp.Levels = append(cp.Levels, lvl)
			remaining--
		}
		out = append(out, cp)
	}
	return out
}

func finalizeQueues(report *Report) {
	var all []*QueueItem
	type meta struct{ ep string }
	_ = meta{}
	for epID, st := range report.EndpointStates {
		for _, q := range st.History {
			all = append(all, q)
		}
		for _, q := range st.Queue {
			all = append(all, q)
		}
		_ = epID
	}
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.EnqueuedAtMs != b.EnqueuedAtMs {
			return a.EnqueuedAtMs < b.EnqueuedAtMs
		}
		if a.EventSequence != b.EventSequence {
			return a.EventSequence < b.EventSequence
		}
		return a.EndpointQueueSeq < b.EndpointQueueSeq
	})
	resultByRequest := map[string]*RequestResult{}
	for _, r := range report.Results {
		resultByRequest[r.RequestID] = r
	}
	for i, item := range all {
		record := QueueRecord{
			QueueNo: i + 1, EndpointQueueSeq: item.EndpointQueueSeq,
			RequestID: item.RequestID, EndpointID: item.EndpointID,
			EventSequence: item.EventSequence, EnqueuedAtMs: item.EnqueuedAtMs,
			Status: item.Status, PrimaryGate: item.PrimaryGate,
			ViolatedConstraints: item.ViolatedConstraints,
		}
		if item.HasAdmission {
			record.HasFinalAdmission = true
			record.FinalAdmissionAtMs = item.AdmittedAtMs
		}
		report.QueueRecords = append(report.QueueRecords, &record)
		if res := resultByRequest[item.RequestID]; res != nil {
			res.QueueNo = record.QueueNo
			if item.HasAdmission {
				res.HasFinalAdmission = true
				res.FinalAdmissionAtMs = item.AdmittedAtMs
			}
		}
	}
}

func itoa(i int) string {
	return strconv.Itoa(i)
}
