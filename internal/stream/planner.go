package stream

import (
	"math"
	"sort"

	"terrain-occlusion/internal/model"
)

type candidate struct {
	id       int
	distance float64
	cost     int
	memory   int64
	age      int
	queued   bool
}

// Planner is the deterministic streaming state machine for residency.
type Planner struct {
	Budgets     model.Budgets
	Resident    map[int]bool
	Queue       []int
	RequestedAt map[int]int
	Reasons     map[int]string
	Completed   map[int]int
	History     []model.DelayedChunk
}

func NewPlanner(b model.Budgets) *Planner {
	return &Planner{
		Budgets:     b,
		Resident:    map[int]bool{},
		RequestedAt: map[int]int{},
		Reasons:     map[int]string{},
		Completed:   map[int]int{},
	}
}

type ReconcileResult struct {
	ResidentIDs  []int
	QueuedIDs    []int
	CancelledIDs []int
	Loads        []model.LoadEvent
	Unloads      []model.UnloadEvent
	Delayed      []model.DelayedChunk
}

func (p *Planner) SnapshotDelayed() []model.DelayedChunk {
	out := append([]model.DelayedChunk(nil), p.History...)
	for id, requested := range p.RequestedAt {
		if p.Completed[id] == 0 && !p.Resident[id] {
			out = append(out, model.DelayedChunk{
				ChunkID:         id,
				RequestedSample: requested,
				Reason:          p.Reasons[id],
				DelaySamples:    0,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ChunkID < out[j].ChunkID })
	return out
}

// Reconcile advances one logical sample. Distances are camera-to-chunk centers.
func (p *Planner) Reconcile(sampleIndex int, project *model.Project, desired []int, distances map[int]float64) (*ReconcileResult, error) {
	byID := map[int]model.Chunk{}
	for _, c := range project.Chunks {
		byID[c.ID] = c
	}
	desiredSet := map[int]bool{}
	for _, id := range desired {
		desiredSet[id] = true
		if _, ok := p.RequestedAt[id]; !ok && !p.Resident[id] {
			p.RequestedAt[id] = sampleIndex
		}
	}

	// A queued request that is no longer visible is explicitly cancelled; it
	// is never silently removed. Remaining queue entries keep request age.
	queueSet := map[int]bool{}
	candidates := []candidate{}
	cancelled := []int{}
	for _, id := range p.Queue {
		if p.Resident[id] {
			continue
		}
		if !desiredSet[id] {
			cancelled = append(cancelled, id)
			delete(p.RequestedAt, id)
			delete(p.Reasons, id)
			continue
		}
		queueSet[id] = true
		c := byID[id]
		candidates = append(candidates, candidate{id: id, distance: distances[id], cost: c.LoadCost, memory: c.MemoryBytes, age: sampleIndex - p.RequestedAt[id], queued: true})
	}
	newIDs := []int{}
	for _, id := range desired {
		if p.Resident[id] || queueSet[id] {
			continue
		}
		if _, requested := p.RequestedAt[id]; !requested {
			p.RequestedAt[id] = sampleIndex
		}
		newIDs = append(newIDs, id)
	}
	sort.Slice(newIDs, func(i, j int) bool {
		return priorityLess(newIDs[i], newIDs[j], distances, byID)
	})
	for _, id := range newIDs {
		c := byID[id]
		candidates = append(candidates, candidate{id: id, distance: distances[id], cost: c.LoadCost, memory: c.MemoryBytes, age: 0})
	}

	// Aged requests go first; within the same age, near/cheap chunks go first.
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].age != candidates[j].age {
			return candidates[i].age > candidates[j].age
		}
		return priorityLess(candidates[i].id, candidates[j].id, distances, byID)
	})

	var used int64
	for id := range p.Resident {
		used += byID[id].MemoryBytes
	}
	loads := []model.LoadEvent{}
	unloads := []model.UnloadEvent{}
	loadCount := 0
	newQueue := []int{}

	for _, cand := range candidates {
		id := cand.id
		if p.Resident[id] {
			continue
		}
		if loadCount >= p.Budgets.LoadSlots {
			p.Reasons[id] = "load_slots_exhausted"
			newQueue = append(newQueue, id)
			continue
		}
		if used+cand.memory > p.Budgets.MemoryBytes {
			freed, events := p.unloadForMemory(sampleIndex, byID, desiredSet, distances, cand.memory, p.Budgets.MemoryBytes-used-cand.memory)
			unloads = append(unloads, events...)
			used -= freed
		}
		if used+cand.memory > p.Budgets.MemoryBytes {
			p.Reasons[id] = "memory_budget_exhausted"
			newQueue = append(newQueue, id)
			continue
		}

		p.Resident[id] = true
		used += cand.memory
		loadCount++
		loads = append(loads, model.LoadEvent{SampleIndex: sampleIndex, ChunkID: id, Reason: "visible_priority"})
		if requested, ok := p.RequestedAt[id]; ok {
			delay := sampleIndex - requested
			p.Completed[id] = sampleIndex
			if delay > 0 {
				p.History = append(p.History, model.DelayedChunk{
					ChunkID: id, RequestedSample: requested,
					Reason: p.Reasons[id], CompletedSample: sampleIndex, DelaySamples: delay,
				})
			}
			delete(p.RequestedAt, id)
			delete(p.Reasons, id)
		}
	}

	// Keep queue unique and in stable priority order for the next sample.
	sort.Ints(newQueue)
	uniqueQueue := newQueue[:0]
	last := 0
	for _, id := range newQueue {
		if len(uniqueQueue) == 0 || id != last {
			uniqueQueue = append(uniqueQueue, id)
		}
		last = id
	}
	p.Queue = uniqueQueue
	sort.Ints(cancelled)

	// Deadline enforcement catches starved requests before they silently disappear.
	for id, requested := range p.RequestedAt {
		delay := sampleIndex - requested
		if delay > p.Budgets.MaxResidencyDelay {
			return nil, &model.Error{
				Code: model.ELoad, Field: "streaming.residency",
				Message: "chunk %d residency delayed by %d samples",
				Details: map[string]any{"chunk_id": id, "delayed_samples": delay, "max": p.Budgets.MaxResidencyDelay, "reason": p.Reasons[id]},
			}
		}
	}

	residentIDs := make([]int, 0, len(p.Resident))
	for id := range p.Resident {
		residentIDs = append(residentIDs, id)
	}
	sort.Ints(residentIDs)
	sort.Ints(p.Queue)

	return &ReconcileResult{
		ResidentIDs:  residentIDs,
		QueuedIDs:    append([]int(nil), p.Queue...),
		CancelledIDs: append([]int(nil), cancelled...),
		Loads:        loads,
		Unloads:      unloads,
		Delayed:      p.SnapshotDelayed(),
	}, nil
}

func priorityLess(a, b int, distances map[int]float64, byID map[int]model.Chunk) bool {
	scoreA := distances[a] + float64(byID[a].LoadCost)*0.05
	scoreB := distances[b] + float64(byID[b].LoadCost)*0.05
	if scoreA != scoreB {
		return scoreA < scoreB
	}
	return a < b
}

func (p *Planner) unloadForMemory(sampleIndex int, byID map[int]model.Chunk, desired map[int]bool, distances map[int]float64, needMemory, shortfall int64) (int64, []model.UnloadEvent) {
	type resident struct {
		id       int
		distance float64
		cost     int
		memory   int64
	}
	candidates := []resident{}
	for id := range p.Resident {
		if desired[id] {
			continue
		}
		c := byID[id]
		candidates = append(candidates, resident{id: id, distance: distances[id], cost: c.LoadCost, memory: c.MemoryBytes})
	}
	// Farthest and most expensive cache blocks are evicted first.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].distance != candidates[j].distance {
			return candidates[i].distance > candidates[j].distance
		}
		if candidates[i].cost != candidates[j].cost {
			return candidates[i].cost > candidates[j].cost
		}
		return candidates[i].id > candidates[j].id
	})
	var freed int64
	events := []model.UnloadEvent{}
	for _, r := range candidates {
		if freed >= shortfall && freed >= needMemory {
			break
		}
		delete(p.Resident, r.id)
		freed += r.memory
		events = append(events, model.UnloadEvent{SampleIndex: sampleIndex, ChunkID: r.id, Reason: "memory_budget_eviction"})
	}
	return freed, events
}

func Distances(project *model.Project, eye model.Vec3) map[int]float64 {
	out := map[int]float64{}
	for _, c := range project.Chunks {
		center := model.Vec3{
			X: (c.Rect.MinX + c.Rect.MaxX) / 2,
			Y: 0,
			Z: (c.Rect.MinZ + c.Rect.MaxZ) / 2,
		}
		out[c.ID] = geoDistance(center, eye)
	}
	return out
}

func geoDistance(a, b model.Vec3) float64 {
	dx := a.X - b.X
	dy := a.Y - b.Y
	dz := a.Z - b.Z
	return math.Sqrt(dx*dx + dy*dy + dz*dz)
}
