package gait

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

type EngineOptions struct {
	Store         *SnapshotStore
	DisableResume bool
}

type cycleRuntime struct {
	state  CycleState
	output *CycleOutput
}

type Engine struct {
	runMu sync.Mutex

	mu        sync.Mutex
	source    SourceLoop
	phases    PhaseTrack
	truth     Truth
	target    TargetSkeleton
	params    Params
	mapping   Mapping
	inputHash string
	store     *SnapshotStore
	cycles    []cycleRuntime
	status    SnapshotReport
	errors    map[int]error
	paused    atomic.Bool
}

func NewEngine(source SourceLoop, phases PhaseTrack, truth Truth, target TargetSkeleton,
	params Params, options EngineOptions) (*Engine, error) {
	if err := ValidateForRetarget(source, phases, truth, target, params); err != nil {
		return nil, err
	}
	mapping, err := BuildMapping(source, phases, target, params)
	if err != nil {
		return nil, err
	}
	inputDigest, err := inputHash(source, phases, target, params)
	if err != nil {
		return nil, err
	}
	engine := &Engine{
		source:    source,
		phases:    phases,
		truth:     truth,
		target:    target,
		params:    params,
		mapping:   mapping,
		inputHash: inputDigest,
		store:     options.Store,
		cycles:    make([]cycleRuntime, len(source.Cycles)),
		errors:    make(map[int]error),
	}
	for i := range engine.cycles {
		engine.cycles[i].state = CyclePending
	}
	if options.Store != nil && !options.DisableResume {
		loaded, report, err := options.Store.Load(source, target, mapping, inputDigest,
			params.ParameterVersion, params.AlgorithmVersion)
		if err != nil {
			return nil, err
		}
		engine.status = report
		for cycle, output := range loaded {
			engine.cycles[cycle].state = CycleCommitted
			engine.cycles[cycle].output = output
		}
	}
	return engine, nil
}

// TransitionCycle applies only the declared cycle state machine. An illegal
// transition returns E_STATE and leaves the prior state untouched.
func (e *Engine) TransitionCycle(cycle int, to CycleState) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.transitionLocked(cycle, to)
}

func (e *Engine) transitionLocked(cycle int, to CycleState) error {
	if cycle < 0 || cycle >= len(e.cycles) {
		return errCycle(CodeState, fmt.Sprintf("cycle %d is out of range", cycle), cycle)
	}
	from := e.cycles[cycle].state
	if !validTransition(from, to) {
		return errCycle(CodeState, fmt.Sprintf("illegal cycle transition %s -> %s", from, to), cycle)
	}
	e.cycles[cycle].state = to
	return nil
}

func validTransition(from, to CycleState) bool {
	switch from {
	case CyclePending:
		return to == CycleAligning || to == CycleFailed
	case CycleAligning:
		return to == CycleSolving || to == CycleFailed
	case CycleSolving:
		return to == CycleCommitted || to == CycleFailed
	case CycleFailed:
		return to == CycleAligning
	case CycleCommitted:
		return false
	default:
		return false
	}
}

func (e *Engine) StateOf(cycle int) (CycleState, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if cycle < 0 || cycle >= len(e.cycles) {
		return "", errCycle(CodeState, fmt.Sprintf("cycle %d is out of range", cycle), cycle)
	}
	return e.cycles[cycle].state, nil
}

func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	status := Status{
		Total:  len(e.cycles),
		States: make([]CycleState, len(e.cycles)),
		Paused: e.paused.Load(),
	}
	for i := range e.cycles {
		status.States[i] = e.cycles[i].state
		if e.cycles[i].state == CycleCommitted {
			status.Committed++
		}
	}
	return status
}

func (e *Engine) SnapshotReport() SnapshotReport {
	e.mu.Lock()
	defer e.mu.Unlock()
	report := e.status
	report.LoadedCycles = append([]int(nil), e.status.LoadedCycles...)
	return report
}

func (e *Engine) Mapping() Mapping {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.mapping
}

func (e *Engine) Pause()  { e.paused.Store(true) }
func (e *Engine) Resume() { e.paused.Store(false) }

func (e *Engine) UpdateParams(params Params) error {
	e.runMu.Lock()
	defer e.runMu.Unlock()
	if err := ValidateParams(params); err != nil {
		return err
	}
	mapping, err := BuildMapping(e.source, e.phases, e.target, params)
	if err != nil {
		return err
	}
	digest, err := inputHash(e.source, e.phases, e.target, params)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.params = params
	e.mapping = mapping
	e.inputHash = digest
	return nil
}

func (e *Engine) Run(ctx context.Context) error {
	return e.RunCycles(ctx, 0)
}

// RunCycles advances at most maxCycles uncommitted/retryable cycles. A value
// of zero means "until complete or paused".
func (e *Engine) RunCycles(ctx context.Context, maxCycles int) error {
	e.runMu.Lock()
	defer e.runMu.Unlock()
	attempted := 0
	nextCycle := 0
	var firstErr error
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.paused.Load() {
			return firstErr
		}
		if maxCycles > 0 && attempted >= maxCycles {
			return firstErr
		}
		batch := e.nextBatch(nextCycle, maxCycles-attempted)
		if len(batch) == 0 {
			return firstErr
		}
		nextCycle = batch[len(batch)-1] + 1
		attempted += len(batch)
		for _, err := range e.runBatch(ctx, batch) {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
}

func (e *Engine) nextBatch(start, remaining int) []int {
	e.mu.Lock()
	defer e.mu.Unlock()
	batch := make([]int, 0, e.params.MaxParallelism)
	for i := start; i < len(e.cycles) && len(batch) < e.params.MaxParallelism; i++ {
		if remaining > 0 && len(batch) >= remaining {
			break
		}
		if e.cycles[i].state != CycleCommitted {
			batch = append(batch, i)
		}
	}
	return batch
}

func (e *Engine) runBatch(ctx context.Context, batch []int) []error {
	type result struct {
		cycle int
		out   *CycleOutput
		err   error
	}
	results := make([]result, len(batch))
	var wg sync.WaitGroup
	for pos, cycle := range batch {
		wg.Add(1)
		go func(pos, cycle int) {
			defer wg.Done()
			out, err := e.solveOne(ctx, cycle)
			results[pos] = result{cycle: cycle, out: out, err: err}
		}(pos, cycle)
	}
	wg.Wait()
	// Results are committed in cycle order, independent of worker completion.
	errs := make([]error, 0, len(results))
	for _, res := range results {
		e.mu.Lock()
		rt := &e.cycles[res.cycle]
		if res.err != nil {
			if rt.state == CycleSolving {
				_ = e.transitionLocked(res.cycle, CycleFailed)
			}
			e.errors[res.cycle] = res.err
			e.mu.Unlock()
			errs = append(errs, res.err)
			continue
		}
		if e.store != nil {
			env := SnapshotEnvelope{
				SchemaVersion:    CheckpointSchemaVersion,
				SourceID:         e.source.ID,
				TargetID:         e.target.ID,
				Cycle:            res.cycle,
				InputHash:        e.inputHash,
				ParameterVersion: e.params.ParameterVersion,
				AlgorithmVersion: e.params.AlgorithmVersion,
				Payload:          *res.out,
			}
			e.mu.Unlock()
			if err := e.store.Write(env); err != nil {
				e.mu.Lock()
				if rt.state == CycleSolving {
					_ = e.transitionLocked(res.cycle, CycleFailed)
				}
				e.errors[res.cycle] = err
				e.mu.Unlock()
				errs = append(errs, err)
				continue
			}
			e.mu.Lock()
		}
		if rt.state != CycleSolving {
			e.mu.Unlock()
			continue
		}
		if err := e.transitionLocked(res.cycle, CycleCommitted); err != nil {
			e.mu.Unlock()
			errs = append(errs, err)
			continue
		}
		rt.output = res.out
		delete(e.errors, res.cycle)
		e.mu.Unlock()
	}
	return errs
}

func (e *Engine) solveOne(ctx context.Context, cycle int) (*CycleOutput, error) {
	e.mu.Lock()
	rt := &e.cycles[cycle]
	switch rt.state {
	case CycleCommitted:
		out := rt.output
		e.mu.Unlock()
		return out, nil
	case CyclePending, CycleFailed:
		if err := e.transitionLocked(cycle, CycleAligning); err != nil {
			e.mu.Unlock()
			return nil, err
		}
	case CycleAligning:
	default:
		e.mu.Unlock()
		return nil, errCycle(CodeState, fmt.Sprintf("cannot solve from state %s", rt.state), cycle)
	}
	if err := e.transitionLocked(cycle, CycleSolving); err != nil {
		e.mu.Unlock()
		return nil, err
	}
	solveCtx := solveContext{
		source:  e.source,
		phases:  e.phases,
		target:  e.target,
		params:  e.params,
		mapping: e.mapping,
	}
	e.mu.Unlock()
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	return solveCycle(solveCtx, cycle)
}

func ctxErr(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func (e *Engine) Result() (Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	outputs := make([]*CycleOutput, len(e.cycles))
	for i := range e.cycles {
		if e.cycles[i].state != CycleCommitted || e.cycles[i].output == nil {
			return Result{}, errCycle(CodeState, fmt.Sprintf("cycle is %s, result is not complete", e.cycles[i].state), i)
		}
		outputs[i] = cloneCycleOutput(e.cycles[i].output)
	}
	result := assembleResult(e.source, e.target, outputs)
	if err := ValidateEventTimeline(result, e.truth, e.mapping, e.params); err != nil {
		return Result{}, err
	}
	return result, nil
}

func (e *Engine) CycleErrors() map[int]error {
	e.mu.Lock()
	defer e.mu.Unlock()
	copyMap := make(map[int]error, len(e.errors))
	for k, v := range e.errors {
		copyMap[k] = v
	}
	return copyMap
}
