// Package incremental caches full runs and recomputes only dependency intervals.
package incremental

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/liang/collector-backpressure/bpderr"
	"github.com/liang/collector-backpressure/engine"
	"github.com/liang/collector-backpressure/model"
	"github.com/liang/collector-backpressure/planner"
)

type Cache struct {
	Budget engine.Budget
	Base   *model.Project
	Full   *engine.Simulation
}

type Report struct {
	Source             string        `json:"source"`
	FromTick           int           `json:"from_tick"`
	ReusedTicks        []int         `json:"reused_ticks"`
	RecomputedTicks    []int         `json:"recomputed_ticks"`
	PropagationClosure []string      `json:"propagation_closure"`
	Result             *model.Result `json:"-"`
}

type EditSpec struct {
	Kind     string
	TargetID string
	Field    string
	NewValue any
	mutate   func(*model.Project) error
}

func (e EditSpec) Source() string {
	return fmt.Sprintf("edit:%s:%s.%s=%v", e.Kind, e.TargetID, e.Field, e.NewValue)
}

func (e EditSpec) Apply(p *model.Project) error {
	if e.mutate == nil {
		return bpderr.New(bpderr.ESchemaInvalid, "edit", "edit has no mutation")
	}
	return e.mutate(p)
}

func NewCache(ctx context.Context, p *model.Project, budget engine.Budget) (*Cache, error) {
	runner, err := engine.NewRunner(budget)
	if err != nil {
		return nil, err
	}
	full, err := runner.Run(ctx, p)
	if err != nil {
		return nil, err
	}
	return &Cache{Budget: budget, Base: p, Full: full}, nil
}

func CloneProject(p *model.Project) (*model.Project, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, bpderr.Wrap(bpderr.ESchemaInvalid, "project", err, "cannot clone project")
	}
	var out model.Project
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, bpderr.Wrap(bpderr.ESchemaInvalid, "project", err, "cannot clone project")
	}
	return &out, nil
}

func EditAgentBuffer(agentID string, bytes int64) EditSpec {
	return EditSpec{Kind: "agent", TargetID: agentID, Field: "buffer_max_bytes", NewValue: bytes, mutate: func(p *model.Project) error {
		for i := range p.Agents {
			if p.Agents[i].ID == agentID {
				p.Agents[i].BufferMaxBytes = bytes
				return nil
			}
		}
		return bpderr.New(bpderr.ESchemaInvalid, "agent_id", "unknown agent in edit").WithContext("agent_id", agentID)
	}}
}

func EditPartitionBacklogLimit(partitionID string, bytes int64) EditSpec {
	return EditSpec{Kind: "partition", TargetID: partitionID, Field: "backlog_limit_bytes", NewValue: bytes, mutate: func(p *model.Project) error {
		for i := range p.Partitions {
			if p.Partitions[i].ID == partitionID {
				p.Partitions[i].BacklogLimitBytes = bytes
				return nil
			}
		}
		return bpderr.New(bpderr.ESchemaInvalid, "partition_id", "unknown partition in edit").WithContext("partition_id", partitionID)
	}}
}

func EditPartitionRate(partitionID string, rateBytesPerTick int64) EditSpec {
	return EditSpec{Kind: "partition", TargetID: partitionID, Field: "rate_bytes_per_tick", NewValue: rateBytesPerTick, mutate: func(p *model.Project) error {
		for i := range p.Partitions {
			if p.Partitions[i].ID == partitionID {
				p.Partitions[i].RateBytesPerTick = rateBytesPerTick
				return nil
			}
		}
		return bpderr.New(bpderr.ESchemaInvalid, "partition_id", "unknown partition in edit").WithContext("partition_id", partitionID)
	}}
}

func EditConsumerWatermark(partitionID string, watermark *int64, strict bool) EditSpec {
	return EditSpec{Kind: "partition", TargetID: partitionID, Field: "consumer_watermark_bytes", NewValue: watermark, mutate: func(p *model.Project) error {
		for i := range p.Partitions {
			if p.Partitions[i].ID == partitionID {
				p.Partitions[i].ConsumerWatermark = watermark
				p.Partitions[i].StrictStaleWatermark = strict
				return nil
			}
		}
		return bpderr.New(bpderr.ESchemaInvalid, "partition_id", "unknown partition in edit").WithContext("partition_id", partitionID)
	}}
}

func EditKeyRule(rule model.KeyRule) EditSpec {
	return EditSpec{Kind: "rule", TargetID: rule.ID, Field: "key_rule", NewValue: rule.PartitionID, mutate: func(p *model.Project) error {
		for i := range p.Rules {
			if p.Rules[i].ID == rule.ID {
				p.Rules[i] = rule
				return nil
			}
		}
		p.Rules = append(p.Rules, rule)
		return nil
	}}
}

func (c *Cache) Apply(ctx context.Context, edit EditSpec) (*model.Result, *Report, error) {
	nextProject, err := CloneProject(c.Base)
	if err != nil {
		return nil, nil, err
	}
	if edit.mutate == nil {
		return nil, nil, bpderr.New(bpderr.ESchemaInvalid, "edit", "edit has no mutation")
	}
	if err := edit.mutate(nextProject); err != nil {
		return nil, nil, err
	}

	oldChunks := c.Full.Result.Chunks
	newChunks, err := planner.BuildChunks(nextProject)
	if err != nil {
		return nil, nil, err
	}

	seedNodes, seedChunks, err := seedDependency(edit, c.Base, oldChunks, newChunks)
	if err != nil {
		return nil, nil, err
	}
	closure := propagationClosure(newChunks, seedNodes, edit.TargetID)

	fromTick := nextProject.Ticks
	for _, chunk := range seedChunks {
		if chunk.ArrivalTick < fromTick {
			fromTick = chunk.ArrivalTick
		}
	}

	report := &Report{Source: edit.Source(), FromTick: fromTick, PropagationClosure: closure}
	if fromTick >= nextProject.Ticks {
		result := c.Full.Result
		result.Ticks = append([]model.TickReport(nil), c.Full.Result.Ticks...)
		for t := range result.Ticks {
			result.Ticks[t].Invalidation = &model.TickInvalidation{Recomputed: false, Sources: []string{"reuse:unchanged-full-cache"}, PropagationClosure: closure}
			report.ReusedTicks = append(report.ReusedTicks, t)
		}
		report.Result = &result
		return &result, report, nil
	}

	var resumeSnapshot *engine.Snapshot
	if fromTick > 0 {
		snapshotCopy := c.Full.Snapshots[fromTick-1]
		snapshotCopy.Agents = append([]engine.AgentSnapshot(nil), snapshotCopy.Agents...)
		snapshotCopy.Partitions = append([]engine.PartitionSnapshot(nil), snapshotCopy.Partitions...)
		if edit.Kind == "partition" && edit.Field == "consumer_watermark_bytes" {
			editedPartitions := map[string]model.Partition{}
			for _, partition := range nextProject.Partitions {
				editedPartitions[partition.ID] = partition
			}
			for i := range snapshotCopy.Partitions {
				if partition, ok := editedPartitions[snapshotCopy.Partitions[i].PartitionID]; ok {
					if partition.ConsumerWatermark == nil {
						snapshotCopy.Partitions[i].DeclaredWatermark = nil
					} else {
						value := *partition.ConsumerWatermark
						snapshotCopy.Partitions[i].DeclaredWatermark = &value
					}
					snapshotCopy.Partitions[i].WatermarkSource = model.WatermarkDeclared
				}
			}
		}
		resumeSnapshot = &snapshotCopy
	}
	runner, err := engine.NewRunner(c.Budget)
	if err != nil {
		return nil, nil, err
	}
	suffix, err := runner.RunFromSnapshot(ctx, nextProject, resumeSnapshot)
	if err != nil {
		return nil, nil, err
	}

	result := model.Result{
		ProjectID: nextProject.ID, ProjectVersion: nextProject.Version, SchemaVersion: nextProject.SchemaVersion,
		Chunks: newChunks, ChunkByID: map[string]model.Chunk{},
	}
	for _, chunk := range newChunks {
		result.ChunkByID[chunk.ChunkID] = chunk
	}
	for t := 0; t < fromTick; t++ {
		tr := c.Full.Result.Ticks[t]
		tr.Agents = append([]model.AgentReport(nil), tr.Agents...)
		tr.Partitions = append([]model.PartitionReport(nil), tr.Partitions...)
		rebaseStaticDeclarations(&tr, nextProject)
		tr.Invalidation = &model.TickInvalidation{
			Recomputed: false, Sources: []string{fmt.Sprintf("reuse:full-cache#tick-%d", t)},
			PropagationClosure: closure,
		}
		result.Ticks = append(result.Ticks, tr)
		report.ReusedTicks = append(report.ReusedTicks, t)
	}
	for _, tr := range suffix.Result.Ticks {
		tr.Invalidation = &model.TickInvalidation{
			Recomputed: true, Sources: []string{edit.Source(), "dependency:agent-partition-backpressure-closure"},
			PropagationClosure: closure,
		}
		result.Ticks = append(result.Ticks, tr)
		report.RecomputedTicks = append(report.RecomputedTicks, tr.Tick)
	}
	report.Result = &result
	return &result, report, nil
}

func seedDependency(edit EditSpec, oldProject *model.Project, oldChunks, newChunks []model.Chunk) (map[string]bool, []model.Chunk, error) {
	seeds := map[string]bool{}
	var chunks []model.Chunk
	switch edit.Kind {
	case "agent":
		seeds["agent:"+edit.TargetID] = true
		for _, chunk := range newChunks {
			if chunk.AgentID == edit.TargetID {
				chunks = append(chunks, chunk)
			}
		}
	case "partition":
		seeds["partition:"+edit.TargetID] = true
		for _, chunk := range newChunks {
			if chunk.PartitionID == edit.TargetID {
				chunks = append(chunks, chunk)
			}
		}
	case "rule":
		seeds["rule:"+edit.TargetID] = true
		oldByID := map[string]model.Chunk{}
		for _, chunk := range oldChunks {
			oldByID[chunk.ChunkID] = chunk
		}
		changedPartition := map[string]bool{}
		for _, next := range newChunks {
			prev, ok := oldByID[next.ChunkID]
			if !ok || prev.PartitionID != next.PartitionID {
				chunks = append(chunks, next)
				changedPartition[next.PartitionID] = true
				if ok {
					changedPartition[prev.PartitionID] = true
					seeds["partition:"+prev.PartitionID] = true
				}
				seeds["partition:"+next.PartitionID] = true
				seeds["agent:"+next.AgentID] = true
			}
		}
		for id := range changedPartition {
			seeds["partition:"+id] = true
		}
	default:
		return nil, nil, bpderr.New(bpderr.ESchemaInvalid, "edit.kind", "unknown incremental edit kind")
	}
	return seeds, chunks, nil
}

func propagationClosure(chunks []model.Chunk, seeds map[string]bool, targetRule string) []string {
	adj := map[string]map[string]bool{}
	addEdge := func(a, b string) {
		if adj[a] == nil {
			adj[a] = map[string]bool{}
		}
		if adj[b] == nil {
			adj[b] = map[string]bool{}
		}
		adj[a][b] = true
		adj[b][a] = true
	}
	for _, chunk := range chunks {
		addEdge("agent:"+chunk.AgentID, "partition:"+chunk.PartitionID)
	}

	visited := map[string]bool{}
	queue := []string{}
	for seed := range seeds {
		if seed == "rule:"+targetRule {
			continue
		}
		if !visited[seed] {
			visited[seed] = true
			queue = append(queue, seed)
		}
	}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		for next := range adj[node] {
			if !visited[next] {
				visited[next] = true
				queue = append(queue, next)
			}
		}
	}
	if targetRule != "" {
		visited["rule:"+targetRule] = true
	}
	out := make([]string, 0, len(visited))
	for node := range visited {
		out = append(out, node)
	}
	sort.Strings(out)
	return out
}

func rebaseStaticDeclarations(tick *model.TickReport, p *model.Project) {
	agents := map[string]model.Agent{}
	partitions := map[string]model.Partition{}
	for _, agent := range p.Agents {
		agents[agent.ID] = agent
	}
	for _, partition := range p.Partitions {
		partitions[partition.ID] = partition
	}
	for i := range tick.Agents {
		if agent, ok := agents[tick.Agents[i].AgentID]; ok {
			tick.Agents[i].BufferMaxBytes = agent.BufferMaxBytes
		}
	}
	for i := range tick.Partitions {
		report := &tick.Partitions[i]
		partition, ok := partitions[report.PartitionID]
		if !ok {
			continue
		}
		report.BacklogLimitBytes = partition.BacklogLimitBytes
		if report.WatermarkSource != model.WatermarkUpdated {
			if partition.ConsumerWatermark != nil {
				report.WatermarkSource = model.WatermarkDeclared
				report.EffectiveWatermark = *partition.ConsumerWatermark
			} else {
				report.WatermarkSource = model.WatermarkDerived
				report.EffectiveWatermark = report.AckedBytes
			}
			report.WatermarkLagBytes = max64(0, report.AckedBytes-report.EffectiveWatermark)
			report.WatermarkBacklogBytes = max64(0, report.WrittenBytes-report.EffectiveWatermark)
		}
	}
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func FullRecompute(ctx context.Context, p *model.Project, budget engine.Budget) (*engine.Simulation, error) {
	runner, err := engine.NewRunner(budget)
	if err != nil {
		return nil, err
	}
	return runner.Run(ctx, p)
}
