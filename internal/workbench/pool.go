package workbench

import (
	"context"
	"sort"
	"sync"
)

type zoneTask struct {
	ID   string
	Zone *Zone
}

type zoneTaskResult struct {
	ID     string
	Result *ZoneResult
	Events []BufferEvent
	Err    *Error
}

func (e *Engine) computeZones(ctx context.Context, ids []string) (map[string]*ZoneResult, WorkerStats, *Error) {
	stats := WorkerStats{MaxParallel: e.options.MaxParallel, BufferLimit: e.options.BufferLimit}
	if len(ids) == 0 {
		return map[string]*ZoneResult{}, stats, nil
	}
	parallel := e.options.MaxParallel
	if e.options.Serial {
		parallel = 1
	}
	if parallel < 1 {
		parallel = 1
	}
	if parallel > len(ids) {
		parallel = len(ids)
	}
	stats.MaxParallel = parallel
	topo := make([]string, 0, len(ids))
	for _, id := range e.topology {
		if containsString(ids, id) {
			topo = append(topo, id)
		}
	}
	// Version derivation walks the dependency graph and memoizes. Resolve all
	// versions before starting workers so the worker pool performs read-only
	// model access only.
	versions := e.allParamVersions()
	jobs := make(chan zoneTask)
	results := make(chan zoneTaskResult, len(topo))
	var wg sync.WaitGroup
	for worker := 0; worker < parallel; worker++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for task := range jobs {
				model := buildZoneModel(e.region, e.state.NoCollect, task.Zone)
				estimate, selection, err := e.sampleZone(task.Zone, model)
				if err != nil {
					results <- zoneTaskResult{ID: task.ID, Err: err}
					continue
				}
				events := e.stageSampleBuffer(task.ID, workerID, selection.Indices)
				result, reconstructErr := reconstructZone(
					e.region, e.state.NoCollect, task.Zone, estimate,
					e.baseHash, versions[task.ID], e.zoneOrder[task.ID],
				)
				if reconstructErr != nil {
					results <- zoneTaskResult{ID: task.ID, Err: reconstructErr}
					continue
				}
				result.SampleSelection = selection
				results <- zoneTaskResult{ID: task.ID, Result: result, Events: events}
			}
		}(worker)
	}
	go func() {
		for _, id := range topo {
			zone := findZone(e.region, id)
			jobs <- zoneTask{ID: id, Zone: zone}
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()
	out := make(map[string]*ZoneResult, len(topo))
	eventMap := make(map[string]BufferEvent)
	for item := range results {
		if item.Err != nil {
			return nil, stats, item.Err
		}
		out[item.ID] = item.Result
		for _, event := range item.Events {
			eventMap[event.ZoneID] = event
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, stats, ioError("zone computation cancelled: %v", err)
	}
	for _, id := range topo {
		if out[id] == nil {
			return nil, stats, ioError("worker failed to produce zone %s", id)
		}
		if event, ok := eventMap[id]; ok {
			stats.BufferEvents = append(stats.BufferEvents, event)
		}
	}
	stats.WorkersUsed = parallel
	return out, stats, nil
}

// stageSampleBuffer moves selected sample indices through a bounded channel.
// The producer blocks when the per-worker buffer is full and the worker drains
// it in the consumer loop; this keeps staging memory bounded independently of
// zone sample count.
func (e *Engine) stageSampleBuffer(zoneID string, workerID int, indices []int) []BufferEvent {
	limit := e.options.BufferLimit
	if limit <= 0 {
		limit = 1
	}
	if len(indices) <= limit {
		return nil
	}
	buffer := make(chan int, limit)
	done := make(chan struct{})
	blocked := 0
	go func() {
		defer close(done)
		defer close(buffer)
		for _, index := range indices {
			select {
			case buffer <- index:
			default:
				blocked++
				buffer <- index
			}
		}
	}()
	checksum := 0
	for index := range buffer {
		checksum += index
	}
	<-done
	_ = checksum
	return []BufferEvent{{
		ZoneID:  zoneID,
		Worker:  workerID,
		Reason:  "sample_buffer_exhausted",
		Blocked: blocked > 0,
		Count:   len(indices) - limit,
	}}
}

func sortedZoneIDs(zones []Zone) []string {
	out := make([]string, 0, len(zones))
	for _, zone := range zones {
		out = append(out, zone.ID)
	}
	sort.Strings(out)
	return out
}
