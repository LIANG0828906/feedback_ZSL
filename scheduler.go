package fog

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

type SchedulerConfig struct {
	MaxParallelTiles   int
	RayBufferPerWorker int
	ShadowBudgetTotal  int64
}

type QueueRecord struct {
	Tile       TileCoord `json:"tile"`
	Reason     string    `json:"reason"`
	Detail     string    `json:"detail,omitempty"`
	EnqueuedAt time.Time `json:"enqueued_at"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	WaitMS     int64     `json:"wait_ms"`
}

type ResourceSnapshot struct {
	MaxParallelTiles   int           `json:"max_parallel_tiles"`
	ActiveTiles        int           `json:"active_tiles"`
	MaxActiveTiles     int           `json:"max_active_tiles"`
	RayBufferCapacity  int           `json:"ray_buffer_capacity"`
	RayBufferHighWater int           `json:"ray_buffer_high_water"`
	ShadowTotal        int64         `json:"shadow_total"`
	ShadowConsumed     int64         `json:"shadow_consumed"`
	ShadowReserved     int64         `json:"shadow_reserved"`
	ShadowAvailable    int64         `json:"shadow_available"`
	Queues             []QueueRecord `json:"queues"`
}

type SchedulerReport struct {
	Tiles    []TileResult      `json:"tiles"`
	Failures map[string]*Error `json:"failures,omitempty"`
	Queue    []QueueRecord     `json:"queue"`
	Resource ResourceSnapshot  `json:"resource"`
}

type FaultController struct {
	mu        sync.Mutex
	failOnce  map[string]int
	permanent map[string]bool
}

func NewFaultController() *FaultController {
	return &FaultController{failOnce: map[string]int{}, permanent: map[string]bool{}}
}

func tileKey(tile TileCoord) string {
	return fmt.Sprintf("%d,%d", tile.X, tile.Y)
}

func (f *FaultController) FailNext(tile TileCoord, times int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOnce[tileKey(tile)] = times
}

func (f *FaultController) FailAlways(tile TileCoord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.permanent[tileKey(tile)] = true
}

func (f *FaultController) Clear(tile TileCoord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.failOnce, tileKey(tile))
	delete(f.permanent, tileKey(tile))
}

func (f *FaultController) MaybeFail(tile TileCoord) error {
	f.mu.Lock()
	key := tileKey(tile)
	if f.permanent[key] {
		f.mu.Unlock()
		return blockError(tile, "injected permanent block failure")
	}
	if f.failOnce[key] > 0 {
		f.failOnce[key]--
		if f.failOnce[key] == 0 {
			delete(f.failOnce, key)
		}
		f.mu.Unlock()
		return blockError(tile, "injected internal block failure")
	}
	f.mu.Unlock()
	return nil
}

func blockError(tile TileCoord, message string) *Error {
	e := newError(CodeBlock, "%s", message)
	e.Tile = &tile
	e.Field = "tile.internal"
	e.Value = tile
	return e
}

type Scheduler struct {
	Config SchedulerConfig
	Faults *FaultController

	mu        sync.Mutex
	active    int
	maxActive int
	bufferHi  int
	queue     []QueueRecord
	shadow    *shadowGate
}

type shadowGate struct {
	mu       sync.Mutex
	cond     *sync.Cond
	total    int64
	consumed int64
	reserved int64
}

func newShadowGate(total int64) *shadowGate {
	g := &shadowGate{total: total}
	g.cond = sync.NewCond(&g.mu)
	return g
}

func (g *shadowGate) available() int64 {
	return g.total - g.consumed - g.reserved
}

func (g *shadowGate) grant(amount int64) {
	g.mu.Lock()
	g.total += amount
	g.cond.Broadcast()
	g.mu.Unlock()
}

func (g *shadowGate) reserve(ctx context.Context, amount int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for g.available() < amount {
		done := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				g.cond.Broadcast()
			case <-done:
			}
		}()
		g.cond.Wait()
		close(done)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	g.reserved += amount
	return nil
}

func (g *shadowGate) finish(maxReserve, actual int64) {
	g.mu.Lock()
	g.reserved -= maxReserve
	if g.reserved < 0 {
		g.reserved = 0
	}
	g.consumed += actual
	g.cond.Broadcast()
	g.mu.Unlock()
}

func NewScheduler(config SchedulerConfig) *Scheduler {
	if config.MaxParallelTiles <= 0 {
		config.MaxParallelTiles = 2
	}
	if config.RayBufferPerWorker <= 0 {
		config.RayBufferPerWorker = 64
	}
	if config.ShadowBudgetTotal <= 0 {
		config.ShadowBudgetTotal = 1 << 62
	}
	return &Scheduler{Config: config, Faults: NewFaultController(), shadow: newShadowGate(config.ShadowBudgetTotal)}
}

func (s *Scheduler) GrantShadowBudget(amount int64) {
	s.shadow.grant(amount)
}

func (s *Scheduler) snapshot() ResourceSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shadow.mu.Lock()
	defer s.shadow.mu.Unlock()
	queue := append([]QueueRecord(nil), s.queue...)
	return ResourceSnapshot{
		MaxParallelTiles:   s.Config.MaxParallelTiles,
		ActiveTiles:        s.active,
		MaxActiveTiles:     s.maxActive,
		RayBufferCapacity:  s.Config.RayBufferPerWorker,
		RayBufferHighWater: s.bufferHi,
		ShadowTotal:        s.shadow.total,
		ShadowConsumed:     s.shadow.consumed,
		ShadowReserved:     s.shadow.reserved,
		ShadowAvailable:    s.shadow.available(),
		Queues:             queue,
	}
}

func (s *Scheduler) addRecord(record QueueRecord) {
	s.mu.Lock()
	s.queue = append(s.queue, record)
	s.mu.Unlock()
}

func (s *Scheduler) Run(ctx context.Context, scene *Scene, tiles []TileCoord, options IntegrationOptions) SchedulerReport {
	if ctx == nil {
		ctx = context.Background()
	}
	semaphore := make(chan struct{}, s.Config.MaxParallelTiles)
	var wg sync.WaitGroup
	var resultMu sync.Mutex
	results := map[string]TileResult{}
	failures := map[string]*Error{}

	for _, tile := range tiles {
		tile := tile
		wg.Add(1)
		go func() {
			defer wg.Done()
			queued := time.Now()
			semaphore <- struct{}{}
			slotWait := time.Since(queued)
			record := QueueRecord{Tile: tile, Reason: "max_parallel_tiles", EnqueuedAt: queued, StartedAt: time.Now(), WaitMS: slotWait.Milliseconds()}
			if slotWait > 0 {
				record.Detail = fmt.Sprintf("waited for one of %d tile workers", s.Config.MaxParallelTiles)
			}
			s.addRecord(record)
			defer func() { <-semaphore }()

			s.mu.Lock()
			s.active++
			if s.active > s.maxActive {
				s.maxActive = s.active
			}
			s.mu.Unlock()
			defer func() {
				s.mu.Lock()
				s.active--
				s.mu.Unlock()
			}()

			rays := RaysForTile(*scene, tile)
			bufferLimit := s.Config.RayBufferPerWorker
			chunkCount := (len(rays) + bufferLimit - 1) / bufferLimit
			if chunkCount > 1 {
				s.addRecord(QueueRecord{Tile: tile, Reason: "ray_buffer_chunked", Detail: fmt.Sprintf("%d rays split into %d buffer batches of %d", len(rays), chunkCount, bufferLimit), EnqueuedAt: time.Now(), StartedAt: time.Now()})
			}

			opts := options.withRecipe(scene.Recipe)
			maxShadow := int64(len(rays) * opts.ShadowBudget)
			shadowQueued := time.Now()
			if err := s.shadow.reserve(ctx, maxShadow); err != nil {
				e := newError(CodeIter, "shadow budget queue wait failed: %v", err)
				e.Tile = &tile
				resultMu.Lock()
				failures[tileKey(tile)] = e
				resultMu.Unlock()
				return
			}
			shadowWait := time.Since(shadowQueued)
			if shadowWait > time.Millisecond {
				s.addRecord(QueueRecord{Tile: tile, Reason: "shadow_budget_exhausted", Detail: fmt.Sprintf("reserved %d worst-case shadow samples", maxShadow), EnqueuedAt: shadowQueued, StartedAt: time.Now(), WaitMS: shadowWait.Milliseconds()})
			}

			result, err := RenderTile(scene, tile, TileRenderConfig{
				Options:        opts,
				RayBufferLimit: bufferLimit,
				FailInjection:  s.Faults.MaybeFail,
			})
			s.shadow.finish(maxShadow, int64(resultShadow(err, result)))
			if err != nil {
				e := AsError(err)
				if e == nil {
					e = newError(CodeBlock, "%v", err)
					e.Tile = &tile
				}
				if e.Tile == nil {
					e.Tile = &tile
				}
				resultMu.Lock()
				failures[tileKey(tile)] = e
				resultMu.Unlock()
				return
			}

			s.mu.Lock()
			if result.MaxRayBufferUsed > s.bufferHi {
				s.bufferHi = result.MaxRayBufferUsed
			}
			s.mu.Unlock()
			resultMu.Lock()
			results[tileKey(tile)] = *result
			resultMu.Unlock()
		}()
	}
	wg.Wait()

	ordered := make([]TileResult, 0, len(results))
	for _, tile := range tiles {
		if result, ok := results[tileKey(tile)]; ok {
			ordered = append(ordered, result)
		}
	}
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].Tile.Y < ordered[j].Tile.Y || (ordered[i].Tile.Y == ordered[j].Tile.Y && ordered[i].Tile.X < ordered[j].Tile.X)
	})
	return SchedulerReport{Tiles: ordered, Failures: failures, Queue: append([]QueueRecord(nil), s.queue...), Resource: s.snapshot()}
}

func resultShadow(err error, result *TileResult) int {
	if err != nil || result == nil {
		return 0
	}
	return result.ShadowSamples
}

func OrderedTiles(tiles []TileResult) []TileResult {
	out := append([]TileResult(nil), tiles...)
	sort.Slice(out, func(i, j int) bool {
		return out[i].Tile.Y < out[j].Tile.Y || (out[i].Tile.Y == out[j].Tile.Y && out[i].Tile.X < out[j].Tile.X)
	})
	for i := range out {
		out[i].CommitOrder = i
	}
	return out
}
