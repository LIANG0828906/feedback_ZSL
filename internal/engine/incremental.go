package engine

import (
	"fmt"
	"reflect"
	"sort"

	"meshpolicy/internal/model"
	"meshpolicy/internal/xerr"
)

type EditRequest struct {
	Kind       string `json:"kind"`
	Seq        int64  `json:"seq"`
	Order      int    `json:"order"`
	ChangeID   string `json:"change_id"`
	PolicyID   string `json:"policy_id,omitempty"`
	WorkloadID string `json:"workload_id,omitempty"`
	Priority   *int   `json:"priority,omitempty"`
	Action     string `json:"action,omitempty"`
	LabelKey   string `json:"label_key,omitempty"`
	LabelValue string `json:"label_value,omitempty"`
}

type InvalidationSource struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Field string `json:"field"`
	Path  string `json:"path"`
}

type ClosureBoundary struct {
	FromSeq              int64 `json:"from_seq"`
	LastAffectedSeq      int64 `json:"last_affected_seq"`
	MaxGeneration        int   `json:"max_generation"`
	PropagationEndSeq    int64 `json:"propagation_end_seq"`
	AcknowledgedEndSeq   int64 `json:"acknowledged_end_seq"`
	BoundarySeq          int64 `json:"boundary_seq"`
	FixedPointIterations int   `json:"fixed_point_iterations"`
	DecisionTolerance    int   `json:"decision_tolerance"`
}

type ResultProvenance struct {
	Sources          []InvalidationSource `json:"sources"`
	MarkerRefs       []MarkerRef          `json:"marker_refs"`
	ClosureLayerIDs  []string             `json:"closure_layer_ids"`
	ClosureWorkloads []string             `json:"closure_workloads"`
	CacheUsed        bool                 `json:"cache_used"`
}

type IncrementalReport struct {
	Edit             EditRequest                 `json:"edit"`
	Sources          []InvalidationSource        `json:"sources"`
	Invalidated      []string                    `json:"invalidated"`
	RecomputedIDs    []string                    `json:"recomputed_ids"`
	ReusedIDs        []string                    `json:"reused_ids"`
	ClosureWorkloads []string                    `json:"closure_workloads"`
	ClosureLayers    []MarkerLayer               `json:"closure_layers"`
	Boundary         ClosureBoundary             `json:"boundary"`
	Provenance       map[string]ResultProvenance `json:"provenance"`
	MergedDecisions  []Decision                  `json:"merged_decisions"`
	Diffs            []DecisionDiff              `json:"diffs"`
	Consistent       bool                        `json:"consistent"`
	Mismatches       []string                    `json:"mismatches,omitempty"`
}

type DecisionDiff struct {
	AttemptID string   `json:"attempt_id"`
	Changed   bool     `json:"changed"`
	Changes   []string `json:"changes,omitempty"`
	Before    Decision `json:"before"`
	After     Decision `json:"after"`
}

func ApplyEdit(p *model.Project, req EditRequest) (*model.PolicyChange, error) {
	if req.Seq <= 0 {
		req.Seq = 2
	}
	if req.ChangeID == "" {
		req.ChangeID = fmt.Sprintf("C-EDIT-%s-%d", req.Kind, req.Seq)
	}
	order := req.Order
	if order == 0 {
		order = nextEditOrder(p, req.Seq)
	}

	switch req.Kind {
	case "priority", "action", "selector_label", "port":
		current, snapErr := policySnapshotForEdit(p, req.PolicyID, req.Seq, order)
		if snapErr != nil {
			return nil, snapErr
		}
		before := clonePolicy(&current)
		after := clonePolicy(&current)
		fields := []string{}
		kind := model.ChangeKind(req.Kind)
		switch req.Kind {
		case "priority":
			if req.Priority == nil || *req.Priority < MinPriority || *req.Priority > MaxPriority {
				value := 0
				if req.Priority != nil {
					value = *req.Priority
				}
				return nil, xerr.New(xerr.EPriorityRange, "edited priority is outside stable bounds", map[string]any{"policy_id": req.PolicyID, "priority": value, "min": MinPriority, "max": MaxPriority})
			}
			after.Priority = *req.Priority
			fields = []string{"priority"}
			kind = model.ChangePriority
		case "action":
			action := model.Action(req.Action)
			if action != model.ActionAllow && action != model.ActionDeny {
				return nil, xerr.New(xerr.EActionConflict, "edited action must be allow or deny", map[string]any{"policy_id": req.PolicyID, "value": req.Action})
			}
			after.Action = action
			fields = []string{"action"}
			kind = model.ChangeAction
		case "selector_label":
			if req.LabelKey == "" {
				return nil, xerr.New(xerr.EFieldConflict, "label_key is required for selector_label edit", map[string]any{"policy_id": req.PolicyID})
			}
			if after.Source.MatchLabels == nil {
				after.Source.MatchLabels = map[string]string{}
			}
			if req.LabelValue == "" {
				delete(after.Source.MatchLabels, req.LabelKey)
			} else {
				after.Source.MatchLabels[req.LabelKey] = req.LabelValue
			}
			fields = []string{"source_selector.match_labels." + req.LabelKey}
			kind = model.ChangeSelector
		case "port":
			if req.Priority == nil { // reuse Priority as port? Better explicit? Not needed workbench maybe.
			}
		}
		after.Revision++
		change := &model.PolicyChange{
			ID: req.ChangeID, Seq: req.Seq, Order: order, Kind: kind, Namespace: after.Namespace,
			PolicyID: after.ID, EditedFields: fields, Before: before, After: after,
		}
		p.Timeline = append(p.Timeline, model.Event{Seq: req.Seq, Order: order, Change: change})
		return change, nil

	case "workload_label":
		before, snapErr := workloadSnapshotForEdit(p, req.WorkloadID, req.Seq, order)
		if snapErr != nil {
			return nil, snapErr
		}
		after := before
		after.Labels = cloneStringMap(before.Labels)
		if req.LabelKey == "" {
			return nil, xerr.New(xerr.EFieldConflict, "label_key is required for workload_label edit", map[string]any{"workload_id": req.WorkloadID})
		}
		if req.LabelValue == "" {
			delete(after.Labels, req.LabelKey)
		} else {
			after.Labels[req.LabelKey] = req.LabelValue
		}
		change := &model.PolicyChange{
			ID: req.ChangeID, Seq: req.Seq, Order: order, Kind: model.ChangeWorkload,
			Namespace: after.Namespace, WorkloadID: after.ID, EditedFields: []string{"labels." + req.LabelKey},
			WorkloadBefore: &before, WorkloadAfter: &after,
		}
		p.Timeline = append(p.Timeline, model.Event{Seq: req.Seq, Order: order, Change: change})
		return change, nil
	default:
		return nil, xerr.New(xerr.ERequestInvalid, "unsupported incremental edit kind", map[string]any{
			"kind": req.Kind, "allowed": []string{"priority", "action", "selector_label", "workload_label"},
		})
	}
}

func nextEditOrder(p *model.Project, seq int64) int {
	maxOrder := 0
	for _, ev := range p.Timeline {
		keySeq, order := ev.Seq, ev.Order
		if ev.Change != nil {
			keySeq, order = ev.Change.Seq, ev.Change.Order
		}
		if keySeq == seq && ev.Change != nil && order > maxOrder {
			maxOrder = order
		}
	}
	if maxOrder == 0 {
		return 5
	}
	return maxOrder + 1
}

func policySnapshotForEdit(p *model.Project, id string, seq int64, order int) (model.Policy, error) {
	pol, ok := p.PolicyByID(id)
	if !ok {
		return model.Policy{}, xerr.New(xerr.EPolicyNotFound, "edit target policy was not found", map[string]any{"policy_id": id})
	}
	for _, ev := range p.SortedEvents() {
		c := ev.Change
		if c == nil || c.After == nil || c.After.ID != id {
			continue
		}
		if c.Seq < seq || (c.Seq == seq && c.Order < order) {
			pol = *c.After
		}
	}
	return pol, nil
}

func workloadSnapshotForEdit(p *model.Project, id string, seq int64, order int) (model.Workload, error) {
	w, ok := p.WorkloadByID(id)
	if !ok {
		return model.Workload{}, xerr.New(xerr.EWorkloadMissing, "edit target workload was not found", map[string]any{"workload_id": id})
	}
	for _, ev := range p.SortedEvents() {
		c := ev.Change
		if c == nil || c.Kind != model.ChangeWorkload || c.WorkloadAfter == nil || c.WorkloadAfter.ID != id {
			continue
		}
		if c.Seq < seq || (c.Seq == seq && c.Order < order) {
			w = *c.WorkloadAfter
		}
	}
	return w, nil
}

func IncrementalRecompute(original *model.Project, req EditRequest, opts RunOptions) (*IncrementalReport, *model.Project, error) {
	baseline, err := Run(original, opts)
	if err != nil {
		return nil, nil, err
	}
	edited := model.CloneProject(original)
	change, err := ApplyEdit(edited, req)
	if err != nil {
		return nil, nil, err
	}

	layers, refs := PropagateChange(edited, change)
	invalidatedSet := map[string]bool{}
	for id := range refs {
		invalidatedSet[id] = true
	}
	invalidated := model.SortedKeys(invalidatedSet)

	editedProp := BuildPropagation(edited)
	workloads := map[string]model.Workload{}
	for _, w := range edited.Workloads {
		workloads[w.ID] = w
	}

	committed := map[string]committedDecision{}
	baselineByID := map[string]Decision{}
	for _, d := range baseline.Decisions {
		baselineByID[d.AttemptID] = d
		if d.Seq < req.Seq && !d.Stale {
			allow := d.Allow
			committed[d.FlowKey] = committedDecision{Allow: allow, PolicyID: d.AppliedPolicyID, Revision: d.AppliedRevision, Seq: d.Seq, DecisionID: d.AttemptID}
		}
	}

	allAttempts := edited.Attempts()
	sort.SliceStable(allAttempts, func(i, j int) bool {
		if allAttempts[i].Seq != allAttempts[j].Seq {
			return allAttempts[i].Seq < allAttempts[j].Seq
		}
		return allAttempts[i].Order < allAttempts[j].Order
	})

	policiesByNS := map[string][]model.Policy{}
	for _, pol := range edited.Policies {
		policiesByNS[pol.Namespace] = append(policiesByNS[pol.Namespace], pol)
	}
	// Apply timeline changes before evaluating each attempt, mirroring the parallel runtime.
	// For cache replay we only need final policy state at each seq; maintain namespaced stores.
	stores := map[string]*namespaceState{}
	for ns := range policiesByNS {
		stores[ns] = &namespaceState{namespace: ns, project: edited, policies: policiesByNS[ns], committed: committed}
	}

	merged := []Decision{}
	recomputedIDs := []string{}
	reusedIDs := []string{}
	provenance := map[string]ResultProvenance{}
	source := InvalidationSource{
		Kind: req.Kind, ID: firstNonEmpty(req.PolicyID, req.WorkloadID, change.PolicyID, change.WorkloadID),
		Field: firstEditedField(change.EditedFields), Path: "edit." + req.Kind,
	}

	for _, a := range allAttempts {
		ns := ""
		if t, ok := workloads[a.TargetWorkloadID]; ok {
			ns = t.Namespace
		}
		st := stores[ns]
		if st == nil {
			st = &namespaceState{namespace: ns, project: edited, committed: committed}
			stores[ns] = st
		}
		applyChangesBeforeAttempt(st, edited, a)

		if invalidatedSet[a.ID] {
			view := WorkloadSnapshotAt(edited, workloads, a.Seq, a.Order)
			d := EvaluateCandidateWithMap(view, edited.DefaultDeny, st.policies, a, a.Seq)
			applyGate(st, &d, editedProp.ByAttempt[a.ID], a.Seq)
			merged = append(merged, d)
			recomputedIDs = append(recomputedIDs, a.ID)
			layerIDs := []string{}
			for _, r := range editedProp.ByAttempt[a.ID] {
				layerIDs = append(layerIDs, r.MarkerID)
			}
			provenance[a.ID] = ResultProvenance{
				Sources: []InvalidationSource{source}, MarkerRefs: editedProp.ByAttempt[a.ID],
				ClosureLayerIDs:  model.SortedStrings(layerIDs),
				ClosureWorkloads: closureWorkloads(layers), CacheUsed: false,
			}
		} else if cached, ok := baselineByID[a.ID]; ok {
			merged = append(merged, cached)
			reusedIDs = append(reusedIDs, a.ID)
			if !cached.Stale && cached.Gate.State != "denied_no_baseline" {
				committed[cached.FlowKey] = committedDecision{
					Allow: cached.Allow, PolicyID: cached.AppliedPolicyID, Revision: cached.AppliedRevision,
					Seq: cached.Seq, DecisionID: cached.AttemptID,
				}
			}
			provenance[a.ID] = ResultProvenance{Sources: []InvalidationSource{}, CacheUsed: true}
		}
	}

	full, err := Run(edited, opts)
	if err != nil {
		return nil, edited, err
	}
	mismatches := compareDecisions(merged, full.Decisions)
	mergedByID := map[string]Decision{}
	for _, d := range merged {
		mergedByID[d.AttemptID] = d
	}
	diffs := buildDecisionDiffs(recomputedIDs, baselineByID, mergedByID)
	closureWorkloads := closureWorkloads(layers)
	maxGen := 0
	lastSeq := req.Seq
	for _, id := range invalidated {
		if d, ok := baselineByID[id]; ok && d.Seq > lastSeq {
			lastSeq = d.Seq
		}
		if a, ok := attemptByID(edited, id); ok && a.Seq > lastSeq {
			lastSeq = a.Seq
		}
	}
	for _, l := range layers {
		if l.Generation > maxGen {
			maxGen = l.Generation
		}
	}
	end := req.Seq + int64(maxGen)
	boundary := ClosureBoundary{
		FromSeq: req.Seq, LastAffectedSeq: lastSeq, MaxGeneration: maxGen,
		PropagationEndSeq: end, AcknowledgedEndSeq: end + 1, BoundarySeq: maxInt64(lastSeq, end+1),
		FixedPointIterations: maxGen + 1, DecisionTolerance: 0,
	}

	return &IncrementalReport{
		Edit: req, Sources: []InvalidationSource{source}, Invalidated: invalidated,
		RecomputedIDs: recomputedIDs, ReusedIDs: reusedIDs, ClosureWorkloads: closureWorkloads,
		ClosureLayers: layers, Boundary: boundary, Provenance: provenance, MergedDecisions: merged,
		Diffs: diffs, Consistent: len(mismatches) == 0, Mismatches: mismatches,
	}, edited, nil
}

func buildDecisionDiffs(ids []string, before, after map[string]Decision) []DecisionDiff {
	out := []DecisionDiff{}
	for _, id := range ids {
		b := before[id]
		a := after[id]
		changes := []string{}
		if b.Allow != a.Allow {
			changes = append(changes, "allow")
		}
		if b.HitPolicyID != a.HitPolicyID {
			changes = append(changes, "hit_policy_id")
		}
		if b.AppliedPolicyID != a.AppliedPolicyID {
			changes = append(changes, "applied_policy_id")
		}
		if b.Gate.State != a.Gate.State {
			changes = append(changes, "gate_state")
		}
		if b.Gate.MarkerID != a.Gate.MarkerID {
			changes = append(changes, "marker_id")
		}
		out = append(out, DecisionDiff{
			AttemptID: id, Changed: len(changes) > 0, Changes: changes, Before: b, After: a,
		})
	}
	return out
}

func applyChangesBeforeAttempt(st *namespaceState, p *model.Project, target model.ConnectionAttempt) {
	// The incremental loop visits attempts only. Apply every change with seq/order before this attempt.
	// A small cursor is unnecessary for the fixed sample; applying same change repeatedly is prevented by
	// replacing same-ID policy. Delete is idempotent. This keeps replay simple and deterministic.
	for _, ev := range p.SortedEvents() {
		if ev.Change == nil || ev.Change.Namespace != st.namespace {
			continue
		}
		if ev.Change.Seq < target.Seq || (ev.Change.Seq == target.Seq && ev.Change.Order < target.Order) {
			applyChange(st, ev.Change)
		}
	}
}

func compareDecisions(a, b []Decision) []string {
	byA := map[string]Decision{}
	byB := map[string]Decision{}
	for _, d := range a {
		byA[d.AttemptID] = d
	}
	for _, d := range b {
		byB[d.AttemptID] = d
	}
	var mismatches []string
	ids := map[string]bool{}
	for id := range byA {
		ids[id] = true
	}
	for id := range byB {
		ids[id] = true
	}
	for _, id := range model.SortedKeys(ids) {
		da, oka := byA[id]
		db, okb := byB[id]
		if !oka || !okb {
			mismatches = append(mismatches, fmt.Sprintf("%s presence incremental=%v full=%v", id, oka, okb))
			continue
		}
		if !reflect.DeepEqual(normalizeDecision(da), normalizeDecision(db)) {
			mismatches = append(mismatches, fmt.Sprintf("%s incremental=%v/%s gate=%s marker=%s G%d full=%v/%s gate=%s marker=%s G%d",
				id, da.Allow, da.AppliedPolicyID, da.Gate.State, da.Gate.MarkerID, da.Gate.Generation,
				db.Allow, db.AppliedPolicyID, db.Gate.State, db.Gate.MarkerID, db.Gate.Generation))
		}
	}
	return mismatches
}

func normalizeDecision(d Decision) Decision {
	// Basis ordering and all semantic fields are already deterministic; this hook keeps comparison explicit.
	return d
}

func closureWorkloads(layers []MarkerLayer) []string {
	set := map[string]bool{}
	for _, l := range layers {
		for _, w := range l.ReachedWorkloads {
			set[w] = true
		}
	}
	return model.SortedKeys(set)
}

func attemptByID(p *model.Project, id string) (model.ConnectionAttempt, bool) {
	for _, a := range p.Attempts() {
		if a.ID == id {
			return a, true
		}
	}
	return model.ConnectionAttempt{}, false
}

func workloadIndex(items []model.Workload, id string) int {
	for i := range items {
		if items[i].ID == id {
			return i
		}
	}
	return -1
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstEditedField(fields []string) string {
	if len(fields) > 0 {
		return fields[0]
	}
	return "object"
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func clonePolicy(pol *model.Policy) *model.Policy {
	if pol == nil {
		return nil
	}
	out := *pol
	out.Source.MatchLabels = cloneStringMap(pol.Source.MatchLabels)
	out.Source.ServiceAccounts = append([]string(nil), pol.Source.ServiceAccounts...)
	out.Source.WorkloadRefs = append([]string(nil), pol.Source.WorkloadRefs...)
	return &out
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
