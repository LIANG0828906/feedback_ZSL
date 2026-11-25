package checkpoint

import (
	"sort"

	"credrot/internal/bizerr"
	"credrot/internal/engine"
	"credrot/internal/model"
)

// BuildState maps a committed solver view into the checkpoint payload. The
// caller commits the returned state through Store.Commit, which persists the
// subscription/pending/batch section before the global tick commit record.
func BuildState(
	p *model.Project,
	committedTick int64,
	validityAtTick []engine.SubscriptionValidity,
	propagation *engine.PropagationResult,
	schedule *engine.ScheduleResult,
	confirmed map[string]bool,
) (*State, error) {
	if p == nil {
		return nil, bizerr.New(bizerr.CodeSchemaInvalid, "project is nil", nil)
	}
	if committedTick < -1 || committedTick > p.HorizonTicks {
		return nil, bizerr.New(bizerr.CodeSchemaInvalid, "committed tick is outside the declared horizon", map[string]any{
			"committed_tick": committedTick,
			"horizon_ticks":  p.HorizonTicks,
		})
	}
	if propagation == nil {
		var err error
		propagation, err = engine.Propagate(p)
		if err != nil {
			return nil, err
		}
	}
	if schedule == nil {
		var err error
		schedule, err = engine.Schedule(p, propagation)
		if err != nil {
			return nil, err
		}
	}
	if validityAtTick == nil {
		solver, err := engine.NewSolver(p)
		if err != nil {
			return nil, err
		}
		validityAtTick, err = solver.ValidityAt(max64(committedTick, 0))
		if err != nil {
			return nil, err
		}
	}
	state := EmptyState(p.ManifestVersion)
	state.CommittedTick = committedTick
	for _, validity := range validityAtTick {
		ids := make([]string, 0, len(validity.Available))
		versions := make([]uint64, 0, len(validity.Available))
		for _, available := range validity.Available {
			ids = append(ids, available.CredentialID)
			versions = append(versions, available.Version)
		}
		state.Subscriptions = append(state.Subscriptions, CommittedSubscription{
			SubscriptionID: validity.SubscriptionID,
			CredentialIDs:  ids,
			Versions:       versions,
			Source:         "validity:tick=" + formatInt(validity.Tick),
		})
	}
	for _, suspended := range propagation.Suspended {
		state.Pending = append(state.Pending, PendingCredential{
			CredentialID:    suspended.CredentialID,
			SourceVersion:   suspended.SourceVersion,
			TargetVersion:   suspended.TargetVersion,
			ReloadBatch:     suspended.BaseReloadBatch,
			SubscriptionIDs: append([]string(nil), suspended.ArrivalSet...),
		})
	}
	for _, batch := range schedule.Batches {
		failed := make([]string, 0, len(batch.Failed))
		for _, failure := range batch.Failed {
			failed = append(failed, failure.SubscriptionID)
		}
		completedTick := batch.StartTick
		if len(batch.Attempts) > 0 {
			completedTick = batch.Attempts[len(batch.Attempts)-1].Tick
		}
		allConfirmed := len(batch.Actual) > 0
		for _, id := range batch.Actual {
			if !confirmed[id] {
				allConfirmed = false
				break
			}
		}
		state.Batches = append(state.Batches, BatchProgress{
			Batch:         batch.Batch,
			Planned:       append([]string(nil), batch.Planned...),
			Actual:        append([]string(nil), batch.Actual...),
			Failed:        failed,
			Attempts:      len(batch.Attempts),
			Confirmed:     allConfirmed,
			CompletedTick: completedTick,
		})
	}
	sort.Slice(state.Subscriptions, func(i, j int) bool {
		return state.Subscriptions[i].SubscriptionID < state.Subscriptions[j].SubscriptionID
	})
	return state, nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func formatInt(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [24]byte
	i := len(buf)
	negative := v < 0
	if negative {
		v = -v
	}
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
