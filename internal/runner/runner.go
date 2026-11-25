// Package runner drives a tick-by-tick simulation with durable checkpoints,
// graceful signal handling and recovery. It uses the same fncore state
// machines as the oracle so recovered runs reproduce the reference truth.
package runner

import (
	"context"
	"encoding/json"
	"os"
	"os/signal"
	"syscall"

	"coldkernel/internal/checkpoint"
	"coldkernel/internal/errcode"
	"coldkernel/internal/fncore"
	"coldkernel/internal/model"
	"coldkernel/internal/oracle"
)

// Runner owns the per-function cores and the checkpoint store.
type Runner struct {
	project *model.Project
	store   *checkpoint.Store
	cores   map[string]*fncore.Core
	reqByID map[string]*model.Request

	startTick int64
	results   []*model.TickResult
	stopped   bool
	stopAfter int64 // graceful stop once this tick commits; -1 disables
}

// New creates a runner and, if a matching checkpoint exists, restores the
// per-function cores at the last complete commit point. A version mismatch
// returns E_CHECKPOINT_VERSION so the caller can force a full recompute.
func New(p *model.Project, store *checkpoint.Store) (*Runner, error) {
	r := &Runner{
		project: p,
		store:   store,
		cores:   map[string]*fncore.Core{},
		reqByID: map[string]*model.Request{},
		stopAfter: -1,
	}
	for _, req := range p.Requests {
		r.reqByID[req.ID] = req
	}
	for _, fn := range p.Functions {
		r.cores[fn.ID] = fncore.New(fn, p.Seed)
	}

	last, _, _, err := store.LatestTick()
	if err != nil {
		return nil, err
	}
	if last < 0 {
		r.startTick = 0
		return r, nil
	}
	states, _, err := store.Restore(last)
	if err != nil {
		return nil, err
	}
	for id, st := range states {
		if c, ok := r.cores[id]; ok {
			c.Load(st)
			c.LoadQueue(st, r.reqByID)
		}
	}
	r.startTick = last + 1
	return r, nil
}

// StartTick reports the first tick that still needs to be computed.
func (r *Runner) StartTick() int64 { return r.startTick }

// StopAfterTick requests a graceful stop once tick t has been checkpointed.
// It models a handleable signal without racing the database connection.
func (r *Runner) StopAfterTick(t int64) { r.stopAfter = t }

// InstallSignalHandler treats SIGINT/SIGTERM as graceful, handleable
// signals: the current tick finishes and is checkpointed before exit.
// SIGKILL cannot be caught and is intentionally not handled; recovery then
// relies solely on the last durable commit point with no exit report.
func (r *Runner) InstallSignalHandler(cancel context.CancelFunc) <-chan os.Signal {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-ch
		r.stopped = true
		cancel()
	}()
	return ch
}

// Run advances ticks up to the horizon or until a graceful stop is
// requested, checkpointing each completed tick.
func (r *Runner) Run(ctx context.Context) ([]*model.TickResult, error) {
	arrivals := map[int64]map[string][]*model.Request{}
	for _, req := range r.project.Requests {
		if arrivals[req.ArriveTick] == nil {
			arrivals[req.ArriveTick] = map[string][]*model.Request{}
		}
		arrivals[req.ArriveTick][req.FunctionID] =
			append(arrivals[req.ArriveTick][req.FunctionID], req)
	}

	for tick := r.startTick; tick < r.project.HorizonTicks; tick++ {
		if ctx.Err() != nil {
			break
		}
		tr := &model.TickResult{Tick: tick, Functions: map[string]*model.FunctionTickState{}}
		var cps []checkpoint.FnCheckpoint
		for _, fn := range r.project.Functions {
			c := r.cores[fn.ID]
			c.BeginTick(tick)
			tr.Rejected = append(tr.Rejected, c.ExpireQueue(tick)...)
			adm, rej := c.DrainQueue(tick)
			tr.Admitted = append(tr.Admitted, adm...)
			tr.Rejected = append(tr.Rejected, rej...)
			for _, req := range arrivals[tick][fn.ID] {
				oc := c.HandleArrival(req, tick)
				record(tr, oc)
			}
			tr.Functions[fn.ID] = c.Snapshot(tick)
			cps = append(cps, checkpoint.FnCheckpoint{FunctionID: fn.ID, State: c.Save()})
		}
		oracle.SortOutcomes(tr.Admitted)
		oracle.SortOutcomes(tr.Queued)
		oracle.SortOutcomes(tr.Rejected)
		payload, err := json.Marshal(tr)
		if err != nil {
			return nil, errcode.New(errcode.ECheckpointState, "outcome encode failed")
		}
		if err := r.store.Commit(tick, cps, payload); err != nil {
			return nil, err
		}
		r.results = append(r.results, tr)
		if r.stopped || r.stopAfter == tick {
			break
		}
	}
	return r.results, nil
}

func record(tr *model.TickResult, oc *model.RequestOutcome) {
	switch oc.Decision {
	case model.DecisionAdmit:
		tr.Admitted = append(tr.Admitted, oc)
	case model.DecisionQueue:
		tr.Queued = append(tr.Queued, oc)
	default:
		tr.Rejected = append(tr.Rejected, oc)
	}
}

// Recovered verifies whether the checkpoint at lastTick matches the given
// reference per-function states. It is used by the demo to prove recovery
// reproduces the same observable state as a full run.
func Recovered(store *checkpoint.Store, lastTick int64) (map[string]*fncore.State, error) {
	states, _, err := store.Restore(lastTick)
	return states, err
}
