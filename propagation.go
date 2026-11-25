package orchestrator

import (
	"fmt"
	"sort"
	"sync"
)

type reachState struct {
	generation int
	chain      []string
	sources    map[string]struct{}
}

type neighborTask struct {
	index  int
	parent string
}

type neighborResult struct {
	index    int
	parent   string
	children []string
}

// propagation performs a deterministic multi-source BFS from every node in
// every active isolation domain. It parallelizes only independent frontier
// chunks. The merge is ordered by parent and child, so worker scheduling can
// never affect chains, generations, or traversal order.
func (s *simulation) propagation(tick int64) ([]PropagationState, error) {
	if len(s.state.Domains) == 0 {
		return nil, nil
	}
	domainIDs := make([]int64, 0, len(s.state.Domains))
	for id := range s.state.Domains {
		domainIDs = append(domainIDs, id)
	}
	sort.Slice(domainIDs, func(i, j int) bool { return domainIDs[i] < domainIDs[j] })

	best := make(map[string]*reachState)
	for _, domainID := range domainIDs {
		for _, nodeID := range s.domainNodes(domainID) {
			source := fmt.Sprintf("domain:%d", domainID)
			if existing, ok := best[nodeID]; ok {
				existing.sources[source] = struct{}{}
				continue
			}
			best[nodeID] = &reachState{
				generation: 0,
				chain:      []string{nodeID},
				sources:    map[string]struct{}{source: {}},
			}
		}
	}
	frontier := make([]string, 0, len(best))
	for nodeID := range best {
		frontier = append(frontier, nodeID)
	}
	sort.Strings(frontier)

	for layer := 0; len(frontier) > 0; layer++ {
		chunks := chunkStrings(frontier, s.perDomainCap)
		if pending := len(frontier); pending > s.workers*s.perDomainCap {
			batches := (len(chunks) + s.workers - 1) / s.workers
			deferred := pending - s.workers*s.perDomainCap
			signal := BackpressureSignal{
				Tick:          tick,
				DomainIDs:     append([]int64(nil), domainIDs...),
				Layer:         layer,
				PendingNodes:  pending,
				WorkerBudget:  s.workers,
				PerDomainCap:  s.perDomainCap,
				Batches:       batches,
				DeferredNodes: deferred,
			}
			s.recordBackpressure(signal)
		}
		results := s.expandFrontier(chunks)
		sort.Slice(results, func(i, j int) bool {
			if results[i].parent != results[j].parent {
				return results[i].parent < results[j].parent
			}
			return results[i].index < results[j].index
		})

		parents := make(map[string][]string)
		for _, result := range results {
			for _, child := range result.children {
				parents[child] = append(parents[child], result.parent)
			}
		}
		nextSet := make(map[string]struct{})
		children := make([]string, 0, len(parents))
		for child := range parents {
			children = append(children, child)
		}
		sort.Strings(children)
		for _, child := range children {
			parentList := uniqueSorted(parents[child])
			if existing, ok := best[child]; ok {
				if existing.generation == layer+1 {
					for _, parent := range parentList {
						for source := range best[parent].sources {
							existing.sources[source] = struct{}{}
						}
						existing.sources[fmt.Sprintf("edge:%s->%s", parent, child)] = struct{}{}
					}
				}
				continue
			}
			parent := parentList[0]
			parentState := best[parent]
			sources := cloneStringSet(parentState.sources)
			sources[fmt.Sprintf("edge:%s->%s", parent, child)] = struct{}{}
			for _, alternative := range parentList[1:] {
				for source := range best[alternative].sources {
					sources[source] = struct{}{}
				}
			}
			chain := append(append([]string(nil), parentState.chain...), child)
			best[child] = &reachState{generation: layer + 1, chain: chain, sources: sources}
			nextSet[child] = struct{}{}
		}
		frontier = frontier[:0]
		for child := range nextSet {
			frontier = append(frontier, child)
		}
		sort.Strings(frontier)
	}

	out := make([]PropagationState, 0, len(best))
	for nodeID, reach := range best {
		out = append(out, PropagationState{
			NodeID:     nodeID,
			Generation: reach.generation,
			Chain:      append([]string(nil), reach.chain...),
			Sources:    sortedSet(reach.sources),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Generation != out[j].Generation {
			return out[i].Generation < out[j].Generation
		}
		return out[i].NodeID < out[j].NodeID
	})
	return out, nil
}

func (s *simulation) expandFrontier(chunks [][]string) []neighborResult {
	if len(chunks) == 0 {
		return nil
	}
	workers := s.workers
	if workers > len(chunks) {
		workers = len(chunks)
	}
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan neighborTask)
	results := make([]neighborResult, 0, len(chunks))
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range jobs {
				children := append([]string(nil), s.reverse[task.parent]...)
				mu.Lock()
				results = append(results, neighborResult{index: task.index, parent: task.parent, children: children})
				mu.Unlock()
			}
		}()
	}
	for index, chunk := range chunks {
		for _, parent := range chunk {
			jobs <- neighborTask{index: index, parent: parent}
		}
	}
	close(jobs)
	wg.Wait()
	sort.Slice(results, func(i, j int) bool { return results[i].index < results[j].index })
	return results
}

func (s *simulation) recordBackpressure(signal BackpressureSignal) {
	key := fmt.Sprintf("%d/%v/%d/%d/%d", signal.Tick, signal.DomainIDs, signal.Layer, signal.PendingNodes, signal.DeferredNodes)
	if _, ok := s.bpSeen[key]; ok {
		return
	}
	s.bpSeen[key] = struct{}{}
	s.backpressure = append(s.backpressure, signal)
}

func chunkStrings(values []string, limit int) [][]string {
	if len(values) == 0 {
		return nil
	}
	if limit <= 0 {
		limit = 1
	}
	chunks := make([][]string, 0, (len(values)+limit-1)/limit)
	for start := 0; start < len(values); start += limit {
		end := start + limit
		if end > len(values) {
			end = len(values)
		}
		chunks = append(chunks, values[start:end])
	}
	return chunks
}
