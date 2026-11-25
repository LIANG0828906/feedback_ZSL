package engine

import (
	"sort"
	"strconv"
	"time"

	"github.com/liang/collector-backpressure/bpderr"
	"github.com/liang/collector-backpressure/model"
)

func judgeAgent(
	tick int,
	agentID string,
	rt *agentRuntime,
	arrivals []model.Chunk,
	pressure map[string]bool,
	queueBudget int,
	workerIndex int,
) (agentJudgeOutput, error) {
	start := time.Now()
	out := agentJudgeOutput{}
	report := model.AgentReport{
		AgentID: agentID, BufferMaxBytes: rt.spec.BufferMaxBytes,
		InflightBytesStart: rt.inflightBytes(), WorkerIndex: workerIndex,
	}

	batchOrder := []string{}
	byBatch := map[string][]model.Chunk{}
	for _, chunk := range arrivals {
		if _, ok := byBatch[chunk.BatchID]; !ok {
			batchOrder = append(batchOrder, chunk.BatchID)
		}
		byBatch[chunk.BatchID] = append(byBatch[chunk.BatchID], chunk)
		report.AcceptedChunks = append(report.AcceptedChunks, chunk.ChunkID)
	}

	limit := rt.spec.PendingQueueLimit
	if limit <= 0 {
		limit = queueBudget
	}
	for _, batchID := range batchOrder {
		chunks := byBatch[batchID]
		if len(rt.pending)+len(chunks) > limit {
			ids := make([]string, 0, len(chunks))
			for _, chunk := range chunks {
				ids = append(ids, chunk.ChunkID)
			}
			if rt.spec.OverflowPolicy == model.OverflowError || rt.spec.OverflowPolicy == model.OverflowSuspend {
				err := bpderr.New(bpderr.EAgentQueueFull, "agents.pending_queue", "agent pending queue budget exhausted").
					WithContext("tick", tick).WithContext("agent_id", agentID).
					WithContext("batch_id", batchID).WithContext("queue_limit", limit).
					WithContext("required_slots", len(chunks)).WithContext("pending", len(rt.pending))
				return out, err
			}
			report.DroppedChunks = append(report.DroppedChunks, ids...)
			report.Codes = append(report.Codes, string(bpderr.EAgentQueueFull))
			out.drops = append(out.drops, model.DropRecord{
				Tick: tick, BatchID: batchID, ChunkIDs: ids, AgentID: agentID,
				PartitionID: chunks[0].PartitionID, TriggerSourceID: agentID,
				TriggerPartition: chunks[0].PartitionID, RuleID: RuleQueueLimit,
				Code:   string(bpderr.EAgentQueueFull),
				Reason: "incoming split batch does not fit remaining per-agent queue budget",
			})
			continue
		}
		for _, chunk := range chunks {
			rt.pending = append(rt.pending, model.PendingItem{
				ChunkID: chunk.ChunkID, BatchID: chunk.BatchID, PartitionID: chunk.PartitionID,
				Bytes: chunk.Bytes, Position: len(rt.pending) + 1, SinceTick: tick,
			})
		}
	}

	for len(rt.pending) > 0 {
		head := rt.pending[0]
		if tick < rt.backoffUntil {
			head.ViolatedRule = RuleBackoff
			rt.pending[0] = head
			break
		}
		if pressure[head.PartitionID] {
			head.ViolatedRule = RulePartitionPressure
			rt.pending[0] = head
			break
		}
		if rt.inflightBytes()+head.Bytes > rt.spec.BufferMaxBytes {
			head.ViolatedRule = RuleBufferBytes
			rt.pending[0] = head
			break
		}
		rt.pending = rt.pending[1:]
		rt.inflight = append(rt.inflight, InflightChunk{
			ChunkID: head.ChunkID, BatchID: head.BatchID, PartitionID: head.PartitionID, Bytes: head.Bytes,
		})
		report.SentChunks = append(report.SentChunks, head.ChunkID)
	}

	rt.labelPending(tick, pressure)

	headBlocked := len(rt.pending) > 0 && rt.pending[0].ViolatedRule == RuleBufferBytes
	if headBlocked {
		if rt.exhaustedSince == nil {
			since := tick
			rt.exhaustedSince = &since
		}
		duration := tick - *rt.exhaustedSince + 1
		if rt.spec.BufferExhaustedLimitTicks > 0 && duration >= rt.spec.BufferExhaustedLimitTicks {
			slowTicks := rt.spec.BackoffTicks
			if slowTicks > 0 {
				rt.backoffUntil = tick + slowTicks + 1
				report.Codes = append(report.Codes, string(CodeBufferSlow))
				out.signals = append(out.signals, model.BackpressureSignal{
					Tick: tick, SourceType: "agent", SourceID: agentID,
					RuleID: RuleBufferExhausted, Code: string(CodeBufferSlow), Action: model.ActionSlow,
					TargetAgentIDs: []string{agentID}, UntilTick: rt.backoffUntil, SlowTicks: slowTicks,
				})
				rt.exhaustedSince = nil
				rt.labelPending(tick, pressure)
			}
		}
	} else {
		rt.exhaustedSince = nil
	}

	report.Pending = append([]model.PendingItem(nil), rt.pending...)
	report.InflightBytesEnd = rt.inflightBytes()
	report.BackoffUntilTick = rt.backoffUntil
	report.ExhaustedSinceTick = rt.exhaustedSince
	sort.Strings(report.Codes)
	rt.elapsedNS = time.Since(start).Nanoseconds()
	report.ElapsedNanoseconds = rt.elapsedNS
	out.report = report
	return out, nil
}

func (rt *agentRuntime) labelPending(tick int, pressure map[string]bool) {
	for i := range rt.pending {
		rt.pending[i].Position = i + 1
		switch {
		case tick < rt.backoffUntil:
			rt.pending[i].ViolatedRule = RuleBackoff
		case i == 0 && pressure[rt.pending[i].PartitionID]:
			rt.pending[i].ViolatedRule = RulePartitionPressure
		case i == 0 && rt.inflightBytes()+rt.pending[i].Bytes > rt.spec.BufferMaxBytes:
			rt.pending[i].ViolatedRule = RuleBufferBytes
		case i == 0:
			rt.pending[i].ViolatedRule = ""
		default:
			if rt.pending[0].ViolatedRule != "" {
				rt.pending[i].ViolatedRule = RuleFIFOBlocked
			} else {
				rt.pending[i].ViolatedRule = ""
			}
		}
	}
}

func (prt *partitionRuntime) advanceAcks(tick int) {
	var eligibleWritten int64
	for _, segment := range prt.segments {
		if segment.Tick+prt.spec.AckDelayTicks <= tick {
			eligibleWritten += segment.Bytes
		}
	}
	target := prt.acked + prt.spec.RateBytesPerTick
	if eligibleWritten < target {
		target = eligibleWritten
	}
	if target > prt.acked {
		prt.acked = target
	}
}

func (prt *partitionRuntime) report(tick int, ackedChunks []string) model.PartitionReport {
	backlog := prt.written - prt.acked
	effectiveWM := prt.acked
	source := model.WatermarkDerived
	if prt.declaredWM != nil {
		effectiveWM = *prt.declaredWM
		source = prt.source
		if source == "" {
			source = model.WatermarkDeclared
		}
	}
	wmBacklog := prt.written - effectiveWM
	if wmBacklog < 0 {
		wmBacklog = 0
	}
	report := model.PartitionReport{
		PartitionID: prt.spec.ID, WrittenBytes: prt.written, AckedBytes: prt.acked,
		BacklogBytes: backlog, BacklogLimitBytes: prt.spec.BacklogLimitBytes,
		EffectiveWatermark: effectiveWM, WatermarkSource: source,
		WatermarkLagBytes:     maxInt64(0, prt.acked-effectiveWM),
		WatermarkBacklogBytes: wmBacklog, AckedChunks: append([]string(nil), ackedChunks...),
	}
	if backlog >= prt.spec.BacklogLimitBytes {
		report.Pressure = true
		report.TriggeredRule = RulePartitionPressure
	} else if wmBacklog >= prt.spec.BacklogLimitBytes && prt.declaredWM != nil {
		report.Pressure = true
		report.TriggeredRule = RuleWatermarkLag
	}
	_ = tick
	return report
}

func dropNewestPendingForPartition(
	tick int,
	partitionID string,
	ruleID string,
	limit int64,
	agentIDs []string,
	agents map[string]*agentRuntime,
	excludeBatches map[string]bool,
) (model.DropRecord, bool) {
	type candidate struct {
		agentID string
		index   int
		item    model.PendingItem
	}
	var best *candidate
	for _, agentID := range agentIDs {
		rt := agents[agentID]
		for i, item := range rt.pending {
			if item.PartitionID != partitionID || excludeBatches[item.BatchID] {
				continue
			}
			current := candidate{agentID: agentID, index: i, item: item}
			if best == nil ||
				item.SinceTick > best.item.SinceTick ||
				(item.SinceTick == best.item.SinceTick && agentID > best.agentID) ||
				(item.SinceTick == best.item.SinceTick && agentID == best.agentID && i > best.index) {
				best = &current
			}
		}
	}
	if best == nil {
		return model.DropRecord{}, false
	}
	rt := agents[best.agentID]
	batchID := best.item.BatchID
	chunkIDs := []string{}
	kept := rt.pending[:0]
	for _, item := range rt.pending {
		if item.BatchID == batchID {
			chunkIDs = append(chunkIDs, item.ChunkID)
		} else {
			kept = append(kept, item)
		}
	}
	rt.pending = kept
	for i := range rt.pending {
		rt.pending[i].Position = i + 1
	}
	return model.DropRecord{
		Tick: tick, BatchID: batchID, ChunkIDs: chunkIDs, AgentID: best.agentID,
		PartitionID: partitionID, TriggerSourceID: partitionID, TriggerPartition: partitionID,
		RuleID: ruleID, Code: string(CodePartitionDrop),
		Reason: "newest pending batch shed after partition backlog/watermark reached limit " + strconv.FormatInt(limit, 10),
	}, true
}

func agentIDsForPartition(chunkByID map[string]model.Chunk, partitionID string, all []model.Agent) []string {
	seen := map[string]struct{}{}
	for _, chunk := range chunkByID {
		if chunk.PartitionID == partitionID {
			seen[chunk.AgentID] = struct{}{}
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func sortDropsAndSignals(report *model.TickReport) {
	sort.SliceStable(report.Drops, func(i, j int) bool {
		a, b := report.Drops[i], report.Drops[j]
		if a.Tick != b.Tick {
			return a.Tick < b.Tick
		}
		if a.PartitionID != b.PartitionID {
			return a.PartitionID < b.PartitionID
		}
		return a.BatchID < b.BatchID
	})
	sort.SliceStable(report.Signals, func(i, j int) bool {
		a, b := report.Signals[i], report.Signals[j]
		if a.Tick != b.Tick {
			return a.Tick < b.Tick
		}
		if a.SourceID != b.SourceID {
			return a.SourceID < b.SourceID
		}
		return a.RuleID < b.RuleID
	})
}

func collectCodes(report *model.TickReport) []string {
	seen := map[string]struct{}{}
	var codes []string
	add := func(code string) {
		if code == "" {
			return
		}
		if _, ok := seen[code]; ok {
			return
		}
		seen[code] = struct{}{}
		codes = append(codes, code)
	}
	for _, agent := range report.Agents {
		for _, code := range agent.Codes {
			add(code)
		}
	}
	for _, drop := range report.Drops {
		add(drop.Code)
	}
	for _, signal := range report.Signals {
		add(signal.Code)
	}
	sort.Strings(codes)
	return codes
}

func buildSnapshot(tick int, agentIDs, partitionIDs []string, agents map[string]*agentRuntime, partitions map[string]*partitionRuntime) *Snapshot {
	snap := &Snapshot{Tick: tick}
	for _, id := range agentIDs {
		rt := agents[id]
		inflight := append([]InflightChunk(nil), rt.inflight...)
		pending := append([]model.PendingItem(nil), rt.pending...)
		snap.Agents = append(snap.Agents, AgentSnapshot{
			AgentID: id, InflightBytes: rt.inflightBytes(), Inflight: inflight,
			Pending: pending, BackoffUntilTick: rt.backoffUntil, ExhaustedSinceTick: rt.exhaustedSince,
		})
	}
	for _, id := range partitionIDs {
		rt := partitions[id]
		snap.Partitions = append(snap.Partitions, PartitionSnapshot{
			PartitionID: id, Segments: append([]Segment(nil), rt.segments...),
			WrittenBytes: rt.written, AckedBytes: rt.acked,
			DeclaredWatermark: rt.declaredWM, WatermarkSource: rt.source,
		})
	}
	return snap
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
