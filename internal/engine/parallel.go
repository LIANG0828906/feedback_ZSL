package engine

import (
	"sort"
	"sync"
	"time"
)

type retryProbe struct {
	quotaRemaining int64
	admission      int64
}

func (e *Engine) parallelRetryProbes(state *State, tenantIDs []string) (map[string]retryProbe, error) {
	if len(tenantIDs) == 0 {
		return map[string]retryProbe{}, nil
	}
	workers := e.cfg.ThreadBudget
	if workers > len(tenantIDs) {
		workers = len(tenantIDs)
	}
	type result struct {
		id    string
		probe retryProbe
		nanos int64
		err   error
	}
	jobs := make(chan string)
	results := make(chan result, len(tenantIDs))
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for tenantID := range jobs {
				start := time.Now()
				tenant := state.Tenants[tenantID]
				probe := retryProbe{quotaRemaining: tenant.Quota - tenant.Used}
				var err error
				if len(tenant.Pending) > 0 {
					nodeID := tenant.Pending[0].NodeID
					probe.admission, err = e.admissionCapacity(state, tenantID, nodeID)
				}
				results <- result{id: tenantID, probe: probe, nanos: time.Since(start).Nanoseconds(), err: err}
			}
		}()
	}
	go func() {
		for _, tenantID := range tenantIDs {
			jobs <- tenantID
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()
	out := make(map[string]retryProbe, len(tenantIDs))
	nanos := map[string]int64{}
	var firstErr error
	for item := range results {
		out[item.id] = item.probe
		nanos[item.id] = item.nanos
		if item.err != nil && firstErr == nil {
			firstErr = item.err
		}
	}
	ids := make([]string, 0, len(nanos))
	for id := range nanos {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		e.stats.addTiming(id, nanos[id])
	}
	return out, firstErr
}

// parallelTargetProbes evaluates candidate target sets against a frozen state.
// The sequential chooseTarget pass revalidates against virtual reservations
// created by earlier migrations in the same failure tick.
func (e *Engine) parallelTargetProbes(state *State, plans []targetPlan) [][]string {
	if len(plans) == 0 {
		return nil
	}
	type result struct {
		index      int
		candidates []string
		nanos      int64
	}
	workers := e.cfg.ThreadBudget
	if workers > len(plans) {
		workers = len(plans)
	}
	jobs := make(chan int)
	results := make(chan result, len(plans))
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for index := range jobs {
				start := time.Now()
				plan := plans[index]
				failedNode, _ := e.project.NodeByID(plan.failedNode)
				var candidates []string
				for _, node := range e.project.Nodes {
					if node.ID == plan.failedNode || state.Nodes[node.ID].Failed {
						continue
					}
					if !allowedOn(e.project, plan.tenantID, node.ID) {
						continue
					}
					if state.DomainInSubtree(failedNode.FaultDomain, state.Nodes[node.ID].FaultDomain) {
						continue
					}
					candidates = append(candidates, node.ID)
				}
				sort.Strings(candidates)
				results <- result{index: index, candidates: candidates, nanos: time.Since(start).Nanoseconds()}
			}
		}()
	}
	go func() {
		for index := range plans {
			jobs <- index
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()
	out := make([][]string, len(plans))
	nanos := make(map[string]int64)
	for item := range results {
		out[item.index] = item.candidates
		nanos[plans[item.index].tenantID] += item.nanos
	}
	ids := make([]string, 0, len(nanos))
	for id := range nanos {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		e.stats.addTiming(id, nanos[id])
	}
	return out
}
