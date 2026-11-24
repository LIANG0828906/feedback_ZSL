package terrain

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type BudgetConfig struct {
	MaxParallel           int   `json:"max_parallel"`
	PerWorkerVertexBuffer int   `json:"per_worker_vertex_buffer"`
	MeshMemoryBudgetBytes int64 `json:"mesh_memory_budget_bytes"`
}

func DefaultBudgetConfig() BudgetConfig {
	return BudgetConfig{MaxParallel: 4, PerWorkerVertexBuffer: 4096, MeshMemoryBudgetBytes: 64 << 20}
}

type BudgetOccupancy struct {
	ActiveWorkers int   `json:"active_workers"`
	MaxParallel   int   `json:"max_parallel"`
	UsedBytes     int64 `json:"used_bytes"`
	BudgetBytes   int64 `json:"budget_bytes"`
	Queued        int   `json:"queued"`
}

type BudgetMeter struct {
	cfg    BudgetConfig
	mu     sync.Mutex
	cond   *sync.Cond
	active int
	used   int64
	queued int
	events []QueueEvent
	clock  func() time.Time
}

func NewBudgetMeter(cfg BudgetConfig) *BudgetMeter {
	if cfg.MaxParallel <= 0 {
		cfg.MaxParallel = 1
	}
	if cfg.PerWorkerVertexBuffer <= 0 {
		cfg.PerWorkerVertexBuffer = 1024
	}
	if cfg.MeshMemoryBudgetBytes <= 0 {
		cfg.MeshMemoryBudgetBytes = 1 << 20
	}
	m := &BudgetMeter{cfg: cfg, clock: time.Now}
	m.cond = sync.NewCond(&m.mu)
	return m
}

func EstimateBlockBytes(block Block) int64 {
	worstV := int64(block.Grid.Width * block.Grid.Height)
	return worstV*192 + worstV*2*48 + 4096
}

func ActualMeshBytes(mesh *Mesh) int64 {
	if mesh == nil {
		return 0
	}
	return int64(len(mesh.Vertices)*96 + len(mesh.Triangles)*48 + len(mesh.TriangleErrors)*72 + 4096)
}

func (m *BudgetMeter) Config() BudgetConfig { return m.cfg }

func (m *BudgetMeter) Events() []QueueEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]QueueEvent{}, m.events...)
}

func (m *BudgetMeter) Occupancy() BudgetOccupancy {
	m.mu.Lock()
	defer m.mu.Unlock()
	return BudgetOccupancy{ActiveWorkers: m.active, MaxParallel: m.cfg.MaxParallel, UsedBytes: m.used, BudgetBytes: m.cfg.MeshMemoryBudgetBytes, Queued: m.queued}
}

type Lease struct {
	blockNo       int
	estimate      int64
	actualCharged int64
	meter         *BudgetMeter
	done          bool
}

func (m *BudgetMeter) Acquire(ctx context.Context, blockNo, worstVertices int, estimate int64) (*Lease, error) {
	if worstVertices > m.cfg.PerWorkerVertexBuffer {
		bn := blockNo
		return nil, &CodedError{Code: CodeBudget, BlockNo: &bn, Path: "budget.per_worker_vertex_buffer",
			Message: fmt.Sprintf("block needs %d vertex buffer slots but per-worker cap is %d", worstVertices, m.cfg.PerWorkerVertexBuffer),
			Details: map[string]any{"worst_vertices": worstVertices, "cap": m.cfg.PerWorkerVertexBuffer}}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	queued := false
	var start time.Time
	stopWatch := context.AfterFunc(ctx, func() {
		m.mu.Lock()
		m.cond.Broadcast()
		m.mu.Unlock()
	})
	defer stopWatch()

	for {
		if err := ctx.Err(); err != nil {
			if queued {
				m.queued--
			}
			return nil, err
		}
		reason := ""
		if m.active >= m.cfg.MaxParallel {
			reason = "max_parallel"
		} else if m.used+estimate > m.cfg.MeshMemoryBudgetBytes {
			reason = "mesh_memory_budget"
		}
		if reason == "" {
			break
		}
		if !queued {
			queued = true
			m.queued++
			start = m.clock()
			m.events = append(m.events, QueueEvent{BlockNo: blockNo, State: "queued", Reason: reason, AtMS: start.UnixMilli(), Vertices: worstVertices, Bytes: estimate})
		}
		m.cond.Wait()
	}

	if queued {
		m.queued--
		waitMS := m.clock().Sub(start).Milliseconds()
		m.events = append(m.events, QueueEvent{BlockNo: blockNo, State: "admitted", Reason: "budget_available", WaitMS: waitMS, AtMS: m.clock().UnixMilli(), Vertices: worstVertices, Bytes: estimate})
	}
	m.active++
	m.used += estimate
	return &Lease{blockNo: blockNo, estimate: estimate, meter: m}, nil
}

func (l *Lease) Complete(actualBytes int64) {
	if l == nil || l.done {
		return
	}
	m := l.meter
	m.mu.Lock()
	defer m.mu.Unlock()
	m.active--
	m.used -= l.estimate
	m.used += actualBytes
	l.actualCharged = actualBytes
	l.done = true
	m.cond.Broadcast()
}

func (l *Lease) Release() { l.Complete(0) }

func (m *BudgetMeter) AddCached(bytes int64) {
	m.mu.Lock()
	m.used += bytes
	m.mu.Unlock()
}

func (m *BudgetMeter) ReleaseCached(bytes int64) {
	m.mu.Lock()
	m.used = maxInt64(0, m.used-bytes)
	m.cond.Broadcast()
	m.mu.Unlock()
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
