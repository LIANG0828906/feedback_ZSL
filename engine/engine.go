// Package engine performs deterministic per-tick collector simulation.
package engine

import (
	"context"
	"sort"
	"time"

	"github.com/liang/collector-backpressure/bpderr"
	"github.com/liang/collector-backpressure/model"
	"github.com/liang/collector-backpressure/planner"
	"github.com/liang/collector-backpressure/schema"
)

const (
	RuleBufferBytes       = "R_AGENT_BUFFER_BYTES"
	RuleFIFOBlocked       = "R_AGENT_FIFO_HEAD_BLOCKED"
	RuleBackoff           = "R_AGENT_BACKOFF_SCALE"
	RuleQueueLimit        = "R_AGENT_PENDING_QUEUE_LIMIT"
	RuleBufferExhausted   = "R_BUFFER_EXHAUST_DURATION"
	RulePartitionPressure = "R_PARTITION_BACKLOG_WATERMARK"
	RuleWatermarkLag      = "R_CONSUMER_WATERMARK_LAG"
	RulePartitionSuspend  = "R_PARTITION_PRESSURE_SUSPEND"

	CodePartitionDrop bpderr.Code = "E_PARTITION_BACKPRESSURE_DROP"
	CodeBufferSlow    bpderr.Code = "E_BUFFER_EXHAUSTED_SLOWDOWN"
	CodeQueueDrop     bpderr.Code = "E_QUEUE_BUDGET_DROP"
)

type Runner struct {
	budget    Budget
	scheduler *Scheduler
	sink      CheckpointSink
	tickDelay time.Duration
}

type Option func(*Runner)

func WithSink(sink CheckpointSink) Option {
	return func(r *Runner) { r.sink = sink }
}

func WithTickDelay(delay time.Duration) Option {
	return func(r *Runner) { r.tickDelay = delay }
}

func NewRunner(budget Budget, opts ...Option) (*Runner, error) {
	scheduler, err := NewScheduler(budget)
	if err != nil {
		return nil, err
	}
	r := &Runner{budget: budget, scheduler: scheduler}
	for _, opt := range opts {
		opt(r)
	}
	return r, nil
}

type InflightChunk struct {
	ChunkID     string `json:"chunk_id"`
	BatchID     string `json:"batch_id"`
	PartitionID string `json:"partition_id"`
	Bytes       int64  `json:"bytes"`
	EndOffset   int64  `json:"end_offset"`
}

type Segment struct {
	Tick     int      `json:"tick"`
	Bytes    int64    `json:"bytes"`
	ChunkIDs []string `json:"chunk_ids"`
}

type AgentSnapshot struct {
	AgentID            string              `json:"agent_id"`
	InflightBytes      int64               `json:"inflight_bytes"`
	Inflight           []InflightChunk     `json:"inflight"`
	Pending            []model.PendingItem `json:"pending"`
	BackoffUntilTick   int                 `json:"backoff_until_tick"`
	ExhaustedSinceTick *int                `json:"exhausted_since_tick,omitempty"`
}

type PartitionSnapshot struct {
	PartitionID       string                `json:"partition_id"`
	Segments          []Segment             `json:"segments"`
	WrittenBytes      int64                 `json:"written_bytes"`
	AckedBytes        int64                 `json:"acked_bytes"`
	DeclaredWatermark *int64                `json:"declared_watermark,omitempty"`
	WatermarkSource   model.WatermarkSource `json:"watermark_source"`
}

type Snapshot struct {
	Tick       int                 `json:"tick"`
	Agents     []AgentSnapshot     `json:"agents"`
	Partitions []PartitionSnapshot `json:"partitions"`
}

type CheckpointSink interface {
	SaveSnapshot(context.Context, *model.Project, Snapshot) error
}

type Simulation struct {
	Result    model.Result
	Snapshots []Snapshot
}

type agentRuntime struct {
	spec           model.Agent
	inflight       []InflightChunk
	pending        []model.PendingItem
	backoffUntil   int
	exhaustedSince *int
	elapsedNS      int64
	workerIndex    int
}

func (a *agentRuntime) inflightBytes() int64 {
	var total int64
	for _, chunk := range a.inflight {
		total += chunk.Bytes
	}
	return total
}

type partitionRuntime struct {
	spec        model.Partition
	segments    []Segment
	written     int64
	acked       int64
	declaredWM  *int64
	source      model.WatermarkSource
	lastElapsed int64
}

type agentWorkItem struct {
	rt       *agentRuntime
	arrivals []model.Chunk
}

type agentJudgeOutput struct {
	report  model.AgentReport
	drops   []model.DropRecord
	signals []model.BackpressureSignal
}

func sortedAgentIDs(p *model.Project) []string {
	ids := make([]string, 0, len(p.Agents))
	for _, a := range p.Agents {
		ids = append(ids, a.ID)
	}
	sort.Strings(ids)
	return ids
}

func sortedPartitionIDs(p *model.Project) []string {
	ids := make([]string, 0, len(p.Partitions))
	for _, d := range p.Partitions {
		ids = append(ids, d.ID)
	}
	sort.Slice(ids, func(i, j int) bool {
		pi, pj := p.Partitions[i], p.Partitions[j]
		if pi.Index != pj.Index {
			return pi.Index < pj.Index
		}
		return pi.ID < pj.ID
	})
	return ids
}

func (r *Runner) Run(ctx context.Context, p *model.Project) (*Simulation, error) {
	return r.RunFromSnapshot(ctx, p, nil)
}

func (r *Runner) RunFromSnapshot(ctx context.Context, p *model.Project, snap *Snapshot) (*Simulation, error) {
	if err := schema.ValidateProject(p); err != nil {
		return nil, err
	}
	chunks, err := planner.BuildChunks(p)
	if err != nil {
		return nil, err
	}
	chunkByID := make(map[string]model.Chunk, len(chunks))
	for _, chunk := range chunks {
		chunkByID[chunk.ChunkID] = chunk
	}

	agentIDs := sortedAgentIDs(p)
	partitionIDs := sortedPartitionIDs(p)
	agents := map[string]*agentRuntime{}
	partitions := map[string]*partitionRuntime{}
	for _, a := range p.Agents {
		queueLimit := a.PendingQueueLimit
		if queueLimit == 0 {
			queueLimit = r.budget.PerAgentQueue
		}
		if queueLimit > r.budget.PerAgentQueue {
			queueLimit = r.budget.PerAgentQueue
		}
		if queueLimit < 1 || queueLimit > 100000 {
			return nil, bpderr.Newf(bpderr.EBudgetOutOfRange, "agents.pending_queue_limit", "agent %s queue limit %d outside [1,100000]", a.ID, queueLimit).
				WithContext("agent_id", a.ID)
		}
		a.PendingQueueLimit = queueLimit
		if a.OverflowPolicy == "" {
			a.OverflowPolicy = model.OverflowDropNewest
		}
		agents[a.ID] = &agentRuntime{spec: a, backoffUntil: -1}
	}
	for _, d := range p.Partitions {
		partitions[d.ID] = &partitionRuntime{spec: d, declaredWM: d.ConsumerWatermark}
	}

	startTick := 0
	if snap != nil {
		startTick = snap.Tick + 1
		if err := restoreSnapshot(snap, agents, partitions); err != nil {
			return nil, err
		}
	}

	arrivals := map[string]map[int][]model.Chunk{}
	for _, id := range agentIDs {
		arrivals[id] = map[int][]model.Chunk{}
	}
	for _, chunk := range chunks {
		arrivals[chunk.AgentID][chunk.ArrivalTick] = append(arrivals[chunk.AgentID][chunk.ArrivalTick], chunk)
	}
	updates := map[int][]model.WatermarkUpdate{}
	for _, update := range p.WatermarkUpdates {
		updates[update.Tick] = append(updates[update.Tick], update)
	}

	sim := &Simulation{Result: model.Result{
		ProjectID: p.ID, ProjectVersion: p.Version, SchemaVersion: p.SchemaVersion,
		Chunks: chunks, ChunkByID: chunkByID,
	}}

	for tick := startTick; tick < p.Ticks; tick++ {
		if r.tickDelay > 0 {
			select {
			case <-ctx.Done():
				bizErr := bpderr.Wrap(bpderr.EInterrupted, "context", ctx.Err(), "simulation interrupted during tick delay")
				return sim, bizErr.WithContext("last_complete_tick", tick-1)
			case <-time.After(r.tickDelay):
			}
		}
		if err := ctx.Err(); err != nil {
			bizErr := bpderr.Wrap(bpderr.EInterrupted, "context", err, "simulation interrupted at complete tick boundary")
			return sim, bizErr.WithContext("last_complete_tick", tick-1)
		}
		report, snapshot, err := r.step(ctx, p, tick, agentIDs, partitionIDs, agents, partitions, arrivals, updates, chunkByID)
		if err != nil {
			return sim, err
		}
		sim.Result.Ticks = append(sim.Result.Ticks, *report)
		sim.Snapshots = append(sim.Snapshots, *snapshot)
		if r.sink != nil {
			if err := r.sink.SaveSnapshot(ctx, p, *snapshot); err != nil {
				return sim, err
			}
		}
	}
	return sim, nil
}

func restoreSnapshot(snap *Snapshot, agents map[string]*agentRuntime, partitions map[string]*partitionRuntime) error {
	for _, as := range snap.Agents {
		rt, ok := agents[as.AgentID]
		if !ok {
			return bpderr.New(bpderr.ECheckpointCorrupt, "snapshot.agents", "checkpoint contains unknown agent").WithContext("agent_id", as.AgentID)
		}
		rt.inflight = append([]InflightChunk(nil), as.Inflight...)
		rt.pending = append([]model.PendingItem(nil), as.Pending...)
		rt.backoffUntil = as.BackoffUntilTick
		rt.exhaustedSince = as.ExhaustedSinceTick
		if rt.inflightBytes() != as.InflightBytes {
			return bpderr.New(bpderr.ECheckpointCorrupt, "snapshot.agents.inflight_bytes", "checkpoint inflight byte total does not match chunks").
				WithContext("agent_id", as.AgentID).WithContext("header", as.InflightBytes).WithContext("recomputed", rt.inflightBytes())
		}
	}
	for _, ps := range snap.Partitions {
		rt, ok := partitions[ps.PartitionID]
		if !ok {
			return bpderr.New(bpderr.ECheckpointCorrupt, "snapshot.partitions", "checkpoint contains unknown partition").WithContext("partition_id", ps.PartitionID)
		}
		rt.segments = append([]Segment(nil), ps.Segments...)
		rt.written = ps.WrittenBytes
		rt.acked = ps.AckedBytes
		rt.declaredWM = ps.DeclaredWatermark
		rt.source = ps.WatermarkSource
	}
	return nil
}

func (r *Runner) step(
	ctx context.Context,
	p *model.Project,
	tick int,
	agentIDs, partitionIDs []string,
	agents map[string]*agentRuntime,
	partitions map[string]*partitionRuntime,
	arrivals map[string]map[int][]model.Chunk,
	updates map[int][]model.WatermarkUpdate,
	chunkByID map[string]model.Chunk,
) (*model.TickReport, *Snapshot, error) {
	tickReport := &model.TickReport{Tick: tick}

	pItems := make([]*partitionRuntime, 0, len(partitionIDs))
	for _, id := range partitionIDs {
		pItems = append(pItems, partitions[id])
	}
	_, err := Map(r.scheduler, pItems, func(_ int, prt *partitionRuntime, worker int) (struct{}, error) {
		start := time.Now()
		prt.advanceAcks(tick)
		prt.lastElapsed = time.Since(start).Nanoseconds()
		_ = worker
		return struct{}{}, nil
	})
	if err != nil {
		return nil, nil, err
	}

	for _, id := range partitionIDs {
		prt := partitions[id]
		if prt.declaredWM == nil {
			continue
		}
		if *prt.declaredWM > prt.acked {
			return nil, nil, bpderr.New(bpderr.EWatermarkAheadAck, "partitions.consumer_watermark_bytes", "declared consumer watermark is ahead of acknowledged position").
				WithContext("tick", tick).WithContext("partition_id", id).
				WithContext("watermark", *prt.declaredWM).WithContext("acked", prt.acked)
		}
		if prt.spec.StrictStaleWatermark && *prt.declaredWM < prt.acked {
			return nil, nil, bpderr.New(bpderr.EWatermarkBeforeAck, "partitions.consumer_watermark_bytes", "strict watermark is earlier than acknowledged position").
				WithContext("tick", tick).WithContext("partition_id", id).
				WithContext("watermark", *prt.declaredWM).WithContext("acked", prt.acked)
		}
	}

	for _, update := range updates[tick] {
		prt := partitions[update.PartitionID]
		value := update.Watermark
		if value > prt.acked {
			return nil, nil, bpderr.New(bpderr.EWatermarkAheadAck, "watermark_updates.watermark_bytes", "declared consumer watermark is ahead of acknowledged position").
				WithContext("tick", tick).WithContext("partition_id", update.PartitionID).
				WithContext("watermark", value).WithContext("acked", prt.acked)
		}
		if prt.spec.StrictStaleWatermark && value < prt.acked {
			return nil, nil, bpderr.New(bpderr.EWatermarkBeforeAck, "watermark_updates.watermark_bytes", "strict watermark is earlier than acknowledged position").
				WithContext("tick", tick).WithContext("partition_id", update.PartitionID).
				WithContext("watermark", value).WithContext("acked", prt.acked)
		}
		prt.declaredWM = &value
		prt.source = model.WatermarkUpdated
	}

	ackedByPartition := map[string][]string{}
	for _, agentID := range agentIDs {
		art := agents[agentID]
		kept := art.inflight[:0]
		for _, inflight := range art.inflight {
			prt := partitions[inflight.PartitionID]
			if inflight.EndOffset <= prt.acked {
				ackedByPartition[inflight.PartitionID] = append(ackedByPartition[inflight.PartitionID], inflight.ChunkID)
			} else {
				kept = append(kept, inflight)
			}
		}
		art.inflight = kept
	}

	startPressure := map[string]bool{}
	for _, id := range partitionIDs {
		prt := partitions[id]
		backlog := prt.written - prt.acked
		startPressure[id] = backlog >= prt.spec.BacklogLimitBytes
	}

	aItems := make([]*agentWorkItem, 0, len(agentIDs))
	for _, id := range agentIDs {
		aItems = append(aItems, &agentWorkItem{rt: agents[id], arrivals: arrivals[id][tick]})
	}
	outputs, err := Map(r.scheduler, aItems, func(index int, item *agentWorkItem, worker int) (agentJudgeOutput, error) {
		return judgeAgent(tick, agentIDs[index], item.rt, item.arrivals, startPressure, r.budget.PerAgentQueue, worker)
	})
	if err != nil {
		return nil, nil, err
	}

	sentBatchThisTick := map[string]bool{}
	for i, output := range outputs {
		agentID := agentIDs[i]
		tickReport.Agents = append(tickReport.Agents, output.report)
		tickReport.Drops = append(tickReport.Drops, output.drops...)
		tickReport.Signals = append(tickReport.Signals, output.signals...)
		for _, chunkID := range output.report.SentChunks {
			chunk := chunkByID[chunkID]
			sentBatchThisTick[chunk.BatchID] = true
			prt := partitions[chunk.PartitionID]
			prt.written += chunk.Bytes
			prt.segments = append(prt.segments, Segment{Tick: tick, Bytes: chunk.Bytes, ChunkIDs: []string{chunkID}})
			for j := range agents[agentID].inflight {
				if agents[agentID].inflight[j].ChunkID == chunkID {
					agents[agentID].inflight[j].EndOffset = prt.written
					break
				}
			}
		}
	}

	pReports, err := Map(r.scheduler, pItems, func(index int, _ *partitionRuntime, worker int) (model.PartitionReport, error) {
		prt := pItems[index]
		start := time.Now()
		report := prt.report(tick, ackedByPartition[partitionIDs[index]])
		report.WorkerIndex = worker
		report.ElapsedNanoseconds = prt.lastElapsed + time.Since(start).Nanoseconds()
		return report, nil
	})
	if err != nil {
		return nil, nil, err
	}
	tickReport.Partitions = pReports

	for _, report := range pReports {
		if !report.Pressure {
			continue
		}
		drop, ok := dropNewestPendingForPartition(tick, report.PartitionID, report.TriggeredRule, partitions[report.PartitionID].spec.BacklogLimitBytes, agentIDs, agents, sentBatchThisTick)
		if !ok {
			tickReport.Signals = append(tickReport.Signals, model.BackpressureSignal{
				Tick: tick, SourceType: "partition", SourceID: report.PartitionID,
				RuleID: RulePartitionSuspend, Code: string(bpderr.EAgentQueueFull),
				Action: model.ActionPending, TargetPartitionID: report.PartitionID,
				TargetAgentIDs: agentIDsForPartition(chunkByID, report.PartitionID, p.Agents),
			})
			continue
		}
		tickReport.Drops = append(tickReport.Drops, drop)
		tickReport.Signals = append(tickReport.Signals, model.BackpressureSignal{
			Tick: tick, SourceType: "partition", SourceID: report.PartitionID,
			RuleID: report.TriggeredRule, Code: string(CodePartitionDrop), Action: model.ActionDrop,
			TargetPartitionID: report.PartitionID, TargetAgentIDs: []string{drop.AgentID},
		})
		if idx := sort.SearchStrings(agentIDs, drop.AgentID); idx < len(agentIDs) && agentIDs[idx] == drop.AgentID {
			outputs[idx].report.DroppedChunks = append(outputs[idx].report.DroppedChunks, drop.ChunkIDs...)
		}
	}

	pressureNow := map[string]bool{}
	for _, report := range pReports {
		pressureNow[report.PartitionID] = report.Pressure
	}
	for i, agentID := range agentIDs {
		rt := agents[agentID]
		rt.labelPending(tick, pressureNow)
		outputs[i].report.Pending = append([]model.PendingItem(nil), rt.pending...)
		outputs[i].report.InflightBytesEnd = rt.inflightBytes()
		outputs[i].report.BackoffUntilTick = rt.backoffUntil
		outputs[i].report.ExhaustedSinceTick = rt.exhaustedSince
		tickReport.Agents[i] = outputs[i].report
	}

	sortDropsAndSignals(tickReport)
	tickReport.ObservedCodes = collectCodes(tickReport)

	snapshot := buildSnapshot(tick, agentIDs, partitionIDs, agents, partitions)
	return tickReport, snapshot, nil
}
