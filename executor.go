package orchestrator

import (
	"context"
	"errors"
	"fmt"
)

var ErrExecutorDone = errors.New("orchestrator: all committed ticks are complete")

type ExecutorOptions struct {
	Resume bool
}

type RunSummary struct {
	ResumedFromTick   int64        `json:"resumed_from_tick"`
	LastCommittedTick int64        `json:"last_committed_tick"`
	Ticks             []TickResult `json:"ticks"`
}

type InterruptedError struct {
	LastCommittedTick int64
}

func (e *InterruptedError) Error() string {
	return fmt.Sprintf("%s: stopped at last complete commit %d", CodeInterrupted, e.LastCommittedTick)
}

func (e *InterruptedError) BusinessCode() Code { return CodeInterrupted }

type Executor struct {
	solver        *Solver
	sim           *simulation
	store         *SQLiteStore
	maxTick       int64
	nextTick      int64
	lastCommitted int64
}

func NewExecutor(ctx context.Context, project *Project, store *SQLiteStore, options ExecutorOptions) (*Executor, error) {
	if store == nil {
		return nil, newError(CodeCheckpoint, "$.store", "checkpoint store is required")
	}
	solver, err := NewSolver(project)
	if err != nil {
		return nil, err
	}
	maxTick, err := newSimulationMaxTick(solver)
	if err != nil {
		return nil, err
	}
	executor := &Executor{
		solver:        solver,
		store:         store,
		maxTick:       maxTick,
		lastCommitted: -1,
		nextTick:      0,
	}
	if !options.Resume {
		sim, err := newSimulation(solver)
		if err != nil {
			return nil, err
		}
		executor.sim = sim
		return executor, nil
	}
	checkpoint, ok, err := store.LatestCheckpoint(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		sim, err := newSimulation(solver)
		if err != nil {
			return nil, err
		}
		executor.sim = sim
		return executor, nil
	}
	if checkpoint.SnapshotID != solver.Project.SnapshotID || checkpoint.Revision != solver.Project.Revision {
		return nil, newError(CodeSnapshotChanged, "$.checkpoint", "persisted snapshot %s@%s does not match %s@%s",
			checkpoint.SnapshotID, checkpoint.Revision, solver.Project.SnapshotID, solver.Project.Revision)
	}
	state, err := stateFromCheckpoint(checkpoint, solver.Project)
	if err != nil {
		return nil, err
	}
	sim, err := newSimulationFromState(solver, state)
	if err != nil {
		return nil, err
	}
	executor.sim = sim
	executor.lastCommitted = checkpoint.Tick
	executor.nextTick = checkpoint.Tick + 1
	return executor, nil
}

func newSimulationMaxTick(solver *Solver) (int64, error) {
	sim, err := newSimulation(solver)
	if err != nil {
		return 0, err
	}
	return sim.maxEventTick()
}

func (e *Executor) Done() bool {
	return e.nextTick > e.maxTick
}

func (e *Executor) LastCommittedTick() int64 {
	return e.lastCommitted
}

func (e *Executor) ResumeTick() int64 {
	return e.nextTick
}

func (e *Executor) Step(ctx context.Context) (TickResult, error) {
	if e.Done() {
		return TickResult{}, ErrExecutorDone
	}
	if err := ctx.Err(); err != nil {
		return TickResult{}, &InterruptedError{LastCommittedTick: e.lastCommitted}
	}
	tick := e.nextTick
	result, err := e.sim.Step(tick)
	if err != nil {
		return TickResult{}, err
	}
	checkpoint := e.sim.checkpoint(tick, result)
	if err := e.store.CommitCheckpoint(ctx, checkpoint); err != nil {
		if ctx.Err() != nil {
			return TickResult{}, &InterruptedError{LastCommittedTick: e.lastCommitted}
		}
		return TickResult{}, err
	}
	e.lastCommitted = tick
	e.nextTick = tick + 1
	return result, nil
}

func (e *Executor) Run(ctx context.Context) (RunSummary, error) {
	summary := RunSummary{
		ResumedFromTick:   e.lastCommitted,
		LastCommittedTick: e.lastCommitted,
		Ticks:             []TickResult{},
	}
	for !e.Done() {
		select {
		case <-ctx.Done():
			summary.LastCommittedTick = e.lastCommitted
			return summary, &InterruptedError{LastCommittedTick: e.lastCommitted}
		default:
		}
		result, err := e.Step(ctx)
		if err != nil {
			summary.LastCommittedTick = e.lastCommitted
			return summary, err
		}
		summary.Ticks = append(summary.Ticks, result)
	}
	summary.LastCommittedTick = e.lastCommitted
	return summary, nil
}
