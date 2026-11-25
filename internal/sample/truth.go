package sample

import (
	"sort"

	"samplingkernel/internal/budget"
	"samplingkernel/internal/engine"
	"samplingkernel/internal/merge"
	"samplingkernel/internal/model"
	"samplingkernel/internal/sampling"
	"samplingkernel/internal/tail"
)

type truthTrace struct {
	head        *model.HeadDecision
	spans       []model.Span
	edges       map[string]bool
	forcedEdges []string
}

func recordTruth(p *model.Project) *model.Report {
	truth := &model.Report{ProjectID: p.ProjectID, Merge: map[string]*model.MergeResult{}, RetainedSpans: map[string][]string{}}
	edgeByID := map[string]model.Edge{}
	for _, edge := range p.Edges {
		edgeByID[edge.ID] = edge
	}
	traces := map[string]*truthTrace{}
	ledger := budget.NewLedger(p)
	get := func(id string) *truthTrace {
		if traces[id] == nil {
			traces[id] = &truthTrace{edges: map[string]bool{}}
		}
		return traces[id]
	}
	useEdge := func(t *truthTrace, id string) {
		if id == "" || t.edges[id] {
			return
		}
		t.edges[id] = true
		if edge := edgeByID[id]; edge.ForcedRetention {
			t.forcedEdges = append(t.forcedEdges, id)
		}
	}

	for _, event := range p.Events {
		switch event.Kind {
		case model.EventRequestEnter:
			payload := event.Enter
			decision := sampling.DecideHead(p.ProjectID, edgeByID[payload.EdgeID], payload.RequestID, payload.TraceID)
			t := get(payload.TraceID)
			t.head = &decision
			useEdge(t, payload.EdgeID)
			truth.Head = append(truth.Head, decision)
		case model.EventPropagate:
			useEdge(get(event.Propagate.TraceID), event.Propagate.EdgeID)
		case model.EventSpanReport:
			t := get(event.Span.TraceID)
			t.spans = append(t.spans, *event.Span)
			useEdge(t, event.Span.EdgeID)
			truth.Merge[event.Span.TraceID] = merge.MergeTrace(event.Span.TraceID, t.spans, p.OrphanPolicy)
		case model.EventRootSettle:
			payload := event.Settle
			t := get(payload.TraceID)
			unique := truthUnique(t.spans)
			headKeep := false
			headRequest := "none"
			if t.head != nil {
				headKeep = t.head.Keep
				headRequest = t.head.RequestID
			}
			tailDecision := tail.Evaluate(tail.Input{
				TraceID: payload.TraceID, Rules: p.TailRules, Spans: unique,
				HeadKeep: headKeep, HeadDecisionID: headRequest,
				Forced: len(t.forcedEdges) > 0, ForcedEdgeIDs: t.forcedEdges,
			})
			truth.Tail = append(truth.Tail, tailDecision)
			if !tailDecision.Keep {
				continue
			}
			counts := map[string]int{}
			for _, sp := range unique {
				counts[sp.ServiceID]++
			}
			budgetDecision := ledger.Decide(event.Seq, event.TimestampMS, payload.TraceID, payload.ServiceID, counts, len(unique), tailDecision.Forced)
			truth.Budget = append(truth.Budget, budgetDecision)
			if budgetDecision.Accepted {
				ids := []string{}
				for _, sp := range unique {
					ids = append(ids, sp.SpanID)
				}
				sort.Strings(ids)
				truth.RetainedSpans[payload.TraceID] = ids
			}
		}
	}
	_ = engine.SortReport(truth)
	return truth
}

func truthUnique(reports []model.Span) []model.Span {
	byID := map[string]model.Span{}
	for _, sp := range reports {
		if _, ok := byID[sp.SpanID]; !ok {
			byID[sp.SpanID] = sp
		}
	}
	out := make([]model.Span, 0, len(byID))
	for _, sp := range byID {
		out = append(out, sp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SpanID < out[j].SpanID })
	return out
}
