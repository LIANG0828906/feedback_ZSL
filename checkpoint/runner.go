package checkpoint

import (
	"context"
	"time"

	"chaosdiag/engine"
	"chaosdiag/model"
	"chaosdiag/reconcile"
)

type Outcome struct {
	Report       *model.Report
	LastTick     int
	StoppedEarly bool
}

type RecoveryOutcome struct {
	*Outcome
	Mode   string `json:"mode"`
	Reason string `json:"reason,omitempty"`
}

func RecoverOrRun(ctx context.Context, p *model.Project, store *Store, stopAfterTick int) (*RecoveryOutcome, error) {
	cp, err := store.Load()
	if err != nil {
		return nil, err
	}
	if cp == nil || cp.LastTick == 0 {
		out, err := RunFresh(ctx, p, store, stopAfterTick)
		if err != nil {
			return &RecoveryOutcome{Outcome: out, Mode: "fresh"}, err
		}
		return &RecoveryOutcome{Outcome: out, Mode: "fresh"}, nil
	}
	if cp.ProjectVersion != p.ProjectVersion {
		out, err := RunFresh(ctx, p, store, stopAfterTick)
		if err != nil {
			return &RecoveryOutcome{Outcome: out, Mode: "full_replay", Reason: model.CodeCheckpointVersionMismatch}, err
		}
		return &RecoveryOutcome{Outcome: out, Mode: "full_replay", Reason: "checkpoint project version mismatch; partial cache rejected"}, nil
	}
	out, err := Resume(ctx, p, store, *cp, stopAfterTick)
	if err != nil {
		return &RecoveryOutcome{Outcome: out, Mode: "resume"}, err
	}
	return &RecoveryOutcome{Outcome: out, Mode: "resume"}, nil
}

type RunOptions struct {
	StopAfterTick int
	TickPause     time.Duration
	AfterSave     func(tick int)
}

func RunFresh(ctx context.Context, p *model.Project, store *Store, stopAfterTick int) (*Outcome, error) {
	return RunFreshWithOptions(ctx, p, store, RunOptions{StopAfterTick: stopAfterTick})
}

func RunFreshWithOptions(ctx context.Context, p *model.Project, store *Store, opts RunOptions) (*Outcome, error) {
	schedules := engine.Schedule(p)
	activeByTick := engine.ActiveMap(schedules)
	results := map[string]model.RequestResult{}
	var last Checkpoint

	for _, schedule := range schedules {
		if err := ctx.Err(); err != nil {
			return &Outcome{LastTick: last.LastTick, StoppedEarly: true}, err
		}
		for _, req := range requestsAtTick(p, schedule.Tick) {
			results[req.ID] = engine.SimulateRequest(p, req, activeByTick[schedule.Tick])
		}
		cp := Checkpoint{
			ProjectVersion: p.ProjectVersion, LastTick: schedule.Tick,
			Requests: resultValues(results), Queue: schedule.Queued,
		}
		if err := store.Save(cp); err != nil {
			return nil, err
		}
		last = cp
		if opts.AfterSave != nil {
			opts.AfterSave(schedule.Tick)
		}
		if opts.StopAfterTick > 0 && schedule.Tick >= opts.StopAfterTick {
			return buildOutcome(p, schedules, results, last.LastTick, true), nil
		}
		if opts.TickPause > 0 && schedule.Tick < len(schedules) {
			select {
			case <-ctx.Done():
				return buildOutcome(p, schedules, results, last.LastTick, true), ctx.Err()
			case <-time.After(opts.TickPause):
			}
		}
	}
	return buildOutcome(p, schedules, results, last.LastTick, false), nil
}

func Resume(ctx context.Context, p *model.Project, store *Store, cp Checkpoint, stopAfterTick int) (*Outcome, error) {
	if cp.ProjectVersion != p.ProjectVersion {
		ce := model.NewError(model.CodeCheckpointVersionMismatch, "checkpoint.project_version", "refusing partial resume across project versions")
		model.WithContext(ce, "checkpoint_version", cp.ProjectVersion)
		model.WithContext(ce, "expected_version", p.ProjectVersion)
		return nil, ce
	}
	schedules := engine.Schedule(p)
	activeByTick := engine.ActiveMap(schedules)
	results := map[string]model.RequestResult{}
	for _, rr := range cp.Requests {
		results[rr.RequestID] = rr
	}
	lastTick := cp.LastTick

	for _, schedule := range schedules {
		if schedule.Tick <= cp.LastTick {
			continue
		}
		if err := ctx.Err(); err != nil {
			return &Outcome{LastTick: lastTick, StoppedEarly: true}, err
		}
		for _, req := range requestsAtTick(p, schedule.Tick) {
			results[req.ID] = engine.SimulateRequest(p, req, activeByTick[schedule.Tick])
		}
		checkpoint := Checkpoint{
			ProjectVersion: p.ProjectVersion, LastTick: schedule.Tick,
			Requests: resultValues(results), Queue: schedule.Queued,
		}
		if err := store.Save(checkpoint); err != nil {
			return nil, err
		}
		lastTick = schedule.Tick
		if stopAfterTick > 0 && schedule.Tick >= stopAfterTick {
			return buildOutcome(p, schedules, results, lastTick, true), nil
		}
	}
	return buildOutcome(p, schedules, results, lastTick, false), nil
}

func buildOutcome(p *model.Project, schedules []model.TickSchedule, results map[string]model.RequestResult, lastTick int, stopped bool) *Outcome {
	ordered := make([]model.RequestResult, 0, len(p.Requests))
	for _, req := range p.Requests {
		if rr, ok := results[req.ID]; ok {
			ordered = append(ordered, rr)
		}
	}
	report := &model.Report{
		ProjectID: p.ProjectID, ProjectVersion: p.ProjectVersion, Seed: p.Seed,
		Schedules: schedules, Results: ordered,
	}
	report.Verdicts = reconcile.ReconcileAll(p, ordered)
	return &Outcome{Report: report, LastTick: lastTick, StoppedEarly: stopped}
}

func requestsAtTick(p *model.Project, tick int) []model.Request {
	out := []model.Request{}
	for _, req := range p.Requests {
		if req.Tick == tick {
			out = append(out, req)
		}
	}
	return out
}

func resultValues(m map[string]model.RequestResult) []model.RequestResult {
	out := make([]model.RequestResult, 0, len(m))
	for _, rr := range m {
		out = append(out, rr)
	}
	return out
}
