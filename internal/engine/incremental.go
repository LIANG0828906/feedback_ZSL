package engine

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	"samplingkernel/internal/budget"
	"samplingkernel/internal/errs"
	"samplingkernel/internal/model"
	"samplingkernel/internal/projectvalidate"
	"samplingkernel/internal/sampling"
	"samplingkernel/internal/tail"
)

type Edit struct {
	Type            string               `json:"type"`
	EdgeID          string               `json:"edge_id,omitempty"`
	ServiceID       string               `json:"service_id,omitempty"`
	RuleID          string               `json:"rule_id,omitempty"`
	RatePermyriad   uint32               `json:"rate_permyriad,omitempty"`
	WeightPermyriad uint32               `json:"weight_permyriad,omitempty"`
	Condition       *model.TailCondition `json:"condition,omitempty"`
}

type Cache struct {
	BaseProject *model.Project
	Base        *model.Report
	outcome     *Outcome
}

func NewCache(p *model.Project) *Cache {
	outcome := New(p).RunDetailed()
	return &Cache{BaseProject: p, Base: outcome.Report, outcome: outcome}
}

type collectedTrace struct {
	EnterSeq  int
	SettleSeq int
	Settle    *model.Event
	Spans     []model.Span
	EdgeIDs   map[string]struct{}
	ForcedIDs []string
	Head      *model.HeadDecision
}

type IncrementalResult struct {
	Project *model.Project
	Report  *model.Report
	Closure *model.Closure
}

func (c *Cache) Apply(edit Edit) (*IncrementalResult, error) {
	edited, err := CloneProject(c.BaseProject)
	if err != nil {
		return nil, err
	}
	closure := &model.Closure{EditType: edit.Type}
	rootSource, err := applyEdit(edited, edit)
	if err != nil {
		return nil, err
	}
	closure.RootSource = rootSource
	if err := projectvalidate.Validate(edited); err != nil {
		return nil, err
	}

	report, err := CloneReport(c.Base)
	if err != nil {
		return nil, err
	}
	collected := collectTraces(edited)
	editedEngine := New(edited)

	headByReq := map[string]model.HeadDecision{}
	for _, decision := range c.Base.Head {
		headByReq[decision.RequestID] = decision
	}
	tailByTrace := map[string]model.TailDecision{}
	for _, decision := range c.Base.Tail {
		tailByTrace[decision.TraceID] = decision
	}

	invalidated := map[string]model.InvalidationNode{}
	recomputed := map[string]struct{}{}
	addNode := func(node model.InvalidationNode) {
		if _, exists := invalidated[node.NodeID]; !exists {
			invalidated[node.NodeID] = node
		}
		recomputed[node.NodeID] = struct{}{}
	}

	budgetWindows := map[int64]int{}
	allBudget := false

	switch edit.Type {
	case "edge_rate":
		for _, event := range edited.Events {
			if event.Kind != model.EventRequestEnter || event.Enter.EdgeID != edit.EdgeID {
				continue
			}
			payload := event.Enter
			edge := editedEngine.edges[payload.EdgeID]
			newHead := sampling.DecideHead(edited.ProjectID, edge, payload.RequestID, payload.TraceID)
			headByReq[payload.RequestID] = newHead
			headNode := "head:" + payload.RequestID
			addNode(model.InvalidationNode{
				NodeID: headNode, Kind: "head_decision", Ref: payload.RequestID, EventSeq: event.Seq,
				RootSource: rootSource, ImmediateSource: rootSource,
				Reason: "edge base sampling rate changed",
			})

			oldHead := findHead(c.Base.Head, payload.RequestID)
			if oldHead != nil && oldHead.Keep != newHead.Keep {
				traceID := payload.TraceID
				tailNode := "tail:" + traceID
				addNode(model.InvalidationNode{
					NodeID: tailNode, Kind: "tail_decision", Ref: traceID,
					RootSource: rootSource, ImmediateSource: headNode,
					Reason: fmt.Sprintf("head keep changed %t -> %t", oldHead.Keep, newHead.Keep),
				})
				newTail := recomputeTail(edited, collected[traceID], headByReq)
				oldTail := tailByTrace[traceID]
				tailByTrace[traceID] = newTail
				if oldTail.Keep != newTail.Keep || oldTail.Forced != newTail.Forced {
					markBudgetWindow(edited, budgetWindows, traceID, collected, tailNode, rootSource, invalidated, addNode)
				}
			}
		}

	case "tail_condition":
		for traceID, info := range collected {
			if info.Settle == nil {
				continue
			}
			tailNode := "tail:" + traceID
			addNode(model.InvalidationNode{
				NodeID: tailNode, Kind: "tail_decision", Ref: traceID, EventSeq: info.SettleSeq,
				RootSource: rootSource, ImmediateSource: rootSource,
				Reason: "tail rule condition or declaration changed",
			})
			newTail := recomputeTail(edited, info, headByReq)
			oldTail := tailByTrace[traceID]
			tailByTrace[traceID] = newTail
			if oldTail.Keep != newTail.Keep || oldTail.Forced != newTail.Forced {
				markBudgetWindow(edited, budgetWindows, traceID, collected, tailNode, rootSource, invalidated, addNode)
			}
		}

	case "allocation_weight":
		allBudget = true
		for _, decision := range c.Base.Budget {
			budgetNode := fmt.Sprintf("budget:%d", decision.EventSeq)
			addNode(model.InvalidationNode{
				NodeID: budgetNode, Kind: "budget_decision", Ref: decision.TraceID, EventSeq: decision.EventSeq,
				RootSource: rootSource, ImmediateSource: rootSource,
				Reason: "service allocation weight changed",
			})
		}

	default:
		return nil, errs.New(errs.SchemaInvalidValue, "unsupported incremental edit type: "+edit.Type).AtPath("$.edit.type")
	}

	if allBudget {
		report.Budget = recomputeAllBudget(edited, collected, tailByTrace)
	} else {
		report.Budget = recomputeBudgetWindows(edited, c.outcome, collected, tailByTrace, budgetWindows, report.Budget)
	}

	rebuildSlices(report, headByReq, tailByTrace)
	rebuildRetained(report, collected)
	updateEventViews(report, edited, headByReq, tailByTrace, report.Budget)

	for _, node := range invalidated {
		closure.Nodes = append(closure.Nodes, node)
		if node.EventSeq > 0 {
			closure.EventRanges = appendRange(closure.EventRanges, node.EventSeq)
		}
	}
	sort.Slice(closure.Nodes, func(i, j int) bool { return closure.Nodes[i].NodeID < closure.Nodes[j].NodeID })
	closure.EventRanges = compactRanges(closure.EventRanges)
	closure.Recomputed = sortedStringSet(recomputed)
	closure.Reused = c.reusedNodes(invalidated)

	return &IncrementalResult{Project: edited, Report: report, Closure: closure}, nil
}

func applyEdit(p *model.Project, edit Edit) (string, error) {
	switch edit.Type {
	case "edge_rate":
		for i := range p.Edges {
			if p.Edges[i].ID == edit.EdgeID {
				p.Edges[i].BaseRatePermyriad = edit.RatePermyriad
				return fmt.Sprintf("edge:%s.base_rate_permyriad", edit.EdgeID), nil
			}
		}
		return "", errs.New(errs.SchemaInvalidValue, "edit target edge not found: "+edit.EdgeID).AtPath("$.edit.edge_id")
	case "allocation_weight":
		for i := range p.Allocations {
			if p.Allocations[i].ServiceID == edit.ServiceID {
				p.Allocations[i].WeightPermyriad = edit.WeightPermyriad
				return fmt.Sprintf("allocation:%s.weight_permyriad", edit.ServiceID), nil
			}
		}
		return "", errs.New(errs.SchemaInvalidValue, "edit target service allocation not found: "+edit.ServiceID).AtPath("$.edit.service_id")
	case "tail_condition":
		for i := range p.TailRules {
			if p.TailRules[i].ID == edit.RuleID {
				if edit.Condition != nil {
					p.TailRules[i].Condition = *edit.Condition
				}
				return fmt.Sprintf("tail_rule:%s.condition", edit.RuleID), nil
			}
		}
		return "", errs.New(errs.SchemaInvalidValue, "edit target tail rule not found: "+edit.RuleID).AtPath("$.edit.rule_id")
	default:
		return "", errs.New(errs.SchemaInvalidValue, "unsupported edit type: "+edit.Type).AtPath("$.edit.type")
	}
}

func CloneProject(p *model.Project) (*model.Project, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, errs.Wrap(errs.InvariantViolation, "clone project failed", err)
	}
	var out model.Project
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, errs.Wrap(errs.InvariantViolation, "clone project failed", err)
	}
	return &out, nil
}

func CloneReport(r *model.Report) (*model.Report, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return nil, errs.Wrap(errs.InvariantViolation, "clone report failed", err)
	}
	var out model.Report
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, errs.Wrap(errs.InvariantViolation, "clone report failed", err)
	}
	return &out, nil
}

func collectTraces(p *model.Project) map[string]*collectedTrace {
	out := map[string]*collectedTrace{}
	get := func(id string) *collectedTrace {
		if out[id] == nil {
			out[id] = &collectedTrace{EdgeIDs: map[string]struct{}{}}
		}
		return out[id]
	}
	edgeByID := map[string]model.Edge{}
	for _, edge := range p.Edges {
		edgeByID[edge.ID] = edge
	}
	for _, event := range p.Events {
		switch event.Kind {
		case model.EventRequestEnter:
			info := get(event.Enter.TraceID)
			info.EnterSeq = event.Seq
			info.Head = &model.HeadDecision{RequestID: event.Enter.RequestID}
			if event.Enter.EdgeID != "" {
				info.EdgeIDs[event.Enter.EdgeID] = struct{}{}
			}
		case model.EventPropagate:
			info := get(event.Propagate.TraceID)
			if event.Propagate.EdgeID != "" {
				info.EdgeIDs[event.Propagate.EdgeID] = struct{}{}
			}
		case model.EventSpanReport:
			info := get(event.Span.TraceID)
			info.Spans = append(info.Spans, *event.Span)
			if event.Span.EdgeID != "" {
				info.EdgeIDs[event.Span.EdgeID] = struct{}{}
			}
		case model.EventRootSettle:
			info := get(event.Settle.TraceID)
			eventCopy := event
			info.Settle = &eventCopy
			info.SettleSeq = event.Seq
		}
	}
	for traceID, info := range out {
		for edgeID := range info.EdgeIDs {
			if edge, ok := edgeByID[edgeID]; ok && edge.ForcedRetention {
				info.ForcedIDs = append(info.ForcedIDs, edgeID)
			}
		}
		sort.Strings(info.ForcedIDs)
		_ = traceID
	}
	return out
}

func recomputeTail(p *model.Project, info *collectedTrace, heads map[string]model.HeadDecision) model.TailDecision {
	headKeep := false
	headID := "none"
	if info != nil && info.Settle != nil {
		var requestID string
		for _, event := range p.Events {
			if event.Kind == model.EventRequestEnter && event.Enter.TraceID == info.Settle.Settle.TraceID {
				requestID = event.Enter.RequestID
				break
			}
		}
		if decision, ok := heads[requestID]; ok {
			headKeep = decision.Keep
			headID = requestID
		}
	}
	traceID := ""
	if info != nil && info.Settle != nil {
		traceID = info.Settle.Settle.TraceID
	}
	spans := []model.Span{}
	if info != nil {
		spans = uniqueSpans(info.Spans)
	}
	return tail.Evaluate(tail.Input{
		TraceID: traceID, Rules: p.TailRules, Spans: spans,
		HeadKeep: headKeep, HeadDecisionID: headID,
		Forced: info != nil && len(info.ForcedIDs) > 0, ForcedEdgeIDs: info.ForcedIDs,
	})
}

func markBudgetWindow(p *model.Project, windows map[int64]int, traceID string, collected map[string]*collectedTrace, immediate string, root string, nodes map[string]model.InvalidationNode, add func(model.InvalidationNode)) {
	info := collected[traceID]
	if info == nil || info.Settle == nil {
		return
	}
	start := budgetWindowStart(p, info.Settle.TimestampMS)
	if current, ok := windows[start]; !ok || info.SettleSeq < current {
		windows[start] = info.SettleSeq
	}

	var chain []model.Event
	for _, event := range p.Events {
		if event.Kind == model.EventRootSettle && budgetWindowStart(p, event.TimestampMS) == start && event.Seq >= windows[start] {
			chain = append(chain, event)
		}
	}
	sort.Slice(chain, func(i, j int) bool { return chain[i].Seq < chain[j].Seq })
	previous := immediate
	for i, event := range chain {
		source := previous
		if i == 0 {
			source = immediate
		}
		nodeID := fmt.Sprintf("budget:%d", event.Seq)
		add(model.InvalidationNode{
			NodeID: nodeID, Kind: "budget_decision", Ref: event.Settle.TraceID, EventSeq: event.Seq,
			RootSource: root, ImmediateSource: source,
			Reason: "budget window deduction chain",
		})
		previous = nodeID
	}
}

func budgetWindowStart(p *model.Project, timestampMS int64) int64 {
	window := p.Budget.WindowMS
	return timestampMS - (timestampMS % window)
}

func recomputeBudgetWindows(p *model.Project, baseline *Outcome, collected map[string]*collectedTrace, tails map[string]model.TailDecision, windows map[int64]int, existing []model.BudgetDecision) []model.BudgetDecision {
	if len(windows) == 0 {
		return existing
	}
	filtered := make([]model.BudgetDecision, 0, len(existing))
	for _, decision := range existing {
		start, affected := windows[decision.WindowStartMS]
		if affected && decision.EventSeq >= start {
			continue
		}
		filtered = append(filtered, decision)
	}

	for windowStart, startSeq := range windows {
		ledger := budget.NewLedger(p)
		var priorSeq int
		for seq := range baseline.BudgetAfter {
			if seq < startSeq && seq > priorSeq {
				priorSeq = seq
			}
		}
		if priorSeq > 0 {
			ledger.AdoptWindow(baseline.BudgetAfter[priorSeq])
		}
		for _, event := range p.Events {
			if event.Kind != model.EventRootSettle {
				continue
			}
			if budgetWindowStart(p, event.TimestampMS) != windowStart || event.Seq < startSeq {
				continue
			}
			decision, ok := tails[event.Settle.TraceID]
			if !ok || !decision.Keep {
				continue
			}
			info := collected[event.Settle.TraceID]
			counts, total := spanCounts(info)
			budgetDecision := ledger.Decide(event.Seq, event.TimestampMS, event.Settle.TraceID, event.Settle.ServiceID, counts, total, decision.Forced)
			filtered = append(filtered, budgetDecision)
		}
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].EventSeq < filtered[j].EventSeq })
	return filtered
}

func recomputeAllBudget(p *model.Project, collected map[string]*collectedTrace, tails map[string]model.TailDecision) []model.BudgetDecision {
	ledger := budget.NewLedger(p)
	out := []model.BudgetDecision{}
	for _, event := range p.Events {
		if event.Kind != model.EventRootSettle {
			continue
		}
		decision := tails[event.Settle.TraceID]
		if !decision.Keep {
			continue
		}
		info := collected[event.Settle.TraceID]
		counts, total := spanCounts(info)
		out = append(out, ledger.Decide(event.Seq, event.TimestampMS, event.Settle.TraceID, event.Settle.ServiceID, counts, total, decision.Forced))
	}
	return out
}

func spanCounts(info *collectedTrace) (map[string]int, int) {
	counts := map[string]int{}
	if info == nil {
		return counts, 0
	}
	spans := uniqueSpans(info.Spans)
	for _, sp := range spans {
		counts[sp.ServiceID]++
	}
	return counts, len(spans)
}

func rebuildSlices(report *model.Report, heads map[string]model.HeadDecision, tails map[string]model.TailDecision) {
	report.Head = report.Head[:0]
	for _, decision := range heads {
		report.Head = append(report.Head, decision)
	}
	report.Tail = report.Tail[:0]
	for _, decision := range tails {
		report.Tail = append(report.Tail, decision)
	}
	_ = SortReport(report)
}

func rebuildRetained(report *model.Report, collected map[string]*collectedTrace) {
	report.RetainedSpans = map[string][]string{}
	for _, decision := range report.Budget {
		if !decision.Accepted {
			continue
		}
		info := collected[decision.TraceID]
		if info == nil {
			continue
		}
		_, total := spanCounts(info)
		ids := make([]string, 0, total)
		for _, sp := range uniqueSpans(info.Spans) {
			ids = append(ids, sp.SpanID)
		}
		sort.Strings(ids)
		report.RetainedSpans[decision.TraceID] = ids
	}
}

func updateEventViews(report *model.Report, p *model.Project, heads map[string]model.HeadDecision, tails map[string]model.TailDecision, budgets []model.BudgetDecision) {
	budgetBySeq := map[int]model.BudgetDecision{}
	for _, decision := range budgets {
		budgetBySeq[decision.EventSeq] = decision
	}
	for i := range report.Events {
		view := &report.Events[i]
		switch view.Kind {
		case model.EventRequestEnter:
			var requestID, traceID, edgeID string
			for _, event := range p.Events {
				if event.Seq == view.Seq && event.Enter != nil {
					requestID, traceID, edgeID = event.Enter.RequestID, event.Enter.TraceID, event.Enter.EdgeID
				}
			}
			if decision, ok := heads[requestID]; ok {
				d := decision
				view.Head = &d
				edge := findEdge(p, edgeID)
				view.Label = fmt.Sprintf("enter request=%s trace=%s edge=%s keep=%t", requestID, traceID, edgeID, decision.Keep)
				_ = edge
			}
		case model.EventRootSettle:
			var traceID string
			for _, event := range p.Events {
				if event.Seq == view.Seq && event.Settle != nil {
					traceID = event.Settle.TraceID
				}
			}
			if decision, ok := tails[traceID]; ok {
				d := decision
				view.Tail = &d
				if budgetDecision, ok := budgetBySeq[view.Seq]; ok {
					bd := budgetDecision
					view.Budget = &bd
					view.Label = fmt.Sprintf("settle trace=%s tail=%s accepted=%t", traceID, decision.DecisionType, budgetDecision.Accepted)
				} else {
					view.Budget = nil
					view.Label = fmt.Sprintf("settle trace=%s tail=%s", traceID, decision.DecisionType)
				}
			}
		}
	}
}

func findEdge(p *model.Project, id string) model.Edge {
	for _, edge := range p.Edges {
		if edge.ID == id {
			return edge
		}
	}
	return model.Edge{}
}

func findHead(heads []model.HeadDecision, requestID string) *model.HeadDecision {
	for i := range heads {
		if heads[i].RequestID == requestID {
			return &heads[i]
		}
	}
	return nil
}

func (c *Cache) reusedNodes(invalidated map[string]model.InvalidationNode) []string {
	reused := map[string]struct{}{}
	for _, decision := range c.Base.Head {
		id := "head:" + decision.RequestID
		if _, bad := invalidated[id]; !bad {
			reused[id] = struct{}{}
		}
	}
	for _, decision := range c.Base.Tail {
		id := "tail:" + decision.TraceID
		if _, bad := invalidated[id]; !bad {
			reused[id] = struct{}{}
		}
	}
	for traceID := range c.Base.Merge {
		id := "merge:" + traceID
		if _, bad := invalidated[id]; !bad {
			reused[id] = struct{}{}
		}
	}
	for _, decision := range c.Base.Budget {
		id := fmt.Sprintf("budget:%d", decision.EventSeq)
		if _, bad := invalidated[id]; !bad {
			reused[id] = struct{}{}
		}
	}
	return sortedStringSet(reused)
}

func sortedStringSet(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func appendRange(ranges []model.SeqRange, seq int) []model.SeqRange {
	return append(ranges, model.SeqRange{FromSeq: seq, ToSeq: seq})
}

func compactRanges(ranges []model.SeqRange) []model.SeqRange {
	if len(ranges) == 0 {
		return []model.SeqRange{}
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].FromSeq < ranges[j].FromSeq })
	out := []model.SeqRange{ranges[0]}
	for _, r := range ranges[1:] {
		last := &out[len(out)-1]
		if r.FromSeq <= last.ToSeq+1 {
			if r.ToSeq > last.ToSeq {
				last.ToSeq = r.ToSeq
			}
		} else {
			out = append(out, r)
		}
	}
	return out
}

func ReportsSemanticEqual(a, b *model.Report) bool {
	if len(a.Head) != len(b.Head) || len(a.Tail) != len(b.Tail) || len(a.Budget) != len(b.Budget) {
		return false
	}
	return reflect.DeepEqual(a.Head, b.Head) &&
		reflect.DeepEqual(a.Tail, b.Tail) &&
		reflect.DeepEqual(a.Budget, b.Budget) &&
		reflect.DeepEqual(a.Merge, b.Merge) &&
		reflect.DeepEqual(a.RetainedSpans, b.RetainedSpans)
}
