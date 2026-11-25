package scheduler

import (
	"sync"
	"sync/atomic"

	"grayrelease/internal/engine"
	"grayrelease/internal/model"
)

const (
	CodeWorkerBudgetExhausted = "E_WORKER_BUDGET_EXHAUSTED"
)

type Backpressure struct {
	Index     int    `json:"index"`
	Tick      int    `json:"tick"`
	RequestID string `json:"request_id"`
	Service   string `json:"service"`
	Code      string `json:"code"`
	Reason    string `json:"reason"`
}

type Output struct {
	Decisions    []engine.Decision `json:"decisions"`
	Backpressure []Backpressure    `json:"backpressure,omitempty"`
	WorkerBudget int               `json:"worker_budget"`
	PeakActive   int32             `json:"peak_active"`
	Processed    int               `json:"processed"`
}

type Options struct {
	RejectOnWorkerBusy bool
}

type Gate struct {
	slots chan struct{}
}

func NewGate(budget int) (*Gate, error) {
	if budget <= 0 {
		return nil, model.NewError(engine.CodeWorkerBudget, "worker budget must be positive").With("worker_budget", budget)
	}
	return &Gate{slots: make(chan struct{}, budget)}, nil
}

func (g *Gate) TryAcquire() bool {
	select {
	case g.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (g *Gate) Release() {
	<-g.slots
}

func (g *Gate) Acquire() {
	g.slots <- struct{}{}
}

func RouteParallel(project *model.Project, requests []model.Request, config engine.Config) (*Output, error) {
	return RouteParallelWithOptions(project, requests, config, Options{})
}

// RouteParallelByTick applies the deterministic admission barrier independently
// for every logical tick. Callers should normally use this for live traffic.
func RouteParallelByTick(project *model.Project, requests []model.Request, config engine.Config, options Options) (*Output, error) {
	ticks := map[int]bool{}
	for _, req := range requests {
		ticks[req.Tick] = true
	}
	orderedTicks := make([]int, 0, len(ticks))
	for tick := range ticks {
		orderedTicks = append(orderedTicks, tick)
	}
	sortInts(orderedTicks)

	combined := &Output{WorkerBudget: config.WorkerBudget}
	for _, tick := range orderedTicks {
		batch := make([]model.Request, 0)
		for _, req := range requests {
			if req.Tick == tick {
				batch = append(batch, req)
			}
		}
		output, err := RouteParallelWithOptions(project, batch, config, options)
		if err != nil {
			return nil, err
		}
		combined.Decisions = append(combined.Decisions, output.Decisions...)
		combined.Backpressure = append(combined.Backpressure, output.Backpressure...)
		combined.Processed += output.Processed
		if output.PeakActive > combined.PeakActive {
			combined.PeakActive = output.PeakActive
		}
	}
	return combined, nil
}

func RouteParallelWithOptions(project *model.Project, requests []model.Request, config engine.Config, options Options) (*Output, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	output := &Output{WorkerBudget: config.WorkerBudget}
	admitted := make([]int, 0, len(requests))
	seenByService := map[string]int{}
	activeWorkersAtBarrier := 0

	for index, req := range requests {
		seen := seenByService[req.Service]
		seenByService[req.Service] = seen + 1
		if seen >= config.RouteBufferPerService {
			reason := "per-service logical-tick route buffer exhausted; buffer=" + itoa(config.RouteBufferPerService)
			code := engine.CodeRouteBufferFull
			if activeWorkersAtBarrier >= config.WorkerBudget || len(admitted) >= config.WorkerBudget {
				reason += ";worker_budget_exhausted=" + itoa(config.WorkerBudget)
			}
			output.Backpressure = append(output.Backpressure, Backpressure{
				Index: index, Tick: req.Tick, RequestID: req.RequestID, Service: req.Service,
				Code: code, Reason: reason,
			})
			continue
		}
		admitted = append(admitted, index)
		if len(admitted) <= config.WorkerBudget {
			activeWorkersAtBarrier = len(admitted)
		} else {
			activeWorkersAtBarrier = config.WorkerBudget
		}
	}

	if options.RejectOnWorkerBusy && len(admitted) > config.WorkerBudget {
		excess := admitted[config.WorkerBudget:]
		admitted = admitted[:config.WorkerBudget]
		for _, index := range excess {
			req := requests[index]
			output.Backpressure = append(output.Backpressure, Backpressure{
				Index: index, Tick: req.Tick, RequestID: req.RequestID, Service: req.Service,
				Code:   CodeWorkerBudgetExhausted,
				Reason: "all routing workers are busy and reject-on-worker-busy is enabled",
			})
		}
	}

	results := make([]engine.Decision, len(requests))
	ok := make([]bool, len(requests))
	slots := make(chan struct{}, config.WorkerBudget)
	jobs := make(chan int)
	var wg sync.WaitGroup
	var active int32
	var peak int32
	var runErr error
	var errMu sync.Mutex

	workerCount := config.WorkerBudget
	if workerCount > len(admitted) {
		workerCount = len(admitted)
	}
	for worker := 0; worker < workerCount; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				slots <- struct{}{}
				current := atomic.AddInt32(&active, 1)
				updatePeak(&peak, current)
				decision, err := engine.Route(project, requests[index])
				if err != nil {
					errMu.Lock()
					if runErr == nil {
						runErr = err
					}
					errMu.Unlock()
				} else {
					results[index] = decision
					ok[index] = true
				}
				atomic.AddInt32(&active, -1)
				<-slots
			}
		}()
	}
	for _, index := range admitted {
		jobs <- index
	}
	close(jobs)
	wg.Wait()

	if runErr != nil {
		return nil, runErr
	}
	output.PeakActive = atomic.LoadInt32(&peak)
	for index := range requests {
		if ok[index] {
			output.Decisions = append(output.Decisions, results[index])
			output.Processed++
		}
	}
	return output, nil
}

func updatePeak(peak *int32, current int32) {
	for {
		old := atomic.LoadInt32(peak)
		if current <= old || atomic.CompareAndSwapInt32(peak, old, current) {
			return
		}
	}
}

func sortInts(values []int) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j-1] > values[j]; j-- {
			values[j-1], values[j] = values[j], values[j-1]
		}
	}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var buf [20]byte
	pos := len(buf)
	for value > 0 {
		pos--
		buf[pos] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
