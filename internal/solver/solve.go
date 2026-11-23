package solver

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"roomreverb/internal/errs"
	"roomreverb/internal/fixed"
	"roomreverb/internal/model"
)

type Config struct {
	MaxParallel      int `json:"max_parallel"`
	PathBufferLimit  int `json:"path_buffer_limit"`
	GraphCacheBudget int `json:"graph_cache_budget"`
}

func (c Config) Normalized() Config {
	if c.MaxParallel <= 0 {
		c.MaxParallel = 2
	}
	if c.PathBufferLimit <= 0 {
		c.PathBufferLimit = DefaultPathBufferLimit
	}
	if c.GraphCacheBudget <= 0 {
		c.GraphCacheBudget = c.MaxParallel
	}
	return c
}

type Metrics struct {
	MaxParallel      int             `json:"max_parallel"`
	PathBufferLimit  int             `json:"path_buffer_limit"`
	GraphCacheBudget int             `json:"graph_cache_budget"`
	PeakWorkers      int             `json:"peak_workers"`
	QueueBlocked     int64           `json:"queue_blocked"`
	CacheBudgetWaits int64           `json:"graph_cache_budget_waits"`
	GraphRebuilds    int64           `json:"graph_rebuilds"`
	PathBufferHigh   int             `json:"path_buffer_high_water"`
	Events           []ResourceEvent `json:"resource_events,omitempty"`
}

type ResourceEvent struct {
	Kind   string `json:"kind"`
	RoomID string `json:"room_id,omitempty"`
	Reason string `json:"reason"`
}

type BatchResult struct {
	Rooms    map[string]RoomResult
	Failures map[string]error
	Metrics  Metrics
}

type graphCache struct {
	mu    sync.Mutex
	graph *Graph
}

func (c *graphCache) get(level model.Level, metrics *Metrics) *Graph {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.graph == nil {
		c.graph = BuildGraph(level)
		atomic.AddInt64(&metrics.GraphRebuilds, 1)
	}
	return c.graph
}

// SolveRooms parallelizes room-local path and acoustic work while preserving
// deterministic output. A bounded job queue, a graph-cache permit budget and a
// per-worker path buffer limit enforce the concurrency boundaries.
func SolveRooms(ctx context.Context, level model.Level, roomIDs []string, cfg Config) BatchResult {
	cfg = cfg.Normalized()
	out := BatchResult{
		Rooms:    make(map[string]RoomResult, len(roomIDs)),
		Failures: make(map[string]error),
		Metrics: Metrics{
			MaxParallel: cfg.MaxParallel, PathBufferLimit: cfg.PathBufferLimit,
			GraphCacheBudget: cfg.GraphCacheBudget,
		},
	}
	ids := append([]string(nil), roomIDs...)
	sort.Strings(ids)
	if len(ids) == 0 {
		return out
	}
	cache := &graphCache{}
	graphPermits := make(chan struct{}, cfg.GraphCacheBudget)
	jobs := make(chan string)
	var mu sync.Mutex
	var wg sync.WaitGroup
	var running int32
	var peak int32
	workers := cfg.MaxParallel
	if workers > len(ids) {
		workers = len(ids)
	}
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for roomID := range jobs {
				cur := atomic.AddInt32(&running, 1)
				for {
					old := atomic.LoadInt32(&peak)
					if cur <= old || atomic.CompareAndSwapInt32(&peak, old, cur) {
						break
					}
				}
				select {
				case graphPermits <- struct{}{}:
				default:
					atomic.AddInt64(&out.Metrics.CacheBudgetWaits, 1)
					mu.Lock()
					out.Metrics.Events = append(out.Metrics.Events, ResourceEvent{
						Kind: "graph_cache", RoomID: roomID, Reason: "graph cache budget exhausted; worker blocked",
					})
					mu.Unlock()
					select {
					case graphPermits <- struct{}{}:
					case <-ctx.Done():
						atomic.AddInt32(&running, -1)
						mu.Lock()
						out.Failures[roomID] = &errs.Error{Code: errs.ECanceled, Room: roomID, Message: "canceled while waiting for graph cache budget"}
						mu.Unlock()
						continue
					}
				}
				g := cache.get(level, &out.Metrics)
				result, err := SolveRoom(ctx, level, g, roomID, cfg.PathBufferLimit)
				<-graphPermits
				atomic.AddInt32(&running, -1)
				mu.Lock()
				if err != nil {
					out.Failures[roomID] = err
				}
				if result != nil {
					out.Rooms[roomID] = *result
					for _, p := range result.Pairs {
						if len(p.RoomPath) > out.Metrics.PathBufferHigh {
							out.Metrics.PathBufferHigh = len(p.RoomPath)
						}
					}
				}
				mu.Unlock()
			}
		}()
	}
	for _, id := range ids {
		select {
		case jobs <- id:
		case <-ctx.Done():
			mu.Lock()
			out.Failures[id] = &errs.Error{Code: errs.ECanceled, Room: id, Message: "canceled while queued"}
			mu.Unlock()
			continue
		default:
			atomic.AddInt64(&out.Metrics.QueueBlocked, 1)
			mu.Lock()
			out.Metrics.Events = append(out.Metrics.Events, ResourceEvent{
				Kind: "worker_queue", RoomID: id, Reason: "all workers busy; job queued",
			})
			mu.Unlock()
			select {
			case jobs <- id:
			case <-ctx.Done():
				mu.Lock()
				out.Failures[id] = &errs.Error{Code: errs.ECanceled, Room: id, Message: "canceled while queued"}
				mu.Unlock()
			}
		}
	}
	close(jobs)
	wg.Wait()
	out.Metrics.PeakWorkers = int(atomic.LoadInt32(&peak))
	return out
}

// SolveRoom computes every source/listener path owned by roomID. Listener
// ownership is based on physical room containment, not graph reachability.
func SolveRoom(ctx context.Context, level model.Level, g *Graph, roomID string, pathBufferLimit int) (*RoomResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, &errs.Error{Code: errs.ECanceled, Room: roomID, Message: err.Error(), Wrapped: err}
	}
	room, ok := level.Room(roomID)
	if !ok {
		return nil, &errs.Error{Code: errs.EInput, Room: roomID, Message: "unknown room"}
	}
	neighbors, portalIDs := g.Neighbors(roomID)
	result := &RoomResult{
		RoomID: roomID, Params: room.Params,
		Portals:   make([]PortalRef, 0, len(g.Adj[roomID])),
		Neighbors: neighbors, PortalIDs: portalIDs,
		AlgorithmVersion: model.AlgorithmVersion, Precision: model.Precision,
	}
	for _, p := range g.Portals {
		if len(p.Rooms) == 2 && (p.Rooms[0] == roomID || p.Rooms[1] == roomID) {
			result.Portals = append(result.Portals, p.Clone())
		}
	}
	var listeners []model.Listener
	for _, l := range level.Listeners {
		if g.RoomOf[l.ID] == roomID {
			listeners = append(listeners, l)
		}
	}
	sources := append([]model.Source(nil), level.Sources...)
	sort.Slice(listeners, func(i, j int) bool { return listeners[i].ID < listeners[j].ID })
	sort.Slice(sources, func(i, j int) bool { return sources[i].ID < sources[j].ID })
	var firstErr error
	for _, l := range listeners {
		for _, s := range sources {
			if err := ctx.Err(); err != nil {
				return result, &errs.Error{Code: errs.ECanceled, Room: roomID, Listener: l.ID, Source: s.ID, Message: err.Error(), Wrapped: err}
			}
			found, err := ShortestPath(g, s.ID, l.ID, pathBufferLimit)
			result.Stats.Pairs++
			pr := PairResult{SourceID: s.ID, ListenerID: l.ID}
			if err != nil {
				pr.Reachable = false
				pr.ErrorCode = string(errs.CodeOf(err))
				pr.ErrorMessage = err.Error()
				result.Stats.Unreachable++
				if firstErr == nil {
					firstErr = err
				}
				result.Pairs = append(result.Pairs, pr)
				continue
			}
			est := Estimate(level, found.Rooms, found.Length)
			pr.Reachable = true
			pr.RoomPath = append([]string{}, found.Rooms...)
			pr.PortalPath = append([]string{}, found.Portals...)
			pr.PathFixed = int64(found.Length)
			pr.DecayFixed = int64(est.Decay)
			pr.EnergyFixed = int64(est.Energy)
			FillPairNumbers(&pr)
			result.Stats.Reachable++
			result.Stats.DecaySumFixed += int64(est.Decay)
			result.Stats.EnergySumFixed += int64(est.Energy)
			result.Pairs = append(result.Pairs, pr)
		}
	}
	if firstErr != nil {
		return result, firstErr
	}
	return result, nil
}

// AverageDecayMS is a fixed-point listener statistic used by three-way diffs.
func (r RoomResult) AverageDecayMS() float64 {
	if r.Stats.Reachable == 0 {
		return 0
	}
	return fixed.Value(r.Stats.DecaySumFixed / int64(r.Stats.Reachable)).Float()
}

func (r RoomResult) AverageEnergy() float64 {
	if r.Stats.Reachable == 0 {
		return 0
	}
	return fixed.Value(r.Stats.EnergySumFixed / int64(r.Stats.Reachable)).Float()
}

func (r RoomResult) String() string { return fmt.Sprintf("room result %s", r.RoomID) }
