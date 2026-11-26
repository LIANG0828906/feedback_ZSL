package engine

import (
	"context"
	"fmt"
	"sort"

	"meshpolicy/internal/model"
	"meshpolicy/internal/sched"
	"meshpolicy/internal/xerr"
)

type RunOptions struct {
	Workers     int            `json:"workers,omitempty"`
	BufferPerNS int            `json:"buffer_per_ns,omitempty"`
	Buffers     map[string]int `json:"buffers,omitempty"`
	StopAtSeq   int64          `json:"stop_at_seq,omitempty"`
}

type committedDecision struct {
	Allow      bool
	PolicyID   string
	Revision   int
	Seq        int64
	DecisionID string
}

type namespaceState struct {
	namespace string
	project   *model.Project
	policies  []model.Policy
	committed map[string]committedDecision
	decisions []Decision
}

type eventJob struct {
	event model.Event
}

func Run(p *model.Project, opts RunOptions) (*RunReport, error) {
	cfg, err := sched.Config{
		Workers: opts.Workers, BufferPerNS: opts.BufferPerNS, NamespaceBuffers: opts.Buffers,
	}.Normalize()
	if err != nil {
		return nil, err
	}
	prop := BuildPropagation(p)
	workloads := map[string]model.Workload{}
	for _, w := range p.Workloads {
		workloads[w.ID] = w
	}

	events := p.SortedEvents()
	states := map[string]*namespaceState{}
	nsEvents := map[string][]model.Event{}
	for _, ev := range events {
		if opts.StopAtSeq > 0 && ev.Seq > opts.StopAtSeq {
			continue
		}
		ns := eventNamespace(ev, workloads)
		if ns == "" {
			return nil, xerr.New(xerr.EFieldConflict, "cannot determine namespace for timeline event", map[string]any{"seq": ev.Seq})
		}
		if _, ok := states[ns]; !ok {
			state := &namespaceState{
				namespace: ns, project: p, committed: map[string]committedDecision{},
			}
			for _, pol := range p.Policies {
				if pol.Namespace == ns {
					state.policies = append(state.policies, pol)
				}
			}
			states[ns] = state
		}
		nsEvents[ns] = append(nsEvents[ns], ev)
	}

	pipeline, err := sched.NewPipeline[eventJob](cfg)
	if err != nil {
		return nil, err
	}
	for ns := range states {
		pipeline.Register(ns)
	}
	pipeline.Start(context.Background(), func(ns string, job eventJob) error {
		st := states[ns]
		handleEvent(st, job.event, workloads, prop)
		return nil
	})

	var submitErr error
	for _, ev := range events {
		if opts.StopAtSeq > 0 && ev.Seq > opts.StopAtSeq {
			continue
		}
		ns := eventNamespace(ev, workloads)
		if err := pipeline.Submit(ns, eventJob{event: ev}); err != nil {
			submitErr = err
			break
		}
	}
	if waitErr := pipeline.CloseAndWait(); waitErr != nil && submitErr == nil {
		submitErr = waitErr
	}
	if submitErr != nil {
		return nil, submitErr
	}

	decisions := []Decision{}
	for _, ns := range pipeline.Namespaces() {
		if st, ok := states[ns]; ok {
			decisions = append(decisions, st.decisions...)
		}
	}
	sort.SliceStable(decisions, func(i, j int) bool {
		if decisions[i].Seq != decisions[j].Seq {
			return decisions[i].Seq < decisions[j].Seq
		}
		if decisions[i].Order != decisions[j].Order {
			return decisions[i].Order < decisions[j].Order
		}
		return decisions[i].AttemptID < decisions[j].AttemptID
	})

	moments := buildMoments(decisions, prop, opts.StopAtSeq)
	return &RunReport{
		ProjectID: p.ID, Schema: p.SchemaSource, Decisions: decisions, Moments: moments,
		Propagation: prop, WorkerCount: cfg.Workers, BufferPerNS: cfg.BufferPerNS,
	}, nil
}

func eventNamespace(ev model.Event, workloads map[string]model.Workload) string {
	if ev.Attempt != nil {
		if w, ok := workloads[ev.Attempt.TargetWorkloadID]; ok {
			return w.Namespace
		}
	}
	if ev.Change != nil && ev.Change.Namespace != "" {
		return ev.Change.Namespace
	}
	if ev.Change != nil {
		polID := ev.Change.PolicyID
		if ev.Change.Before != nil && ev.Change.Before.Namespace != "" {
			return ev.Change.Before.Namespace
		}
		if polID != "" {
			// Last-resort IDs are not used for generated documents because change.ns is set.
			return ""
		}
	}
	return ""
}

func handleEvent(st *namespaceState, ev model.Event, workloads map[string]model.Workload, prop *PropagationReport) {
	if ev.Change != nil {
		applyChange(st, ev.Change)
	}
	if ev.Attempt != nil {
		view := WorkloadSnapshotAt(st.project, workloads, ev.Attempt.Seq, ev.Attempt.Order)
		d := EvaluateCandidateWithMap(view, st.project.DefaultDeny, st.policies, *ev.Attempt, ev.Attempt.Seq)
		applyGate(st, &d, prop.ByAttempt[ev.Attempt.ID], ev.Attempt.Seq)
		st.decisions = append(st.decisions, d)
	}
}

func applyChange(st *namespaceState, c *model.PolicyChange) {
	if c.Kind == model.ChangeDelete || (c.After == nil && c.PolicyID != "") {
		markPolicyState(st, c.PolicyID, model.StateDeleted)
		if c.Before != nil {
			markPolicyState(st, c.Before.ID, model.StateDeleted)
		}
		return
	}
	if c.After == nil {
		return
	}
	if c.Before != nil && c.Before.ID != c.After.ID {
		markPolicyState(st, c.Before.ID, model.StateReplaced)
		if idx := policyIndex(st.policies, c.Before.ID); idx >= 0 {
			st.policies[idx].ReplacedBy = c.After.ID
		}
	}
	if idx := policyIndex(st.policies, c.After.ID); idx >= 0 {
		st.policies[idx] = *c.After
		return
	}
	st.policies = append(st.policies, *c.After)
}

func WorkloadSnapshotAt(p *model.Project, base map[string]model.Workload, seq int64, order int) map[string]model.Workload {
	view := make(map[string]model.Workload, len(base))
	for id, w := range base {
		view[id] = w
	}
	for _, ev := range p.SortedEvents() {
		c := ev.Change
		if c == nil || c.Kind != model.ChangeWorkload || c.WorkloadAfter == nil {
			continue
		}
		if c.Seq < seq || (c.Seq == seq && c.Order < order) {
			view[c.WorkloadAfter.ID] = *c.WorkloadAfter
		}
	}
	return view
}

func markPolicyState(st *namespaceState, id string, state model.PolicyState) {
	if idx := policyIndex(st.policies, id); idx >= 0 {
		st.policies[idx].State = state
	}
}

func policyIndex(policies []model.Policy, id string) int {
	for i := range policies {
		if policies[i].ID == id {
			return i
		}
	}
	return -1
}

func applyGate(st *namespaceState, d *Decision, refs []MarkerRef, seq int64) {
	pending := pendingRef(refs, seq)
	if pending == nil {
		if len(refs) > 0 {
			d.Gate.State = "applied_new"
			d.Gate.NewPolicyApplied = true
		} else {
			d.Gate.State = "committed"
		}
		allow := d.Allow
		d.Gate.Arrived = true
		d.Gate.Acknowledged = true
		d.Gate.AppliedAllow = &allow
		d.Gate.AppliedPolicyID = d.AppliedPolicyID
		d.Gate.AppliedRevision = d.AppliedRevision
		st.committed[d.FlowKey] = committedDecision{
			Allow: allow, PolicyID: d.AppliedPolicyID, Revision: d.AppliedRevision,
			Seq: seq, DecisionID: d.AttemptID,
		}
		return
	}

	ref := pending.ref
	arrivedGeneration := arrivedGeneration(ref, seq)
	d.Gate = GateInfo{
		State:             "fallback_committed",
		MarkerID:          ref.MarkerID,
		Generation:        ref.Generation,
		ArrivalGeneration: arrivedGeneration,
		Arrived:           seq >= ref.ArrivalSeq,
		Acknowledged:      false,
	}
	if prior, ok := st.committed[d.FlowKey]; ok {
		allow := prior.Allow
		d.Allow = prior.Allow
		d.AppliedPolicyID = prior.PolicyID
		d.AppliedRevision = prior.Revision
		d.Stale = true
		d.ReasonCode = ReasonGateFallback
		d.Gate.Code = ReasonGateFallback
		d.Gate.AppliedPolicyID = prior.PolicyID
		d.Gate.AppliedAllow = &allow
		d.Gate.AppliedRevision = prior.Revision
		d.Gate.NewPolicyApplied = false
		return
	}

	deny := false
	d.Allow = false
	d.AppliedPolicyID = ""
	d.AppliedRevision = 0
	d.Stale = false
	d.ReasonCode = ReasonGateDeny
	d.Gate.State = "denied_no_baseline"
	d.Gate.Code = xerr.EGateNoBaseline
	d.Gate.AppliedAllow = &deny
	d.Gate.NewPolicyApplied = false
}

type pendingMarker struct {
	ref MarkerRef
}

func pendingRef(refs []MarkerRef, seq int64) *pendingMarker {
	var chosen *MarkerRef
	for i := range refs {
		ref := refs[i]
		if seq > ref.ArrivalSeq {
			continue
		}
		if chosen == nil || ref.ArrivalSeq > chosen.ArrivalSeq ||
			(ref.ArrivalSeq == chosen.ArrivalSeq && ref.Generation > chosen.Generation) ||
			(ref.ArrivalSeq == chosen.ArrivalSeq && ref.Generation == chosen.Generation && ref.MarkerID > chosen.MarkerID) {
			copy := ref
			chosen = &copy
		}
	}
	if chosen == nil {
		return nil
	}
	return &pendingMarker{ref: *chosen}
}

func arrivedGeneration(ref MarkerRef, seq int64) int {
	if seq >= ref.ArrivalSeq {
		return ref.Generation
	}
	elapsed := seq - ref.BornSeq
	if elapsed < 0 {
		return 0
	}
	if int(elapsed) > ref.Generation-1 {
		return ref.Generation - 1
	}
	return int(elapsed)
}

func buildMoments(decisions []Decision, prop *PropagationReport, stopAt int64) []MomentReport {
	seqSet := map[int64]bool{}
	for _, d := range decisions {
		seqSet[d.Seq] = true
	}
	for _, l := range prop.Layers {
		if stopAt > 0 && l.ArrivalSeq > stopAt {
			continue
		}
		if l.Generation > 0 {
			seqSet[l.ArrivalSeq] = true
		}
	}
	seqs := make([]int64, 0, len(seqSet))
	for seq := range seqSet {
		if stopAt > 0 && seq > stopAt {
			continue
		}
		seqs = append(seqs, seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	moments := []MomentReport{}
	for _, seq := range seqs {
		m := MomentReport{Seq: seq}
		for _, d := range decisions {
			if d.Seq == seq {
				m.Decisions = append(m.Decisions, d)
			}
		}
		for _, l := range prop.Layers {
			if l.Generation > 0 && l.ArrivalSeq == seq {
				m.Arrivals = append(m.Arrivals, l)
			}
		}
		sort.SliceStable(m.Decisions, func(i, j int) bool { return m.Decisions[i].Order < m.Decisions[j].Order })
		moments = append(moments, m)
	}
	return moments
}

func FormatDecisionLine(d Decision) string {
	verdict := "DENY"
	if d.Allow {
		verdict = "ALLOW"
	}
	if d.Gate.State == "fallback_committed" {
		return fmt.Sprintf("%s %s fallback old=%s marker=%s G%d", d.AttemptID, verdict, d.AppliedPolicyID, d.Gate.MarkerID, d.Gate.Generation)
	}
	return fmt.Sprintf("%s %s hit=%s", d.AttemptID, verdict, d.HitPolicyID)
}
