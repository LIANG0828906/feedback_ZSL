package bake

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"terrainkernel/internal/kerr"
	"terrainkernel/internal/model"
)

type Committer interface {
	Commit(chunkID int, result *ChunkResult, dependencies []string) error
}

type CommitterFunc func(chunkID int, result *ChunkResult, dependencies []string) error

func (f CommitterFunc) Commit(chunkID int, result *ChunkResult, dependencies []string) error {
	return f(chunkID, result, dependencies)
}

type EngineConfig struct {
	MaxWorkers     int
	BufferBudgetPx int
	Precise        bool
	StopAfter      int
}

type QueueEvent struct {
	ChunkID int
	Reason  string
}

type ChunkFailure struct {
	ChunkID int
	Err     error
}

func (f *ChunkFailure) Error() string {
	if f == nil {
		return "<nil>"
	}
	return fmt.Sprintf("chunk %d failed: %v", f.ChunkID, f.Err)
}

type RunReport struct {
	Results        map[int]*ChunkResult
	QueueEvents    []QueueEvent
	Failures       map[int]*ChunkFailure
	CommitSequence []int
	Reused         []int
	Paused         bool
}

type Engine struct {
	project   *model.Project
	graph     *Graph
	cfg       EngineConfig
	committer Committer

	mu          sync.Mutex
	paused      bool
	resumeCh    chan struct{}
	queueEvents []QueueEvent
	failures    map[int]*ChunkFailure
	injectOnce  map[int]string
	budgetToken *tokenPool
}

func NewEngine(p *model.Project, graph *Graph, cfg EngineConfig, committer Committer) (*Engine, error) {
	if p == nil {
		return nil, kerr.New(kerr.CodeSchema, "project", "nil project")
	}
	if cfg.MaxWorkers <= 0 {
		cfg.MaxWorkers = 1
	}
	if cfg.BufferBudgetPx <= 0 {
		cfg.BufferBudgetPx = p.ChunkSizePx * p.ChunkSizePx
		if cfg.BufferBudgetPx <= 0 {
			cfg.BufferBudgetPx = 1
		}
	}
	if graph == nil {
		graph = BuildGraph(p)
	}
	resume := make(chan struct{})
	close(resume)
	return &Engine{
		project:     p,
		graph:       graph,
		cfg:         cfg,
		committer:   committer,
		resumeCh:    resume,
		failures:    make(map[int]*ChunkFailure),
		injectOnce:  make(map[int]string),
		budgetToken: newTokenPool(cfg.BufferBudgetPx),
	}, nil
}

func (e *Engine) Config() EngineConfig {
	return e.cfg
}

func (e *Engine) Pause() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.paused {
		return
	}
	e.paused = true
	e.resumeCh = make(chan struct{})
}

func (e *Engine) Resume() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.paused {
		return
	}
	e.paused = false
	close(e.resumeCh)
}

func (e *Engine) InjectFailure(chunkID int, message string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.injectOnce[chunkID] = message
}

func (e *Engine) waitIfPaused(ctx context.Context) error {
	e.mu.Lock()
	if !e.paused {
		e.mu.Unlock()
		return nil
	}
	ch := e.resumeCh
	e.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ch:
		return nil
	}
}

func (e *Engine) recordQueue(chunkID int, reason string) {
	e.mu.Lock()
	e.queueEvents = append(e.queueEvents, QueueEvent{ChunkID: chunkID, Reason: reason})
	e.mu.Unlock()
}

func (e *Engine) consumeInjectedFailure(chunkID int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if msg, ok := e.injectOnce[chunkID]; ok {
		delete(e.injectOnce, chunkID)
		return kerr.New(kerr.CodeSchema, fmt.Sprintf("chunk[%d]", chunkID), msg)
	}
	return nil
}

type jobResult struct {
	chunkID int
	result  *ChunkResult
	err     error
}

// Run bakes the requested chunks, reuses values from cache, and commits
// successful chunks in ascending chunk-id order. A failure is isolated to its
// chunk and can be retried by invoking Run again.
func (e *Engine) Run(ctx context.Context, chunks []int, cache map[int]*ChunkResult) (*RunReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	report := &RunReport{
		Results:  make(map[int]*ChunkResult),
		Failures: make(map[int]*ChunkFailure),
	}
	e.mu.Lock()
	e.queueEvents = nil
	e.mu.Unlock()
	requested := normalizeChunkList(e.project, chunks)
	explicit := len(chunks) > 0
	requestedSet := make(map[int]struct{}, len(requested))
	for _, id := range requested {
		requestedSet[id] = struct{}{}
	}
	for id, result := range cache {
		if id >= 0 && id < e.project.ChunkCount() && result != nil {
			report.Results[id] = result
			if !explicit {
				report.Reused = append(report.Reused, id)
			} else if _, force := requestedSet[id]; !force {
				report.Reused = append(report.Reused, id)
			}
		}
	}
	sort.Ints(report.Reused)

	pending := make([]int, 0, len(requested))
	if explicit {
		pending = append(pending, requested...)
	} else {
		for _, id := range requested {
			if _, reused := report.Results[id]; !reused {
				pending = append(pending, id)
			}
		}
	}
	if len(pending) == 0 {
		return report, nil
	}

	e.mu.Lock()
	for _, id := range pending {
		delete(e.failures, id)
	}
	e.mu.Unlock()

	jobs := make(chan int)
	results := make(chan jobResult, len(pending))
	workers := e.cfg.MaxWorkers
	if workers > len(pending) {
		workers = len(pending)
	}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				results <- e.runJob(ctx, id)
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, id := range pending {
			select {
			case <-ctx.Done():
				return
			case jobs <- id:
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	baked := make(map[int]jobResult, len(pending))
	for result := range results {
		baked[result.chunkID] = result
	}
	if err := ctx.Err(); err != nil && len(baked) == 0 {
		return report, err
	}

	committed := 0
	stopAfter := e.cfg.StopAfter
	for _, id := range pending {
		result, ok := baked[id]
		if !ok {
			continue
		}
		if result.err != nil {
			failure := &ChunkFailure{ChunkID: id, Err: result.err}
			report.Failures[id] = failure
			e.mu.Lock()
			e.failures[id] = failure
			e.mu.Unlock()
			continue
		}
		if stopAfter >= 0 && committed >= stopAfter {
			report.Paused = true
			break
		}
		if e.committer != nil {
			deps := []string(nil)
			if e.graph != nil {
				deps = e.graph.ChunkDependencies(id)
			}
			result.result.ParamVersion = ChunkParamVersion(e.project, id, deps)
			if err := e.committer.Commit(id, result.result, deps); err != nil {
				failure := &ChunkFailure{ChunkID: id, Err: err}
				report.Failures[id] = failure
				e.mu.Lock()
				e.failures[id] = failure
				e.mu.Unlock()
				continue
			}
		}
		report.Results[id] = result.result
		report.CommitSequence = append(report.CommitSequence, id)
		committed++
	}
	if err := ctx.Err(); err != nil && len(report.CommitSequence) == 0 {
		return report, err
	}
	sort.Ints(report.CommitSequence)
	e.mu.Lock()
	report.QueueEvents = append([]QueueEvent(nil), e.queueEvents...)
	e.mu.Unlock()
	return report, nil
}

func normalizeChunkList(p *model.Project, chunks []int) []int {
	if len(chunks) == 0 {
		return p.ChunkIDs()
	}
	seen := make(map[int]struct{}, len(chunks))
	out := make([]int, 0, len(chunks))
	for _, id := range chunks {
		if id < 0 || id >= p.ChunkCount() {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}

func (e *Engine) runJob(ctx context.Context, chunkID int) (result jobResult) {
	result.chunkID = chunkID
	defer func() {
		if recovered := recover(); recovered != nil {
			result.err = kerr.New(kerr.CodeSchema, fmt.Sprintf("chunk[%d]", chunkID), fmt.Sprintf("panic isolated: %v", recovered))
		}
	}()
	if err := e.waitIfPaused(ctx); err != nil {
		result.err = err
		return result
	}
	if err := e.consumeInjectedFailure(chunkID); err != nil {
		result.err = err
		return result
	}
	result.result, result.err = e.bakeWithBudget(ctx, chunkID)
	return result
}

func (e *Engine) bakeWithBudget(ctx context.Context, chunkID int) (*ChunkResult, error) {
	originX, originY, ok := e.project.ChunkOrigin(chunkID)
	if !ok {
		return nil, kerr.New(kerr.CodeSchema, "chunk_id", "chunk id out of range")
	}
	size := e.project.ChunkSizePx
	anchors := LayerAnchors(e.project)
	perWorker := e.cfg.BufferBudgetPx / e.cfg.MaxWorkers
	if perWorker <= 0 {
		perWorker = 1
	}
	rowsPerStripe := perWorker / size
	if rowsPerStripe <= 0 {
		rowsPerStripe = 1
	}
	if rowsPerStripe > size {
		rowsPerStripe = size
	}
	tokenCount := rowsPerStripe * size
	if tokenCount <= 0 {
		tokenCount = 1
	}
	result := &ChunkResult{
		ChunkID:    chunkID,
		Size:       size,
		Dominants:  make([]int, 0, size*size),
		Blends:     make([]int64, 0, size*size),
		WeightRows: make([][]int64, 0, size*size),
	}
	for row := 0; row < size; row += rowsPerStripe {
		if err := e.waitIfPaused(ctx); err != nil {
			return nil, err
		}
		rows := rowsPerStripe
		if row+rows > size {
			rows = size - row
		}
		pixels := rows * size
		if pixels > tokenCount {
			pixels = tokenCount
		}
		if pixels > e.cfg.BufferBudgetPx {
			pixels = e.cfg.BufferBudgetPx
		}
		if err := e.acquireToken(ctx, pixels, chunkID); err != nil {
			return nil, err
		}
		for y := row; y < row+rows; y++ {
			for x := 0; x < size; x++ {
				pixel, err := bakePixelWithAnchors(e.project, anchors, originX+x, originY+y)
				if err != nil {
					e.releaseToken(pixels)
					return nil, err
				}
				result.WeightRows = append(result.WeightRows, append([]int64(nil), pixel.Weights...))
				result.Dominants = append(result.Dominants, pixel.Dominant)
				result.Blends = append(result.Blends, pixel.Blend)
			}
		}
		e.releaseToken(pixels)
	}
	if err := result.ComputeHashes(); err != nil {
		return nil, err
	}
	return result, nil
}

func (e *Engine) acquireToken(ctx context.Context, count, chunkID int) error {
	if count <= 0 {
		count = 1
	}
	if count > e.cfg.BufferBudgetPx {
		count = e.cfg.BufferBudgetPx
	}
	if e.budgetToken.tryAcquire(count) {
		return nil
	}
	e.recordQueue(chunkID, "pixel_buffer_budget_exhausted")
	return e.budgetToken.acquire(ctx, count)
}

func (e *Engine) releaseToken(count int) {
	if count <= 0 {
		count = 1
	}
	e.budgetToken.release(count)
}

type tokenPool struct {
	mu       sync.Mutex
	capacity int
	used     int
	notify   chan struct{}
}

func newTokenPool(capacity int) *tokenPool {
	if capacity <= 0 {
		capacity = 1
	}
	return &tokenPool{capacity: capacity, notify: make(chan struct{}, 1)}
}

func (p *tokenPool) tryAcquire(count int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if count > p.capacity {
		count = p.capacity
	}
	if p.used+count > p.capacity {
		return false
	}
	p.used += count
	return true
}

func (p *tokenPool) acquire(ctx context.Context, count int) error {
	if count > p.capacity {
		count = p.capacity
	}
	for {
		p.mu.Lock()
		if p.used+count <= p.capacity {
			p.used += count
			p.mu.Unlock()
			return nil
		}
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.notify:
		}
	}
}

func (p *tokenPool) release(count int) {
	p.mu.Lock()
	p.used -= count
	if p.used < 0 {
		p.used = 0
	}
	p.mu.Unlock()
	select {
	case p.notify <- struct{}{}:
	default:
	}
}

type MemoryCommitter struct {
	mu       sync.Mutex
	Results  map[int]*ChunkResult
	Sequence []int
	Deps     map[int][]string
}

func NewMemoryCommitter() *MemoryCommitter {
	return &MemoryCommitter{
		Results: make(map[int]*ChunkResult),
		Deps:    make(map[int][]string),
	}
}

func (m *MemoryCommitter) Commit(chunkID int, result *ChunkResult, dependencies []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Results[chunkID] = result
	m.Sequence = append(m.Sequence, chunkID)
	m.Deps[chunkID] = append([]string(nil), dependencies...)
	return nil
}

func RunPreview(p *model.Project, cfg EngineConfig) (map[int]*ChunkResult, []int, []QueueEvent, error) {
	committer := NewMemoryCommitter()
	graph := BuildGraph(p)
	engine, err := NewEngine(p, graph, cfg, committer)
	if err != nil {
		return nil, nil, nil, err
	}
	report, err := engine.Run(context.Background(), nil, nil)
	return report.Results, report.CommitSequence, report.QueueEvents, err
}

func RunFull(p *model.Project, cfg EngineConfig, committer Committer) (*RunReport, error) {
	graph := BuildGraph(p)
	engine, err := NewEngine(p, graph, cfg, committer)
	if err != nil {
		return nil, err
	}
	return engine.Run(context.Background(), nil, nil)
}

func RunIncremental(p *model.Project, cache map[int]*ChunkResult, invalidations []Invalidation, cfg EngineConfig, committer Committer) (*RunReport, error) {
	graph := BuildGraph(p)
	engine, err := NewEngine(p, graph, cfg, committer)
	if err != nil {
		return nil, err
	}
	return engine.Run(context.Background(), InvalidationChunks(invalidations), cache)
}

func IncrementalBake(p *model.Project, previous map[int]*ChunkResult, invalidations []Invalidation, cfg EngineConfig) (map[int]*ChunkResult, []int, error) {
	committer := NewMemoryCommitter()
	report, err := RunIncremental(p, previous, invalidations, cfg, committer)
	if err != nil {
		return nil, nil, err
	}
	return report.Results, report.CommitSequence, nil
}
