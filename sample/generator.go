// Package sample contains the deterministic local seed project and truth.
package sample

import (
	"fmt"
	"math/rand"

	"github.com/liang/collector-backpressure/model"
	"github.com/liang/collector-backpressure/planner"
)

const DefaultSeed int64 = 42

type BatchTruth struct {
	BatchID      string `json:"batch_id"`
	AgentID      string `json:"agent_id"`
	PartitionID  string `json:"partition_id"`
	ChunkCount   int    `json:"chunk_count"`
	MessageCount int    `json:"message_count"`
	Bytes        int64  `json:"bytes"`
}

type Bundle struct {
	Project *model.Project
	Truth   []BatchTruth
}

type batchSpec struct {
	tick   int
	agent  string
	tenant string
	key    string
	sizes  []int64
}

func GenerateProject(seed int64) *model.Project {
	rng := rand.New(rand.NewSource(seed))
	wm0 := int64(0)
	p := &model.Project{
		ID: "local-backpressure-demo", Version: "engineering-v1", SchemaVersion: "canonical",
		Seed: seed, TickMillis: 10, RateBaseBytes: 10000, BackoffBaseTick: 1000, Ticks: 14,
		Agents: []model.Agent{
			{ID: "a1", Host: "wsl-agent-1", BufferMaxBytes: 260, MaxBatchBytes: 120, MaxBatchMessages: 3, BackoffTicks: 2, PendingQueueLimit: 3, OverflowPolicy: model.OverflowDropNewest, BufferExhaustedLimitTicks: 2},
			{ID: "a2", Host: "wsl-agent-2", BufferMaxBytes: 360, MaxBatchBytes: 160, MaxBatchMessages: 4, BackoffTicks: 1, PendingQueueLimit: 4, OverflowPolicy: model.OverflowDropNewest, BufferExhaustedLimitTicks: 2},
			{ID: "a3", Host: "wsl-agent-3", BufferMaxBytes: 420, MaxBatchBytes: 160, MaxBatchMessages: 4, BackoffTicks: 2, PendingQueueLimit: 5, OverflowPolicy: model.OverflowDropNewest, BufferExhaustedLimitTicks: 3},
		},
		Partitions: []model.Partition{
			{ID: "p1", Index: 0, RateBytesPerTick: 60, AckDelayTicks: 2, BacklogLimitBytes: 220},
			{ID: "p2", Index: 1, RateBytesPerTick: 120, AckDelayTicks: 1, BacklogLimitBytes: 320},
			{ID: "p3", Index: 2, RateBytesPerTick: 180, AckDelayTicks: 1, BacklogLimitBytes: 420, ConsumerWatermark: &wm0},
		},
		Rules: []model.KeyRule{
			{ID: "r-acme-all", Tenant: "acme", Key: "*", PartitionID: "p3", Priority: 0},
			{ID: "r-acme-click", Tenant: "acme", Key: "click", PartitionID: "p1", ParentID: "r-acme-all", Priority: 10},
			{ID: "r-acme-view", Tenant: "acme", Key: "view", PartitionID: "p2", ParentID: "r-acme-all", Priority: 10},
			{ID: "r-globex-all", Tenant: "globex", Key: "*", PartitionID: "p2", Priority: 1},
			{ID: "r-global-audit", Tenant: "*", Key: "audit", PartitionID: "p1", Priority: 5},
		},
		WatermarkUpdates: []model.WatermarkUpdate{
			{PartitionID: "p3", Tick: 6, Watermark: 120},
		},
	}

	specs := []batchSpec{
		{0, "a1", "acme", "click", []int64{40, 40, 40, 40}},
		{0, "a2", "acme", "view", []int64{40, 40, 40, 40}},
		{1, "a1", "acme", "click", []int64{40, 40, 40}},
		{1, "a3", "acme", "trace", []int64{40, 40, 40, 40}},
		{1, "a3", "globex", "event", []int64{40, 40, 40, 40}},
		{2, "a1", "*", "audit", []int64{40, 40}},
		{2, "a2", "acme", "view", []int64{32, 32, 32, 32, 32}},
		{3, "a3", "acme", "view", []int64{36, 36, 36, 36}},
		{3, "a2", "*", "audit", []int64{40, 40, 40}},
		{4, "a1", "acme", "click", []int64{40, 40, 40, 40}},
		{4, "a1", "acme", "click", []int64{40, 40, 40}},
		{4, "a1", "*", "audit", []int64{40, 40}},
		{4, "a3", "globex", "event", []int64{40, 40, 40}},
		{5, "a2", "acme", "view", []int64{40, 40}},
		{5, "a3", "acme", "click", []int64{40, 40}},
		{6, "a1", "acme", "view", []int64{40, 40, 40}},
		{6, "a2", "*", "audit", []int64{40, 40}},
		{7, "a3", "globex", "event", []int64{40, 40, 40, 40}},
		{7, "a1", "acme", "click", []int64{40}},
		{8, "a2", "acme", "view", []int64{40, 40, 40, 40}},
		{8, "a3", "*", "audit", []int64{40, 40, 40}},
		{9, "a1", "acme", "view", []int64{40, 40}},
		{9, "a2", "globex", "event", []int64{40, 40, 40}},
		{10, "a3", "acme", "view", []int64{40, 40}},
		{10, "a1", "*", "audit", []int64{40, 40}},
		{11, "a2", "acme", "view", []int64{32, 32, 32}},
		{12, "a3", "acme", "trace", []int64{24, 24, 24, 24}},
	}

	for i, spec := range specs {
		batch := model.Batch{
			ID: fmt.Sprintf("B-%03d", i+1), AgentID: spec.agent, Tenant: spec.tenant,
			Key: spec.key, ArrivalTick: spec.tick,
		}
		for j, size := range spec.sizes {
			batch.Messages = append(batch.Messages, model.Message{
				ID: fmt.Sprintf("%s-M%02d", batch.ID, j+1), Bytes: size,
			})
		}
		p.Batches = append(p.Batches, batch)
	}

	// Seed is part of the reproducible contract. Draw one benign jitter value so
	// future generators can evolve sizes without changing the fixed seed path.
	_ = rng.Intn(8)
	return p
}

func GenerateBundle(seed int64) (*Bundle, error) {
	p := GenerateProject(seed)
	chunks, err := planner.BuildChunks(p)
	if err != nil {
		return nil, err
	}
	agg := map[string]*BatchTruth{}
	for _, batch := range p.Batches {
		agg[batch.ID] = &BatchTruth{BatchID: batch.ID, AgentID: batch.AgentID}
	}
	for _, chunk := range chunks {
		t := agg[chunk.BatchID]
		t.PartitionID = chunk.PartitionID
		t.ChunkCount++
		t.MessageCount += chunk.Count
		t.Bytes += chunk.Bytes
	}
	truth := make([]BatchTruth, 0, len(p.Batches))
	for _, batch := range p.Batches {
		truth = append(truth, *agg[batch.ID])
	}
	return &Bundle{Project: p, Truth: truth}, nil
}
