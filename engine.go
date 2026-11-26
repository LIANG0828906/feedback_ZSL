package leasekit

import (
	"fmt"
	"sort"
)

type RunOptions struct {
	// AttemptBudgetPerResource, when positive, overrides resource-specific
	// renewal attempt budgets. Zero means use the declared resource budget.
	AttemptBudgetPerResource int
	StatePatches             []StatePatch
	ResultOverrides          map[int]ResultOverride
}

type engine struct {
	p       *Project
	opts    RunOptions
	states  map[string]*HolderState
	results map[int]*OpResult
	events  []TimedEvent
	arb     map[string]*Arbitration
	budget  map[int]bool
}

func Run(p *Project) (*Report, error) {
	return RunWithOptions(p, RunOptions{})
}

func RunWithOptions(p *Project, opts RunOptions) (*Report, error) {
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	if opts.AttemptBudgetPerResource < 0 || opts.AttemptBudgetPerResource > 1_000_000 {
		return nil, validationError(CodeAttemptBudgetOOR, "options.attemptBudget", "must be in [0,1000000], got %d", opts.AttemptBudgetPerResource)
	}
	e := &engine{
		p:       p,
		opts:    opts,
		states:  map[string]*HolderState{},
		results: map[int]*OpResult{},
		arb:     map[string]*Arbitration{},
		budget:  precomputeBudgetExhausted(p, opts.AttemptBudgetPerResource),
	}
	for _, h := range p.Holders {
		e.states[h.ID] = &HolderState{HolderID: h.ID, ResourceID: h.ResourceID}
	}

	for _, op := range p.SortedOperations() {
		if _, done := e.results[op.Seq]; done {
			continue
		}
		e.applyPatches(op.Seq)
		if op.Kind == OpRenew && op.Cohort != "" {
			if _, ok := e.arb[op.Cohort]; !ok {
				e.arbitrate(op.Cohort)
			}
		}
		e.handle(op)
	}

	return e.report(), nil
}

func (e *engine) applyPatches(afterSeq int) {
	for _, patch := range e.opts.StatePatches {
		if patch.BeforeSeq != afterSeq {
			continue
		}
		st, ok := e.states[patch.HolderID]
		if !ok {
			continue
		}
		if patch.Active != nil {
			st.Active = *patch.Active
		}
		if patch.ExpiresAtMS != nil {
			st.ExpiresAtMS = *patch.ExpiresAtMS
		}
		if patch.RenewCount != nil {
			st.RenewCount = *patch.RenewCount
		}
	}
}

func precomputeBudgetExhausted(p *Project, optionBudget int) map[int]bool {
	byResource := map[string][]Operation{}
	holderResource := map[string]string{}
	for _, h := range p.Holders {
		holderResource[h.ID] = h.ResourceID
	}
	for _, op := range p.Operations {
		if op.Kind == OpRenew {
			r := holderResource[op.HolderID]
			byResource[r] = append(byResource[r], op)
		}
	}
	out := map[int]bool{}
	for resourceID, ops := range byResource {
		sort.SliceStable(ops, func(i, j int) bool { return ops[i].Seq < ops[j].Seq })
		budget := optionBudget
		if budget == 0 {
			if r := resourceByID(p, resourceID); r != nil {
				budget = r.RenewAttemptBudget
			}
		}
		if budget <= 0 {
			continue
		}
		for i, op := range ops {
			if i >= budget {
				out[op.Seq] = true
			}
		}
	}
	return out
}

func (e *engine) handle(op Operation) {
	h := holderByID(e.p, op.HolderID)
	if h == nil {
		e.results[op.Seq] = &OpResult{Seq: op.Seq, Code: CodeUnknownHolder}
		return
	}
	r := resourceByID(e.p, h.ResourceID)
	st := e.states[op.HolderID]

	switch op.Kind {
	case OpAcquire:
		e.expireResource(r.ID, op.AtMS)
		e.handleAcquire(op, h, r, st)
	case OpRenew:
		e.expireResource(r.ID, op.AtMS)
		e.handleRenew(op, h, r, st)
	case OpRelease:
		e.expireResource(r.ID, op.AtMS)
		e.handleRelease(op, h, st)
	case OpAbandon:
		e.expireResource(r.ID, op.AtMS)
		res := e.baseResult(op, h, r)
		if !st.Active {
			res.Code = CodeNoActiveLease
			res.Granted = false
			res.Source = SourceRef{HolderID: h.ID, AtMS: op.AtMS, Reason: "abandon ignored: no active lease"}
		} else {
			res.Code = CodeOK
			res.Granted = true
			res.ExpiresAtMS = st.ExpiresAtMS
			res.RemainingMS = st.ExpiresAtMS - op.AtMS
			res.RenewCount = st.RenewCount
			res.Source = SourceRef{HolderID: h.ID, Seq: op.Seq, Event: "abandon_observed", AtMS: op.AtMS, Reason: "holder abandoned operation flow; lease remains until deadline"}
		}
		e.results[op.Seq] = res
	}
}

func (e *engine) baseResult(op Operation, h *Holder, r *Resource) *OpResult {
	return &OpResult{
		Seq:        op.Seq,
		HolderID:   h.ID,
		ResourceID: r.ID,
		Kind:       op.Kind,
		AtMS:       op.AtMS,
	}
}

func (e *engine) handleAcquire(op Operation, h *Holder, r *Resource, st *HolderState) {
	res := e.baseResult(op, h, r)
	if st.Active {
		res.Code = CodeOK
		res.Granted = true
		res.ExpiresAtMS = st.ExpiresAtMS
		res.Source = SourceRef{HolderID: h.ID, Seq: st.LastSeq, Event: "existing_lease", AtMS: st.ExpiresAtMS, Reason: "holder already active"}
		e.results[op.Seq] = res
		return
	}

	blocker := e.findBlocker(r, h.ID)
	if blocker != nil {
		res.Code = CodeResourceHeld
		res.Granted = false
		res.ExpiresAtMS = blocker.ExpiresAtMS
		res.RemainingMS = blocker.ExpiresAtMS - op.AtMS
		res.Source = SourceRef{
			HolderID: blocker.HolderID,
			Seq:      blocker.LastSeq,
			Event:    "active_lease_expiry",
			AtMS:     blocker.ExpiresAtMS,
			Reason:   fmt.Sprintf("%s resource blocked by holder %s until its declared expiry", r.Mode, blocker.HolderID),
		}
		e.results[op.Seq] = res
		return
	}

	expiry, err := checkedAddMilli(op.AtMS, r.LeaseTTLMS)
	if err != nil {
		res.Code = CodeCountOverflow
		res.Diagnostic = err.Error()
		e.results[op.Seq] = res
		return
	}
	st.Active = true
	st.RenewCount = 0
	st.ExpiresAtMS = expiry
	st.LastSeq = op.Seq
	res.Code = CodeOK
	res.Granted = true
	res.ExpiresAtMS = expiry
	res.RemainingMS = r.LeaseTTLMS
	res.Source = SourceRef{HolderID: h.ID, Seq: op.Seq, Event: "acquire_granted", AtMS: op.AtMS, Reason: fmt.Sprintf("%s lock lease TTL=%dms", r.Mode, r.LeaseTTLMS)}
	e.results[op.Seq] = res
}

func (e *engine) findBlocker(r *Resource, requestingHolder string) *HolderState {
	var blockers []*HolderState
	for _, h := range e.p.Holders {
		if h.ResourceID != r.ID || h.ID == requestingHolder {
			continue
		}
		st := e.states[h.ID]
		if st.Active {
			blockers = append(blockers, st)
		}
	}
	if len(blockers) == 0 {
		return nil
	}
	if r.Mode == ModeShared {
		var exclusive []*HolderState
		for _, st := range blockers {
			h := holderByID(e.p, st.HolderID)
			rr := resourceByID(e.p, h.ResourceID)
			if rr.Mode == ModeExclusive {
				exclusive = append(exclusive, st)
			}
		}
		blockers = exclusive
		if len(blockers) == 0 {
			return nil
		}
	}
	sort.SliceStable(blockers, func(i, j int) bool {
		if blockers[i].ExpiresAtMS != blockers[j].ExpiresAtMS {
			return blockers[i].ExpiresAtMS > blockers[j].ExpiresAtMS
		}
		return blockers[i].HolderID < blockers[j].HolderID
	})
	return blockers[0]
}

type renewEvaluation struct {
	code        string
	open        Milli
	close       Milli
	remaining   Milli
	budgetLimit bool
}

func (e *engine) evaluateRenew(op Operation, r *Resource, st *HolderState) renewEvaluation {
	closeAt := st.ExpiresAtMS
	openAt := closeAt - r.RenewWindowMS
	remaining := closeAt - op.AtMS
	ev := renewEvaluation{code: CodeOK, open: openAt, close: closeAt, remaining: remaining}
	if e.budget[op.Seq] {
		ev.code = CodeAttemptsExhausted
		ev.budgetLimit = true
		return ev
	}
	if closeAt == 0 {
		ev.code = CodeNoActiveLease
		return ev
	}
	if op.AtMS == closeAt {
		ev.code = CodeRenewWindowClosed
		return ev
	}
	if op.AtMS > closeAt {
		ev.code = CodeRenewExpired
		return ev
	}
	if !st.Active {
		ev.code = CodeNoActiveLease
		return ev
	}
	if op.AtMS < openAt {
		ev.code = CodeRenewTooEarly
		return ev
	}
	if st.RenewCount >= r.MaxRenewals {
		ev.code = CodeRenewLimitReached
		return ev
	}
	return ev
}

func (e *engine) handleRenew(op Operation, h *Holder, r *Resource, st *HolderState) {
	res := e.baseResult(op, h, r)
	if override, ok := e.opts.ResultOverrides[op.Seq]; ok {
		e.applyRenewOverride(op, h, r, st, res, override)
		return
	}
	ev := e.evaluateRenew(op, r, st)
	res.WindowOpenMS = ev.open
	res.WindowCloseMS = ev.close
	res.RemainingMS = ev.remaining
	res.ExpiresAtMS = st.ExpiresAtMS
	res.RenewCount = st.RenewCount

	if op.Cohort != "" {
		if arb := e.arb[op.Cohort]; arb != nil {
			res.Arbitration = arb
		}
		winner := e.arbitrationWinner(op.Cohort)
		if winner != 0 && op.Seq != winner {
			if ev.code == CodeOK {
				ev.code = CodeCompetitionLost
			}
		}
	}

	if ev.code != CodeOK {
		res.Code = ev.code
		res.Granted = false
		if ev.code == CodeAttemptsExhausted {
			budget := e.opBudget(r)
			res.Source = SourceRef{HolderID: r.ID, Event: "resource_attempt_budget", Reason: fmt.Sprintf("per-resource renewal attempt budget %d exhausted", budget)}
		} else {
			res.Source = SourceRef{HolderID: h.ID, Seq: st.LastSeq, Event: "original_lease_deadline", AtMS: st.ExpiresAtMS, Reason: e.rejectionReason(ev.code)}
		}
		e.results[op.Seq] = res
		return
	}

	newExpiry, err := checkedAddMilli(op.AtMS, op.RenewForMS)
	if err != nil {
		res.Code = CodeCountOverflow
		res.Diagnostic = err.Error()
		e.results[op.Seq] = res
		return
	}
	st.ExpiresAtMS = newExpiry
	st.RenewCount++
	st.LastSeq = op.Seq
	res.Code = CodeOK
	res.Granted = true
	res.ExpiresAtMS = newExpiry
	res.RenewCount = st.RenewCount
	res.Source = SourceRef{HolderID: h.ID, Seq: op.Seq, Event: "renewal_granted", AtMS: op.AtMS, Reason: fmt.Sprintf("renewal accepted in [%d,%d); new expiry=%dms", ev.open, ev.close, newExpiry)}
	e.results[op.Seq] = res
}

func (e *engine) applyRenewOverride(op Operation, h *Holder, r *Resource, st *HolderState, res *OpResult, override ResultOverride) {
	ev := e.evaluateRenew(op, r, st)
	res.WindowOpenMS = ev.open
	res.WindowCloseMS = ev.close
	res.RemainingMS = ev.remaining
	if arb := e.arb[op.Cohort]; op.Cohort != "" && arb != nil {
		res.Arbitration = arb
	}
	if override.Granted {
		newExpiry := override.ExpiresAtMS
		if newExpiry == 0 {
			computed, err := checkedAddMilli(op.AtMS, op.RenewForMS)
			if err != nil {
				res.Code = CodeCountOverflow
				res.Diagnostic = err.Error()
				e.results[op.Seq] = res
				return
			}
			newExpiry = computed
		}
		st.Active = true
		st.ExpiresAtMS = newExpiry
		if override.RenewCount > 0 {
			st.RenewCount = override.RenewCount
		} else {
			st.RenewCount++
		}
		st.LastSeq = op.Seq
		res.Code = override.Code
		if res.Code == "" {
			res.Code = CodeOK
		}
		res.Granted = true
		res.ExpiresAtMS = newExpiry
		res.RenewCount = st.RenewCount
		res.Source = SourceRef{HolderID: h.ID, Seq: op.Seq, Event: "edited_renewal_result", AtMS: op.AtMS, Reason: "incremental renewal result override applied"}
		e.results[op.Seq] = res
		return
	}
	res.Code = override.Code
	if res.Code == "" {
		res.Code = CodeCompetitionLost
	}
	res.Granted = false
	if override.ExpiresAtMS != 0 {
		st.ExpiresAtMS = override.ExpiresAtMS
	}
	if override.RenewCount > 0 {
		st.RenewCount = override.RenewCount
	}
	res.ExpiresAtMS = st.ExpiresAtMS
	res.RenewCount = st.RenewCount
	res.Source = SourceRef{HolderID: h.ID, Seq: st.LastSeq, Event: "edited_rejection", AtMS: st.ExpiresAtMS, Reason: "incremental rejected renewal retains edited deadline"}
	e.results[op.Seq] = res
}

func (e *engine) opBudget(r *Resource) int {
	if e.opts.AttemptBudgetPerResource > 0 {
		return e.opts.AttemptBudgetPerResource
	}
	return r.RenewAttemptBudget
}

func (e *engine) rejectionReason(code string) string {
	switch code {
	case CodeRenewTooEarly:
		return "renewal arrived before inclusive window opening"
	case CodeRenewWindowClosed:
		return "upper boundary is exclusive; renewal at exact expiry is closed"
	case CodeRenewExpired:
		return "renewal arrived after declared expiry"
	case CodeRenewLimitReached:
		return "maximum successful renewal count reached; original deadline retained"
	case CodeCompetitionLost:
		return "same-window arbitration selected another holder; original deadline retained"
	case CodeNoActiveLease:
		return "holder has no active lease at operation time"
	default:
		return code
	}
}

func (e *engine) handleRelease(op Operation, h *Holder, st *HolderState) {
	res := e.baseResult(op, h, resourceByID(e.p, h.ResourceID))
	if !st.Active {
		res.Code = CodeNoActiveLease
		res.Granted = false
		res.Source = SourceRef{HolderID: h.ID, AtMS: op.AtMS, Reason: "release ignored: no active lease"}
		e.results[op.Seq] = res
		return
	}
	st.Active = false
	st.LastSeq = op.Seq
	res.Code = CodeOK
	res.Granted = true
	res.ExpiresAtMS = st.ExpiresAtMS
	res.Source = SourceRef{HolderID: h.ID, Seq: op.Seq, Event: "manual_release", AtMS: op.AtMS, Reason: "holder released lock explicitly"}
	e.events = append(e.events, TimedEvent{
		AtMS: op.AtMS, Type: "resource_released", HolderID: h.ID, ResourceID: h.ResourceID,
		Reason: "manual_release", Source: res.Source,
	})
	e.results[op.Seq] = res
}

func (e *engine) expireResource(resourceID string, now Milli) {
	for _, h := range e.p.Holders {
		if h.ResourceID != resourceID {
			continue
		}
		st := e.states[h.ID]
		if !st.Active || st.ExpiresAtMS > now {
			continue
		}
		st.Active = false
		source := SourceRef{
			HolderID: h.ID,
			Seq:      st.LastSeq,
			Event:    "lease_expired",
			AtMS:     st.ExpiresAtMS,
			Reason:   "integer-millisecond deadline reached; invalidation precedes release",
		}
		e.events = append(e.events,
			TimedEvent{AtMS: st.ExpiresAtMS, Type: "lease_invalidated", HolderID: h.ID, ResourceID: resourceID, Reason: CodeRenewExpired, Source: source},
			TimedEvent{AtMS: st.ExpiresAtMS, Type: "resource_released", HolderID: h.ID, ResourceID: resourceID, Reason: "lease_expired", Source: source},
		)
	}
}

func (e *engine) arbitrationWinner(cohort string) int {
	if a := e.arb[cohort]; a != nil {
		return a.WinnerSeq
	}
	return 0
}

func (e *engine) arbitrate(cohort string) {
	var ops []Operation
	for _, op := range e.p.Operations {
		if op.Cohort == cohort && op.Kind == OpRenew {
			ops = append(ops, op)
		}
	}
	sort.SliceStable(ops, func(i, j int) bool { return ops[i].Seq < ops[j].Seq })
	a := &Arbitration{Cohort: cohort, Rule: "priority_desc_then_seq_asc"}
	for _, op := range ops {
		a.Candidates = append(a.Candidates, op.Seq)
	}

	type candidate struct {
		op Operation
		h  *Holder
		ev renewEvaluation
	}
	var eligible []candidate
	for _, op := range ops {
		h := holderByID(e.p, op.HolderID)
		r := resourceByID(e.p, h.ResourceID)
		st := e.states[h.ID]
		ev := e.evaluateRenew(op, r, st)
		if ev.code == CodeOK {
			eligible = append(eligible, candidate{op: op, h: h, ev: ev})
		}
	}
	if len(eligible) == 0 {
		a.Basis = "no window-and-budget eligible candidate; each operation retains its own rejection code"
		e.arb[cohort] = a
		return
	}
	maxOpen := eligible[0].ev.open
	minClose := eligible[0].ev.close
	for _, candidate := range eligible[1:] {
		if candidate.ev.open > maxOpen {
			maxOpen = candidate.ev.open
		}
		if candidate.ev.close < minClose {
			minClose = candidate.ev.close
		}
	}
	a.TemporalOverlap = len(eligible) >= 2 && maxOpen < minClose
	if len(eligible) >= 2 && !a.TemporalOverlap {
		a.Basis = fmt.Sprintf("declared windows do not temporally overlap (common interval [%d,%d)); no same-window arbitration", maxOpen, minClose)
		e.arb[cohort] = a
		return
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		if eligible[i].h.Priority != eligible[j].h.Priority {
			return eligible[i].h.Priority > eligible[j].h.Priority
		}
		return eligible[i].op.Seq < eligible[j].op.Seq
	})
	w := eligible[0]
	a.WinnerSeq = w.op.Seq
	a.Winner = w.h.ID
	if len(eligible) == 1 {
		a.Basis = fmt.Sprintf("sole eligible candidate seq=%d holder=%s", w.op.Seq, w.h.ID)
	} else {
		runner := eligible[1]
		if w.h.Priority != runner.h.Priority {
			a.Basis = fmt.Sprintf("priority decides: %s(priority=%d,seq=%d) > %s(priority=%d,seq=%d)", w.h.ID, w.h.Priority, w.op.Seq, runner.h.ID, runner.h.Priority, runner.op.Seq)
		} else {
			a.Basis = fmt.Sprintf("priority tie at %d; fixed sequence order selects seq=%d over seq=%d", w.h.Priority, w.op.Seq, runner.op.Seq)
		}
	}
	e.arb[cohort] = a
}

func (e *engine) report() *Report {
	results := make([]OpResult, 0, len(e.results))
	for _, r := range e.results {
		results = append(results, *r)
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].Seq < results[j].Seq })

	events := append([]TimedEvent(nil), e.events...)
	eventRank := map[string]int{"lease_invalidated": 0, "resource_released": 1, "lease_acquired": 2}
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].AtMS != events[j].AtMS {
			return events[i].AtMS < events[j].AtMS
		}
		if events[i].ResourceID != events[j].ResourceID {
			return events[i].ResourceID < events[j].ResourceID
		}
		if events[i].HolderID != events[j].HolderID {
			return events[i].HolderID < events[j].HolderID
		}
		if eventRank[events[i].Type] != eventRank[events[j].Type] {
			return eventRank[events[i].Type] < eventRank[events[j].Type]
		}
		return events[i].Type < events[j].Type
	})
	for i := range events {
		events[i].Order = i + 1
	}

	states := make([]HolderState, 0, len(e.p.Holders))
	for _, h := range e.p.Holders {
		st := e.states[h.ID]
		states = append(states, *st)
	}
	return &Report{
		SchemaVersion: CanonicalSchemaVersion,
		ProjectID:     e.p.ProjectID,
		Seed:          e.p.Seed,
		ToleranceMS:   0,
		Results:       results,
		Events:        events,
		HolderStates:  states,
	}
}
