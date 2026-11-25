package plancache

import (
	"sort"
	"sync"
	"time"
)

type SessionMetrics struct {
	SessionID          string  `json:"session_id"`
	RequestCount       int     `json:"request_count"`
	TotalElapsedMicros int64   `json:"total_elapsed_micros"`
	ObservationsMicros []int64 `json:"observations_micros"`
}

type PipelineReport struct {
	KeyResults           []CacheKeyResult       `json:"key_results"`
	RequestReports       []RequestReport        `json:"request_reports"`
	Closures             []ClosureReport        `json:"closures"`
	InvalidationProgress []InvalidationProgress `json:"invalidation_progress,omitempty"`
	Metrics              []SessionMetrics       `json:"session_metrics"`
	Workers              int                    `json:"workers"`
	SessionBuffer        int                    `json:"session_buffer"`
}

// RuntimeGuard exposes the exact worker and session-buffer accounting used by
// the parallel runner. Try* methods never grow a hidden queue.
type RuntimeGuard struct {
	workerTokens    chan struct{}
	workerBudget    int
	sessionCapacity int
	mu              sync.Mutex
	sessionSlots    map[string]chan struct{}
}

func NewRuntimeGuard(workers, sessionBuffer int) (*RuntimeGuard, error) {
	if workers <= 0 {
		return nil, withContext(newError(CodeBudgetWorkersInvalid, "runtime.workers", "worker budget must be positive"), "workers", workers)
	}
	if sessionBuffer <= 0 {
		return nil, withContext(newError(CodeBudgetBufferInvalid, "runtime.session_buffer", "session buffer must be positive"), "session_buffer", sessionBuffer)
	}
	return &RuntimeGuard{
		workerTokens:    make(chan struct{}, workers),
		workerBudget:    workers,
		sessionCapacity: sessionBuffer,
		sessionSlots:    map[string]chan struct{}{},
	}, nil
}

func NewRuntimeGuardFromProject(p *Project) (*RuntimeGuard, error) {
	if p == nil {
		return nil, newError(CodeSchemaMissingField, "$", "project is nil")
	}
	return NewRuntimeGuard(p.Runtime.Workers, p.Runtime.SessionBuffer)
}

// TryAcquireWorker returns a release callback or E_BACKPRESSURE_WORKERS.
func (g *RuntimeGuard) TryAcquireWorker() (func(), error) {
	select {
	case g.workerTokens <- struct{}{}:
		return func() { <-g.workerTokens }, nil
	default:
		return nil, withContext(newError(CodeBackpressureWorkers, "runtime.workers", "all declared workers are busy"), "worker_budget", g.workerBudget)
	}
}

func (g *RuntimeGuard) sessionChan(sessionID string) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	ch, ok := g.sessionSlots[sessionID]
	if !ok {
		ch = make(chan struct{}, g.sessionCapacity)
		g.sessionSlots[sessionID] = ch
	}
	return ch
}

// TryReserveSession reserves one bounded per-session buffer slot.
func (g *RuntimeGuard) TryReserveSession(sessionID string) (func(), error) {
	ch := g.sessionChan(sessionID)
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	default:
		return nil, withContext(newError(CodeBackpressureBuffer, "runtime.session_buffer", "per-session request buffer is full"), "session_id", sessionID, "buffer_capacity", g.sessionCapacity)
	}
}

// ExecuteBounded runs fn only when both a session slot and worker are
// available. It is primarily an observable backpressure API for injection
// tests; PipelineRunner uses ordered commits for deterministic outcomes.
func ExecuteBounded(g *RuntimeGuard, sessionID string, fn func() error) error {
	releaseSession, err := g.TryReserveSession(sessionID)
	if err != nil {
		return err
	}
	releaseWorker, err := g.TryAcquireWorker()
	if err != nil {
		releaseSession()
		return err
	}
	defer releaseWorker()
	defer releaseSession()
	return fn()
}

type metricAccumulator struct {
	mu      sync.Mutex
	metrics map[string]*SessionMetrics
}

func newMetricAccumulator() *metricAccumulator {
	return &metricAccumulator{metrics: map[string]*SessionMetrics{}}
}

func (m *metricAccumulator) observe(sessionID string, elapsed time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	item, ok := m.metrics[sessionID]
	if !ok {
		item = &SessionMetrics{SessionID: sessionID, ObservationsMicros: []int64{}}
		m.metrics[sessionID] = item
	}
	micros := elapsed.Microseconds()
	item.RequestCount++
	item.TotalElapsedMicros += micros
	item.ObservationsMicros = append(item.ObservationsMicros, micros)
}

func (m *metricAccumulator) sorted() []SessionMetrics {
	m.mu.Lock()
	out := make([]SessionMetrics, 0, len(m.metrics))
	for _, v := range m.metrics {
		cp := *v
		cp.ObservationsMicros = append([]int64(nil), v.ObservationsMicros...)
		out = append(out, cp)
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out
}

// RunParallelPipeline fans out key derivation, processes requests in global
// logical sequence order, and fans out invalidation event solving. Goroutine
// scheduling therefore cannot alter keys, reuse decisions or closures.
func RunParallelPipeline(p *Project) (*PipelineReport, *Engine, error) {
	guard, err := NewRuntimeGuardFromProject(p)
	if err != nil {
		return nil, nil, err
	}
	if err := ValidateProject(p); err != nil {
		return nil, nil, err
	}

	keyResults, err := deriveKeysParallel(p, guard.workerBudget)
	if err != nil {
		return nil, nil, err
	}
	engine, err := NewEngine(p)
	if err != nil {
		return nil, nil, err
	}
	reqs := append([]SessionRequest(nil), p.Requests...)
	sort.Slice(reqs, func(i, j int) bool { return reqs[i].Seq < reqs[j].Seq })

	metrics := newMetricAccumulator()
	reports := make([]RequestReport, len(reqs))
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	turns := make([]chan struct{}, len(reqs))
	for i := range turns {
		turns[i] = make(chan struct{})
	}
	if len(turns) > 0 {
		close(turns[0])
	}
	workerSem := make(chan struct{}, guard.workerBudget)

	for i, req := range reqs {
		releaseSession, berr := guard.TryReserveSession(req.SessionID)
		if berr != nil {
			return nil, nil, berr
		}
		wg.Add(1)
		go func(order int, req SessionRequest, releaseSession func()) {
			defer wg.Done()
			defer releaseSession()
			start := time.Now()
			<-turns[order]
			workerSem <- struct{}{}
			report, perr := engine.ProcessRequest(req)
			<-workerSem
			if perr == nil && report != nil {
				reports[order] = *report
			} else if perr != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = perr
				}
				errMu.Unlock()
			}
			if order+1 < len(turns) {
				close(turns[order+1])
			}
			metrics.observe(req.SessionID, time.Since(start))
		}(i, req, releaseSession)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, engine, firstErr
	}

	closures, err := SolveInvalidationsParallel(p, engine.ActiveEntries(), engine.AllEntriesMap(), guard.workerBudget)
	if err != nil {
		return nil, engine, err
	}
	progress := engine.ApplyClosure(closures)
	return &PipelineReport{
		KeyResults: keyResults, RequestReports: reports, Closures: closures,
		InvalidationProgress: progress, Metrics: metrics.sorted(),
		Workers: p.Runtime.Workers, SessionBuffer: p.Runtime.SessionBuffer,
	}, engine, nil
}

func deriveKeysParallel(p *Project, workers int) ([]CacheKeyResult, error) {
	stmts := append([]Statement(nil), p.Statements...)
	results := make([]CacheKeyResult, len(stmts))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i, st := range stmts {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, st Statement) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = DeriveCacheKey(p, st, p.EffectiveSettings(nil))
		}(i, st)
	}
	wg.Wait()
	sort.Slice(results, func(i, j int) bool { return results[i].StatementID < results[j].StatementID })
	return results, nil
}
