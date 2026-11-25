package engine

import (
	"sort"
	"time"

	"tlscheck/internal/bizerr"
	"tlscheck/internal/model"
)

func Run(project model.Project, opts ...Options) (*Report, error) {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	order := o.EndpointOrder
	if len(order) == 0 {
		for _, ep := range project.Endpoints {
			order = append(order, ep.ID)
		}
		sort.Strings(order)
	}
	runs := make([]EndpointRun, 0, len(order))
	var interruptErr error
	for _, epID := range order {
		var initial *EndpointState
		if o.InitialStates != nil {
			initial = o.InitialStates[epID]
		}
		start := time.Now()
		results, state, epsMoments, err := RunEndpoint(project, epID, initial, o)
		runs = append(runs, EndpointRun{
			ID: epID, Results: results, State: state, Moments: epsMoments,
			Err: err, ElapsedNS: time.Since(start).Nanoseconds(),
		})
		if err != nil && interruptErr == nil {
			interruptErr = err
		}
	}
	report := MergeRuns(project, runs)
	return report, interruptErr
}

type EndpointRun struct {
	ID        string
	Results   []*RequestResult
	State     *EndpointState
	Moments   []MomentReport
	Err       error
	ElapsedNS int64
}

func MergeRuns(project model.Project, runs []EndpointRun) *Report {
	hash, _ := project.CanonicalHash()
	report := &Report{
		ProjectID: project.ProjectID, SnapshotHash: hash, Seed: project.Seed,
		EndpointStates: map[string]*EndpointState{},
		Diagnostics:    Diagnostics{EndpointElapsedNS: map[string]int64{}},
	}
	moments := map[int64]*MomentReport{}
	for _, run := range runs {
		report.Results = append(report.Results, run.Results...)
		if run.State != nil {
			report.EndpointStates[run.ID] = run.State
		}
		if run.ElapsedNS > 0 {
			report.Diagnostics.EndpointElapsedNS[run.ID] = run.ElapsedNS
		}
		for _, m := range run.Moments {
			mm := moments[m.TimeMs]
			if mm == nil {
				cp := m
				cp.Endpoints = map[string]MomentEndpoint{}
				mm = &cp
				moments[m.TimeMs] = mm
			}
			for k, v := range m.Endpoints {
				mm.Endpoints[k] = v
			}
		}
	}
	times := make([]int64, 0, len(moments))
	for t := range moments {
		times = append(times, t)
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	for i, t := range times {
		m := moments[t]
		m.Sequence = i + 1
		report.Moments = append(report.Moments, *m)
	}
	finalizeQueues(report)
	sort.SliceStable(report.Results, func(i, j int) bool {
		a, b := report.Results[i], report.Results[j]
		if a.EventSequence != b.EventSequence {
			return a.EventSequence < b.EventSequence
		}
		return a.RequestID < b.RequestID
	})
	return report
}

func RunEndpoint(project model.Project, endpointID string, initial *EndpointState, opts Options) ([]*RequestResult, *EndpointState, []MomentReport, error) {
	ep, ok := project.Endpoint(endpointID)
	if !ok {
		return nil, nil, nil, bizerr.Newf(bizerr.ENotFound, "endpoint %q not found", endpointID)
	}
	reqByID := map[string]*model.Request{}
	var events []model.Event
	for _, ev := range project.Events {
		if ev.Request != nil && ev.Request.EndpointID == endpointID {
			reqByID[ev.Request.ID] = ev.Request
			events = append(events, ev)
		}
		if ev.Update != nil && ev.Update.EndpointID == endpointID {
			events = append(events, ev)
		}
	}
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].AtMs != events[j].AtMs {
			return events[i].AtMs < events[j].AtMs
		}
		return events[i].Sequence < events[j].Sequence
	})

	state := initial
	if state == nil {
		state = newState(project, ep)
	}
	ensureStateMaps(state)
	results := []*RequestResult{}
	byID := map[string]*RequestResult{}
	if opts.InitialResults != nil {
		for _, rr := range opts.InitialResults[endpointID] {
			cp := rr
			results = append(results, cp)
			byID[cp.RequestID] = cp
		}
	}

	var moments []MomentReport
	guard := 0
	for {
		guard++
		if guard > 100000 {
			return results, state, moments, bizerr.New(bizerr.EInvalidArgument, "engine scheduling loop exceeded deterministic guard")
		}
		t, has := nextWake(ep, state, events)
		if !has {
			break
		}
		advancePeriod(state, ep, t)
		if err := commitDueUpdates(project, ep, state, t, opts); err != nil {
			moments = append(moments, snapshot(t, len(moments)+1, state))
			return results, state, moments, err
		}
		if err := resumeIncomplete(project, ep, state, byID, t, opts); err != nil {
			moments = append(moments, snapshot(t, len(moments)+1, state))
			return results, state, moments, err
		}
		completeHandshakes(state, byID, t)
		if err := processQueue(project, ep, state, reqByID, byID, t, opts); err != nil {
			moments = append(moments, snapshot(t, len(moments)+1, state))
			return results, state, moments, err
		}
		if err := processEvents(project, ep, events, state, reqByID, byID, &results, t, opts); err != nil {
			moments = append(moments, snapshot(t, len(moments)+1, state))
			return results, state, moments, err
		}
		state.CommittedTimeMs = t
		moments = append(moments, snapshot(t, len(moments)+1, state))
		if opts.ObserveState != nil {
			opts.ObserveState(endpointID, t, state)
		}
	}
	return results, state, moments, nil
}

func newState(project model.Project, ep model.Endpoint) *EndpointState {
	st := &EndpointState{
		EndpointID: ep.ID, PeriodStartMs: 0, CurrentChainID: ep.ChainIDs[0],
		NextQueueSequence: 1, Active: map[string]*ActiveHandshake{},
		ProcessedEvents: map[int]bool{}, PendingUpdates: map[string]*model.ChainUpdate{},
	}
	if ch, ok := project.Chain(st.CurrentChainID); ok {
		r := ValidateChain(ch, 0, pinFor(ep, ch))
		if r.Valid {
			st.LastValidChainID = ch.ID
		} else {
			for _, cid := range ep.ChainIDs {
				if cid == st.CurrentChainID {
					continue
				}
				if candidate, ok := project.Chain(cid); ok {
					cr := ValidateChain(candidate, 0, pinFor(ep, candidate))
					if cr.Valid {
						st.LastValidChainID = cid
						break
					}
				}
			}
		}
	}
	return st
}

func ensureStateMaps(st *EndpointState) {
	if st.Active == nil {
		st.Active = map[string]*ActiveHandshake{}
	}
	if st.ProcessedEvents == nil {
		st.ProcessedEvents = map[int]bool{}
	}
	if st.PendingUpdates == nil {
		st.PendingUpdates = map[string]*model.ChainUpdate{}
	}
}

func pinFor(ep model.Endpoint, ch model.Chain) string {
	if ep.Pinned && ep.PinDigest != "" {
		return ep.PinDigest
	}
	if ch.Pinned && ch.PinDigest != "" {
		return ch.PinDigest
	}
	return ""
}

func nextWake(ep model.Endpoint, st *EndpointState, events []model.Event) (int64, bool) {
	var best int64
	found := false
	consider := func(t int64) {
		if !found || t < best {
			best, found = t, true
		}
	}
	for _, ev := range events {
		if !st.ProcessedEvents[ev.Sequence] {
			consider(ev.AtMs)
		}
	}
	for _, upd := range st.PendingUpdates {
		consider(upd.CommitAtMs)
	}
	for _, a := range st.Active {
		consider(a.FinishAtMs)
	}
	if len(st.Queue) > 0 {
		consider(st.PeriodStartMs + ep.PeriodMs)
	}
	return best, found
}

func advancePeriod(st *EndpointState, ep model.Endpoint, t int64) {
	if ep.PeriodMs <= 0 {
		return
	}
	for t >= st.PeriodStartMs+ep.PeriodMs {
		st.PeriodStartMs += ep.PeriodMs
	}
	var used int64
	for _, charge := range st.Charges {
		if charge.TimeMs >= st.PeriodStartMs {
			used += charge.AmountBP
		}
	}
	st.UsedBudgetBP = used
}

func commitDueUpdates(project model.Project, ep model.Endpoint, st *EndpointState, t int64, opts Options) error {
	for id, upd := range st.PendingUpdates {
		if upd.CommitAtMs > t {
			continue
		}
		if upd.InterruptBeforeCommit || (opts.Faults.InterruptUpdateIDs != nil && opts.Faults.InterruptUpdateIDs[upd.ID]) {
			return bizerr.Newf(bizerr.EInterrupt, "chain update %q interrupted before commit point", upd.ID).
				WithContext("endpoint_id", ep.ID).WithContext("update_id", upd.ID).
				WithContext("rollback_to_update_id", st.LastCommittedUpdateID)
		}
		ch, ok := project.Chain(upd.ChainID)
		if !ok {
			return bizerr.Newf(bizerr.ENotFound, "chain %q not found during commit", upd.ChainID)
		}
		st.CurrentChainID = upd.ChainID
		st.LastCommittedUpdateID = upd.ID
		st.LastCommitAtMs = t
		st.Frozen = false
		st.FrozenReason = ""
		result := ValidateChain(ch, t, pinFor(ep, ch))
		if result.Valid {
			st.LastValidChainID = upd.ChainID
		}
		delete(st.PendingUpdates, id)
	}
	return nil
}

func completeHandshakes(st *EndpointState, byID map[string]*RequestResult, t int64) {
	for id, active := range st.Active {
		if active.FinishAtMs > t {
			continue
		}
		st.ActiveCount--
		if st.ActiveCount < 0 {
			st.ActiveCount = 0
		}
		delete(st.Active, id)
		res := byID[id]
		if res == nil {
			continue
		}
		res.Status = StatusCompleted
		res.CompletedAtMs = t
		res.BudgetChargedBP = active.ChargedBP
		if active.TimedOut {
			res.HandshakeConclusion = HandshakeTimeout
		} else if active.Conclusion != "" {
			res.HandshakeConclusion = active.Conclusion
		} else {
			res.HandshakeConclusion = HandshakeSuccess
		}
	}
}

func snapshot(t int64, seq int, st *EndpointState) MomentReport {
	ids := []string{}
	for _, q := range st.Queue {
		ids = append(ids, q.RequestID)
	}
	return MomentReport{TimeMs: t, Sequence: seq, Endpoints: map[string]MomentEndpoint{
		st.EndpointID: {
			UsedBudgetBP: st.UsedBudgetBP, ReservedBudgetBP: st.ReservedBudgetBP,
			Frozen: st.Frozen, ActiveCount: st.ActiveCount, QueueIDs: ids,
			CurrentChainID: st.CurrentChainID, LastValidChainID: st.LastValidChainID,
		},
	}}
}
