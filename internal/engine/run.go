package engine

import (
	"fmt"
	"sort"

	"samplingkernel/internal/budget"
	"samplingkernel/internal/merge"
	"samplingkernel/internal/model"
	"samplingkernel/internal/sampling"
	"samplingkernel/internal/tail"
)

type Engine struct {
	Project *model.Project
	edges   map[string]model.Edge
}

func New(p *model.Project) *Engine {
	e := &Engine{Project: p, edges: map[string]model.Edge{}}
	for _, edge := range p.Edges {
		e.edges[edge.ID] = edge
	}
	return e
}

func RunFull(p *model.Project) *model.Report {
	return New(p).RunDetailed().Report
}

type Outcome struct {
	Report      *model.Report
	Ledger      *budget.Ledger
	BudgetAfter map[int]*budget.WindowState
}

type traceState struct {
	head          *model.HeadDecision
	spans         []model.Span
	edgeIDs       map[string]struct{}
	forcedEdgeIDs []string
}

func (e *Engine) Run() *model.Report {
	return e.RunDetailed().Report
}

func (e *Engine) RunDetailed() *Outcome {
	report := &model.Report{
		ProjectID:     e.Project.ProjectID,
		SourceVersion: e.Project.SchemaVersion,
		Merge:         map[string]*model.MergeResult{},
		RetainedSpans: map[string][]string{},
	}
	states := map[string]*traceState{}
	ledger := budget.NewLedger(e.Project)
	budgetAfter := map[int]*budget.WindowState{}

	getState := func(traceID string) *traceState {
		state, ok := states[traceID]
		if !ok {
			state = &traceState{edgeIDs: map[string]struct{}{}}
			states[traceID] = state
		}
		return state
	}
	addEdge := func(state *traceState, edgeID string) {
		if edgeID == "" {
			return
		}
		if _, ok := state.edgeIDs[edgeID]; ok {
			return
		}
		state.edgeIDs[edgeID] = struct{}{}
		if edge, ok := e.edges[edgeID]; ok && edge.ForcedRetention {
			state.forcedEdgeIDs = append(state.forcedEdgeIDs, edgeID)
		}
	}

	for _, event := range e.Project.Events {
		view := model.EventView{Seq: event.Seq, Kind: event.Kind, TimestampMS: event.TimestampMS}
		switch event.Kind {
		case model.EventRequestEnter:
			payload := event.Enter
			edge := e.edges[payload.EdgeID]
			decision := sampling.DecideHead(e.Project.ProjectID, edge, payload.RequestID, payload.TraceID)
			state := getState(payload.TraceID)
			state.head = &decision
			addEdge(state, payload.EdgeID)
			report.Head = append(report.Head, decision)
			view.Head = &decision
			view.Label = fmt.Sprintf("enter request=%s trace=%s edge=%s keep=%t", payload.RequestID, payload.TraceID, payload.EdgeID, decision.Keep)

		case model.EventPropagate:
			payload := event.Propagate
			state := getState(payload.TraceID)
			addEdge(state, payload.EdgeID)
			view.Label = fmt.Sprintf("propagate trace=%s edge=%s", payload.TraceID, payload.EdgeID)

		case model.EventSpanReport:
			sp := event.Span
			state := getState(sp.TraceID)
			state.spans = append(state.spans, *sp)
			addEdge(state, sp.EdgeID)
			merged := merge.MergeTrace(sp.TraceID, state.spans, e.Project.OrphanPolicy)
			report.Merge[sp.TraceID] = merged
			view.Merge = merged
			view.Label = fmt.Sprintf("span report %s trace=%s service=%s", sp.SpanID, sp.TraceID, sp.ServiceID)

		case model.EventRootSettle:
			payload := event.Settle
			state := getState(payload.TraceID)
			uniqueSpans := uniqueSpans(state.spans)
			headKeep := false
			headID := "none"
			if state.head != nil {
				headKeep = state.head.Keep
				headID = state.head.RequestID
			}
			tailDecision := tail.Evaluate(tail.Input{
				TraceID: payload.TraceID, Rules: e.Project.TailRules, Spans: uniqueSpans,
				HeadKeep: headKeep, HeadDecisionID: headID,
				Forced: len(state.forcedEdgeIDs) > 0, ForcedEdgeIDs: state.forcedEdgeIDs,
			})
			report.Tail = append(report.Tail, tailDecision)
			view.Tail = &tailDecision

			if tailDecision.Keep {
				counts := map[string]int{}
				for _, sp := range uniqueSpans {
					counts[sp.ServiceID]++
				}
				budgetDecision := ledger.Decide(event.Seq, event.TimestampMS, payload.TraceID, payload.ServiceID, counts, len(uniqueSpans), tailDecision.Forced)
				report.Budget = append(report.Budget, budgetDecision)
				budgetAfter[event.Seq] = ledger.SnapshotWindow(budgetDecision.WindowStartMS)
				view.Budget = &budgetDecision
				if budgetDecision.Accepted {
					ids := make([]string, 0, len(uniqueSpans))
					for _, sp := range uniqueSpans {
						ids = append(ids, sp.SpanID)
					}
					sort.Strings(ids)
					report.RetainedSpans[payload.TraceID] = ids
				}
				view.Label = fmt.Sprintf("settle trace=%s tail=%s accepted=%t", payload.TraceID, tailDecision.DecisionType, budgetDecision.Accepted)
			} else {
				view.Label = fmt.Sprintf("settle trace=%s tail=%s", payload.TraceID, tailDecision.DecisionType)
			}
		}
		report.Events = append(report.Events, view)
	}

	if err := SortReport(report); err != nil {
		panic(err)
	}
	return &Outcome{Report: report, Ledger: ledger, BudgetAfter: budgetAfter}
}

func uniqueSpans(reports []model.Span) []model.Span {
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
	sort.Slice(out, func(i, j int) bool {
		if out[i].CausalOrder != out[j].CausalOrder {
			return out[i].CausalOrder < out[j].CausalOrder
		}
		return out[i].SpanID < out[j].SpanID
	})
	return out
}

func SortReport(report *model.Report) error {
	sort.Slice(report.Head, func(i, j int) bool { return report.Head[i].RequestID < report.Head[j].RequestID })
	sort.Slice(report.Tail, func(i, j int) bool { return report.Tail[i].TraceID < report.Tail[j].TraceID })
	sort.Slice(report.Budget, func(i, j int) bool { return report.Budget[i].EventSeq < report.Budget[j].EventSeq })
	keys := make([]string, 0, len(report.Merge))
	for key := range report.Merge {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	ordered := map[string]*model.MergeResult{}
	for _, key := range keys {
		ordered[key] = report.Merge[key]
	}
	report.Merge = ordered
	return nil
}
