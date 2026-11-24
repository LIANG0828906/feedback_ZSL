// Package runner orchestrates engine windows with atomic SQLite checkpoints.
package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"economycore/checkpoint"
	"economycore/engine"
	"economycore/model"
)

// Options configures deterministic settlement and test fault injection.
type Options struct {
	AlgorithmVersion string
	Fault            func(window int, cycle int64) error
}

// Report is an explicit partial/complete settlement report.
type Report struct {
	Complete            bool                   `json:"complete"`
	TotalWindows        int                    `json:"totalWindows"`
	CommittedWindows    int                    `json:"committedWindows"`
	LastCommittedWindow int                    `json:"lastCommittedWindow"`
	Cycles              []engine.CycleSnapshot `json:"cycles"`
	OverflowTotals      map[string]int64       `json:"overflowTotals"`
	IntentOffset        int                    `json:"intentOffset"`
	FinalState          model.RuntimeState     `json:"finalState"`
	ReusedCycles        int                    `json:"reusedCycles"`
	RecomputedCycles    int                    `json:"recomputedCycles"`
	Invalidated         bool                   `json:"invalidated"`
	InvalidationReason  string                 `json:"invalidationReason,omitempty"`
	FailedWindows       []checkpoint.Failure   `json:"failedWindows,omitempty"`
}

// Runner serializes window execution and checkpoint commits.
type Runner struct {
	mu       sync.Mutex
	store    *checkpoint.Store
	fp       model.Fingerprint
	engine   *engine.Engine
	records  []checkpoint.Record
	recovery checkpoint.Recovery
}

// New opens the committed prefix and restores only a fully compatible,
// contiguous snapshot.
func New(m *model.Model, store *checkpoint.Store, opts Options) (*Runner, error) {
	if store == nil {
		return nil, model.NewError(model.EStore, "/database", "checkpoint store is required")
	}
	algorithmVersion := opts.AlgorithmVersion
	if algorithmVersion == "" {
		algorithmVersion = model.DefaultAlgorithmVersion
	}
	fp, err := m.Fingerprint(algorithmVersion)
	if err != nil {
		return nil, err
	}
	recovery, err := store.LoadCompatible(context.Background(), fp, m.WindowCount)
	if err != nil {
		return nil, err
	}
	eng, err := engine.New(m, engine.Options{AlgorithmVersion: algorithmVersion, Fault: opts.Fault})
	if err != nil {
		return nil, err
	}
	if len(recovery.Records) > 0 {
		last := recovery.Records[len(recovery.Records)-1]
		state, decodeErr := engine.DecodeState(last.StateJSON)
		expectedNext := int64((last.Window + 1) * m.WindowCycles)
		if decodeErr != nil || state.NextCycle != expectedNext {
			decodeErr = model.NewError(model.EStore, "/checkpoint/state", "checkpoint state is corrupt or at the wrong window boundary")
		} else {
			decodeErr = eng.RestoreState(state)
		}
		if decodeErr != nil {
			if invalidateErr := store.InvalidateAll(context.Background()); invalidateErr != nil {
				return nil, invalidateErr
			}
			recovery = checkpoint.Recovery{
				Fingerprint:        fp,
				ExpectedWindows:    m.WindowCount,
				LastWindow:         -1,
				Invalidated:        true,
				InvalidationReason: "checkpoint state is corrupt or at the wrong window boundary",
			}
		}
	}
	return &Runner{
		store:    store,
		fp:       fp,
		engine:   eng,
		records:  append([]checkpoint.Record(nil), recovery.Records...),
		recovery: recovery,
	}, nil
}

// Fingerprint returns the all-or-nothing checkpoint identity.
func (r *Runner) Fingerprint() model.Fingerprint { return r.fp }

// Recovery returns the state observed at runner construction.
func (r *Runner) Recovery() checkpoint.Recovery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneRecovery(r.recovery)
}

// RunNextWindow settles and commits the next window.
func (r *Runner) RunNextWindow(ctx context.Context) (engine.WindowResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result, _, err := r.runNextWindowLocked(ctx)
	return result, err
}

// RunAll resumes from the committed prefix and runs all remaining windows.
func (r *Runner) RunAll(ctx context.Context) (*Report, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(r.records) < r.engine.Model().WindowCount {
		if err := ctx.Err(); err != nil {
			report, buildErr := r.buildReportLocked(false)
			if buildErr != nil {
				return nil, buildErr
			}
			return report, model.NewError(model.ECancel, "/context", err.Error()).WithCause(err)
		}
		if _, _, err := r.runNextWindowLocked(ctx); err != nil {
			report, buildErr := r.buildReportLocked(false)
			if buildErr != nil {
				return nil, buildErr
			}
			return report, err
		}
	}
	report, err := r.buildReportLocked(true)
	if err != nil {
		return nil, err
	}
	return report, nil
}

func (r *Runner) runNextWindowLocked(ctx context.Context) (engine.WindowResult, bool, error) {
	m := r.engine.Model()
	window := int(r.engine.State().NextCycle / int64(m.WindowCycles))
	if window >= m.WindowCount {
		return engine.WindowResult{}, false, model.NewError(model.EWindow, "/window", "all windows are already committed").WithWindow(window)
	}
	next := r.engine.Clone()
	result, err := next.RunWindow(ctx, window)
	if err != nil {
		r.recordFailure(window, err)
		return engine.WindowResult{}, false, err
	}
	if err := ctx.Err(); err != nil {
		r.recordFailure(window, model.NewError(model.ECancel, "/context", err.Error()).WithCause(err))
		return engine.WindowResult{}, false, model.NewError(model.ECancel, "/context", err.Error()).WithCause(err)
	}
	stateJSON, err := json.Marshal(result.State)
	if err != nil {
		return engine.WindowResult{}, false, model.NewError(model.EIO, "/state", err.Error()).WithCause(err)
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return engine.WindowResult{}, false, model.NewError(model.EIO, "/windowResult", err.Error()).WithCause(err)
	}
	record := checkpoint.Record{
		Window:       window,
		IntentOffset: result.IntentOffset,
		NextCycle:    result.State.NextCycle,
		StateJSON:    stateJSON,
		ResultJSON:   resultJSON,
		Fingerprint:  r.fp,
	}
	already, err := r.store.CommitWindow(ctx, record)
	if err != nil {
		return engine.WindowResult{}, false, err
	}
	if already {
		recovery, loadErr := r.store.LoadCompatible(context.Background(), r.fp, m.WindowCount)
		if loadErr != nil {
			return engine.WindowResult{}, false, loadErr
		}
		r.recovery = recovery
		r.records = append([]checkpoint.Record(nil), recovery.Records...)
		if err := r.restoreLastRecordLocked(); err != nil {
			return engine.WindowResult{}, false, err
		}
		return result, false, nil
	}
	r.engine = next
	r.records = append(r.records, record)
	r.recovery.Records = append(r.recovery.Records, record)
	r.recovery.ValidWindows = len(r.records)
	r.recovery.LastWindow = window
	r.recovery.Complete = len(r.records) == m.WindowCount
	failures, failureErr := r.store.Failures(context.Background())
	if failureErr == nil {
		r.recovery.Failures = failures
	}
	return result, true, nil
}

func (r *Runner) restoreLastRecordLocked() error {
	if len(r.records) == 0 {
		return nil
	}
	last := r.records[len(r.records)-1]
	state, err := engine.DecodeState(last.StateJSON)
	if err != nil {
		return err
	}
	return r.engine.RestoreState(state)
}

func (r *Runner) recordFailure(window int, failure error) {
	code := model.EStore
	message := failure.Error()
	if typed, ok := model.AsError(failure); ok {
		code = typed.Code
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = r.store.RecordFailure(ctx, window, r.fp, string(code), message)
}

func (r *Runner) buildReportLocked(complete bool) (*Report, error) {
	m := r.engine.Model()
	report := &Report{
		Complete:            complete && len(r.records) == m.WindowCount,
		TotalWindows:        m.WindowCount,
		CommittedWindows:    len(r.records),
		LastCommittedWindow: -1,
		OverflowTotals:      make(map[string]int64),
		Invalidated:         r.recovery.Invalidated,
		InvalidationReason:  r.recovery.InvalidationReason,
	}
	for _, record := range r.records {
		var result engine.WindowResult
		if err := json.Unmarshal(record.ResultJSON, &result); err != nil {
			return nil, model.NewError(model.EIO, "/checkpoint/result", fmt.Sprintf("window %d result is corrupt", record.Window)).WithCause(err)
		}
		report.Cycles = append(report.Cycles, result.Cycles...)
		report.ReusedCycles += result.ReusedCycles
		report.RecomputedCycles += result.RecomputedCycles
		report.IntentOffset = result.IntentOffset
		report.FinalState = result.State
		report.LastCommittedWindow = record.Window
	}
	if len(r.records) > 0 {
		report.OverflowTotals = cloneMap(report.FinalState.OverflowTotals)
	}
	failures, err := r.store.Failures(context.Background())
	if err != nil {
		return nil, err
	}
	report.FailedWindows = failures
	return report, nil
}

func cloneMap(in map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneRecovery(in checkpoint.Recovery) checkpoint.Recovery {
	out := in
	out.Records = append([]checkpoint.Record(nil), in.Records...)
	out.Failures = append([]checkpoint.Failure(nil), in.Failures...)
	return out
}
