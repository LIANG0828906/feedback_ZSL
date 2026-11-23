package streaming

import (
	"sort"

	"occlbench/internal/core"
	"occlbench/internal/scene"
)

// Budget 加载槽位与内存预算配置。
type Budget struct {
	// Slots 并发加载槽位上限（每窗口可新驻留的分块数上限）。
	Slots int `json:"slots"`
	// Memory 内存预算（分块 MemoryCost 之和上限）。
	Memory float64 `json:"memory"`
}

// Validate 非法槽位或预算返回 E_BUDGET 并指出字段名。
func (b Budget) Validate() error {
	if b.Slots <= 0 {
		return core.NewError(core.EBudget, "slots must be positive",
			map[string]any{"field": "budget.slots", "value": b.Slots})
	}
	if b.Memory <= 0 || core.IsInf(b.Memory) || core.IsNaN(b.Memory) {
		return core.NewError(core.EBudget, "memory must be positive finite",
			map[string]any{"field": "budget.memory", "value": b.Memory})
	}
	return nil
}

// Action 单条加载/卸载动作。
type Action struct {
	Kind    string  `json:"kind"` // "load" 或 "unload"
	ChunkID int     `json:"chunkId"`
	Cost    float64 `json:"cost"`
	Reason  string  `json:"reason"`
}

// Schedule 单个窗口的确定性调度结果。
type Schedule struct {
	WindowIndex int      `json:"windowIndex"`
	Loads       []Action `json:"loads"`
	Unloads     []Action `json:"unloads"`
	// Delayed 因槽位或内存预算耗尽而排队延迟的分块（不静默丢弃）。
	Delayed []DelayedChunk `json:"delayed"`
	// Resident 调度完成后驻留分块（升序）。
	Resident []int `json:"resident"`
}

// DelayedChunk 被延迟分块及原因。
type DelayedChunk struct {
	ChunkID int     `json:"chunkId"`
	Reason  string  `json:"reason"`
	Cost    float64 `json:"cost"`
}

// Scheduler 确定性调度器。给定相同驻留集合、可见集合与预算，输出完全一致。
type Scheduler struct {
	budget Budget
	chunks map[int]*scene.Chunk
}

// NewScheduler 构造调度器。
func NewScheduler(w *scene.World, budget Budget) (*Scheduler, error) {
	if err := budget.Validate(); err != nil {
		return nil, err
	}
	return &Scheduler{budget: budget, chunks: w.ChunkByID()}, nil
}

// priorityCandidate 加载候选分块。
type priorityCandidate struct {
	id       int
	distance float64
	cost     float64
	memory   float64
	visible  bool
}

// Plan 根据当前驻留集合与本窗口可见集合，计算卸载、加载与延迟序列。
// resident 为上一窗口结束后的驻留分块；camPos 用于距离优先级。
func (s *Scheduler) Plan(windowIndex int, resident []int, visible []int, camPos core.Vec3) Schedule {
	resSet := map[int]bool{}
	for _, id := range resident {
		resSet[id] = true
	}
	visSet := map[int]bool{}
	for _, id := range visible {
		visSet[id] = true
	}

	sched := Schedule{WindowIndex: windowIndex}
	sched.Loads = []Action{}
	sched.Unloads = []Action{}
	sched.Delayed = []DelayedChunk{}
	sched.Resident = []int{}

	// 1) 卸载：不再可见且不在等待集合中的驻留分块，按 ID 降序卸载（确定顺序）。
	unloadCandidates := make([]int, 0)
	for id := range resSet {
		if !visSet[id] {
			unloadCandidates = append(unloadCandidates, id)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(unloadCandidates)))
	currentResident := append([]int{}, resident...)
	resMem := 0.0
	resSet2 := map[int]bool{}
	for _, id := range currentResident {
		resSet2[id] = true
		resMem += s.chunks[id].MemoryCost
	}
	for _, id := range unloadCandidates {
		ch := s.chunks[id]
		sched.Unloads = append(sched.Unloads, Action{
			Kind: "unload", ChunkID: id, Cost: ch.LoadCost, Reason: "no-longer-visible",
		})
		delete(resSet2, id)
		resMem -= ch.MemoryCost
	}

	// 2) 加载候选：仅可见且未驻留分块；未可见分块绝不进入候选（不抢占槽位）。
	candidates := make([]priorityCandidate, 0, len(visible))
	for _, id := range visible {
		if resSet2[id] {
			continue
		}
		ch := s.chunks[id]
		candidates = append(candidates, priorityCandidate{
			id:       id,
			distance: ch.Box.Distance(camPos),
			cost:     ch.LoadCost,
			memory:   ch.MemoryCost,
			visible:  true,
		})
	}
	// 优先级：距离升序，距离相同按加载成本升序，再按 ID 升序，保证确定性。
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.distance != b.distance {
			return a.distance < b.distance
		}
		if a.cost != b.cost {
			return a.cost < b.cost
		}
		return a.id < b.id
	})

	// 3) 在槽位与内存预算内加载；预算耗尽则记录延迟而非丢弃。
	slotsUsed := 0
	for _, cand := range candidates {
		if slotsUsed >= s.budget.Slots {
			sched.Delayed = append(sched.Delayed, DelayedChunk{
				ChunkID: cand.id, Reason: "slot-exhausted", Cost: cand.cost,
			})
			continue
		}
		if resMem+cand.memory > s.budget.Memory {
			sched.Delayed = append(sched.Delayed, DelayedChunk{
				ChunkID: cand.id, Reason: "memory-exhausted", Cost: cand.cost,
			})
			continue
		}
		sched.Loads = append(sched.Loads, Action{
			Kind: "load", ChunkID: cand.id, Cost: cand.cost, Reason: "visible-priority",
		})
		resSet2[cand.id] = true
		resMem += cand.memory
		slotsUsed++
	}

	for id := range resSet2 {
		sched.Resident = append(sched.Resident, id)
	}
	sort.Ints(sched.Resident)
	return sched
}
