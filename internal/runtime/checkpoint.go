package runtime

import (
	"context"
	"encoding/json"
	"sort"

	"samplingkernel/internal/budget"
	"samplingkernel/internal/engine"
	"samplingkernel/internal/errs"
	"samplingkernel/internal/model"
	"samplingkernel/internal/sampling"
	"samplingkernel/internal/store"
	"samplingkernel/internal/tail"
)

const CrashAfterCommitCode = "E_CRASH_AFTER_COMMIT"

type RecoveryInfo struct {
	ExpectedVersion       string `json:"expected_version"`
	CheckpointVersion     string `json:"checkpoint_version,omitempty"`
	LoadedSeq             int    `json:"loaded_seq"`
	Action                string `json:"action"`
	Detail                string `json:"detail,omitempty"`
	CompletedInCheckpoint int    `json:"completed_in_checkpoint,omitempty"`
}

type RunResult struct {
	Report   *model.Report
	Recovery RecoveryInfo
}

type checkpointProcessor struct {
	project *model.Project
	store   *store.Store
	sched   *Scheduler
	ledger  *budget.Ledger
	options Options

	heads   map[string]model.HeadDecision
	edgeIDs map[string]map[string]struct{}
	records map[string]store.TraceRecord
}

func ProjectVersion(p *model.Project) string {
	if p.SchemaVersion != "" {
		return p.SchemaVersion
	}
	return "sampling.project/canonical-v1"
}

func RunCheckpointed(ctx context.Context, p *model.Project, st *store.Store, options Options, crashAfterSeq int) (*RunResult, error) {
	options, err := ValidateOptions(options)
	if err != nil {
		return nil, err
	}
	version := ProjectVersion(p)
	recovery := RecoveryInfo{ExpectedVersion: version, Action: "full_recompute"}
	checkpoint, err := st.Load(ctx, version)
	if err != nil {
		if errs.CodeOf(err) == errs.CheckpointVersionMismatch {
			if checkpoint != nil {
				recovery.CheckpointVersion = checkpoint.ProjectVersion
				recovery.LoadedSeq = checkpoint.LastSeq
			}
			recovery.Action = "restart_from_start_version_mismatch"
			recovery.Detail = err.Error()
			if resetErr := st.Reset(ctx); resetErr != nil {
				return nil, resetErr
			}
			checkpoint = nil
		} else {
			return nil, err
		}
	}
	if checkpoint != nil {
		recovery.Action = "resume_matching_version"
		recovery.CheckpointVersion = checkpoint.ProjectVersion
		recovery.LoadedSeq = checkpoint.LastSeq
		for _, record := range checkpoint.Traces {
			if record.Status == "complete" {
				recovery.CompletedInCheckpoint++
			}
		}
	}

	sched, err := NewScheduler(options)
	if err != nil {
		return nil, err
	}
	processor := &checkpointProcessor{
		project: p,
		store:   st,
		sched:   sched,
		ledger:  budget.NewLedger(p),
		options: options,
		heads:   map[string]model.HeadDecision{},
		edgeIDs: map[string]map[string]struct{}{},
		records: map[string]store.TraceRecord{},
	}

	for _, event := range p.Events {
		persist := checkpoint == nil || event.Seq > checkpoint.LastSeq
		if err := processor.handle(ctx, event, persist); err != nil {
			return nil, err
		}
		if persist && crashAfterSeq > 0 && event.Seq == crashAfterSeq {
			return nil, errs.New(CrashAfterCommitCode, "simulated process kill immediately after durable checkpoint commit").
				AtPath("$.crash_after_seq").WithDetail("seq=" + itoa(crashAfterSeq))
		}
	}

	return &RunResult{Report: engine.RunFull(p), Recovery: recovery}, nil
}

func (p *checkpointProcessor) handle(ctx context.Context, event model.Event, persist bool) error {
	switch event.Kind {
	case model.EventRequestEnter:
		payload := event.Enter
		edge := p.edge(payload.EdgeID)
		decision := sampling.DecideHead(p.project.ProjectID, edge, payload.RequestID, payload.TraceID)
		p.heads[payload.TraceID] = decision
		p.addEdge(payload.TraceID, payload.EdgeID)
		p.touch(payload.TraceID, event.Seq, "collecting")

	case model.EventPropagate:
		p.addEdge(event.Propagate.TraceID, event.Propagate.EdgeID)
		p.touch(event.Propagate.TraceID, event.Seq, "collecting")

	case model.EventSpanReport:
		span := *event.Span
		if err := p.sched.SubmitSpan(span); err != nil {
			return err
		}
		p.addEdge(span.TraceID, span.EdgeID)
		merged := p.sched.MergeCurrent(span.TraceID, p.project.OrphanPolicy)
		record := p.touch(span.TraceID, event.Seq, "collecting")
		record.ReceivedCount = p.sched.BufferedSpanCount(span.TraceID)
		record.MergedCount = len(merged.OrderedSpanIDs)
		record.ResultJSON = mustJSON(merged)
		p.records[span.TraceID] = record

	case model.EventRootSettle:
		payload := event.Settle
		p.touch(payload.TraceID, event.Seq, "adjudicating")
		spans := p.uniqueBuffered(payload.TraceID)
		head := p.heads[payload.TraceID]
		forcedIDs := p.forcedEdges(payload.TraceID)
		tailDecision := tail.Evaluate(tail.Input{
			TraceID: payload.TraceID, Rules: p.project.TailRules, Spans: spans,
			HeadKeep: head.Keep, HeadDecisionID: head.RequestID,
			Forced: len(forcedIDs) > 0, ForcedEdgeIDs: forcedIDs,
		})
		if tailDecision.Keep {
			counts := map[string]int{}
			for _, span := range spans {
				counts[span.ServiceID]++
			}
			p.ledger.Decide(event.Seq, event.TimestampMS, payload.TraceID, payload.ServiceID, counts, len(spans), tailDecision.Forced)
		}
		merged := p.sched.MergeCurrent(payload.TraceID, p.project.OrphanPolicy)
		record := p.touch(payload.TraceID, event.Seq, "complete")
		record.ReceivedCount = p.sched.BufferedSpanCount(payload.TraceID)
		record.MergedCount = len(merged.OrderedSpanIDs)
		record.ResultJSON = mustJSON(struct {
			*model.MergeResult
			Tail *model.TailDecision `json:"tail_decision"`
		}{MergeResult: merged, Tail: &tailDecision})
		p.records[payload.TraceID] = record
	}

	if persist {
		return p.save(ctx, event.Seq)
	}
	return nil
}

func (p *checkpointProcessor) save(ctx context.Context, seq int) error {
	checkpoint := store.Checkpoint{
		ProjectID:      p.project.ProjectID,
		ProjectVersion: ProjectVersion(p.project),
		LastSeq:        seq,
	}
	ids := make([]string, 0, len(p.records))
	for id := range p.records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		checkpoint.Traces = append(checkpoint.Traces, p.records[id])
	}

	windows := p.ledger.States()
	windowStarts := make([]int64, 0, len(windows))
	for start := range windows {
		windowStarts = append(windowStarts, start)
	}
	sort.Slice(windowStarts, func(i, j int) bool { return windowStarts[i] < windowStarts[j] })
	for _, start := range windowStarts {
		window := windows[start]
		serviceIDs := make([]string, 0, len(window.Services))
		for id := range window.Services {
			serviceIDs = append(serviceIDs, id)
		}
		sort.Strings(serviceIDs)
		for _, serviceID := range serviceIDs {
			state := window.Services[serviceID]
			checkpoint.Services = append(checkpoint.Services, store.ServiceRecord{
				WindowStartMS: start, ServiceID: serviceID,
				LimitSpans: state.LimitSpans, ChargedSpans: state.ChargedSpans, ForcedSpans: state.ForcedSpans,
			})
		}
	}
	return p.store.SaveProgress(ctx, checkpoint)
}

func (p *checkpointProcessor) touch(traceID string, seq int, status string) store.TraceRecord {
	record := p.records[traceID]
	if record.TraceID == "" {
		record.TraceID = traceID
	}
	if status == "complete" || record.Status != "complete" {
		record.Status = status
	}
	record.LastEventSeq = seq
	p.records[traceID] = record
	return record
}

func (p *checkpointProcessor) addEdge(traceID, edgeID string) {
	if edgeID == "" {
		return
	}
	if p.edgeIDs[traceID] == nil {
		p.edgeIDs[traceID] = map[string]struct{}{}
	}
	p.edgeIDs[traceID][edgeID] = struct{}{}
}

func (p *checkpointProcessor) edge(id string) model.Edge {
	for _, edge := range p.project.Edges {
		if edge.ID == id {
			return edge
		}
	}
	return model.Edge{}
}

func (p *checkpointProcessor) forcedEdges(traceID string) []string {
	var out []string
	for edgeID := range p.edgeIDs[traceID] {
		if edge := p.edge(edgeID); edge.ForcedRetention {
			out = append(out, edgeID)
		}
	}
	sort.Strings(out)
	return out
}

func (p *checkpointProcessor) uniqueBuffered(traceID string) []model.Span {
	p.sched.mu.Lock()
	reports := append([]model.Span{}, p.sched.buffers[traceID]...)
	p.sched.mu.Unlock()
	byID := map[string]model.Span{}
	for _, span := range reports {
		if _, ok := byID[span.SpanID]; !ok {
			byID[span.SpanID] = span
		}
	}
	out := make([]model.Span, 0, len(byID))
	for _, span := range byID {
		out = append(out, span)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SpanID < out[j].SpanID })
	return out
}

func mustJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(data)
}
