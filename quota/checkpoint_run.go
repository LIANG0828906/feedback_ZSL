package quota

import (
	"context"
	"errors"
	"time"
)

type CheckpointRun struct {
	Timeline         *Timeline `json:"timeline"`
	Resumed          bool      `json:"resumed"`
	ResumedFromMilli int64     `json:"resumed_from_millis"`
	Restarted        bool      `json:"restarted_from_origin"`
	RestartReason    string    `json:"restart_reason,omitempty"`
	Interrupted      bool      `json:"interrupted"`
}

// RunWithCheckpoint commits each complete logical time in its own SQLite
// transaction. A canceled context can leave only the previous full time in
// storage, which is the required recoverable boundary.
func RunWithCheckpoint(ctx context.Context, p *Project, opts Options, store *CheckpointStore) (*CheckpointRun, error) {
	return runWithCheckpointLimit(ctx, p, opts, store, -1)
}

// RunWithCheckpointLimit stops after commitLimit complete logical-time
// commits. It simulates a forced process stop at a transaction boundary and
// leaves a valid checkpoint for the next resume.
func RunWithCheckpointLimit(ctx context.Context, p *Project, opts Options, store *CheckpointStore, commitLimit int) (*CheckpointRun, error) {
	if commitLimit == 0 {
		return nil, NewError(CodeInvalidBudget, "checkpoint_run", "commit_limit", "commit limit must be positive or -1")
	}
	return runWithCheckpointLimit(ctx, p, opts, store, commitLimit)
}

func runWithCheckpointLimit(ctx context.Context, p *Project, opts Options, store *CheckpointStore, commitLimit int) (*CheckpointRun, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	opts, err := normalizeOptions(opts)
	if err != nil {
		return nil, err
	}
	fp, err := p.Fingerprint()
	if err != nil {
		return nil, err
	}
	state := NewState(p)
	result := &CheckpointRun{}
	snap, err := store.Load(ctx, p)
	if err != nil {
		switch ErrorCode(err) {
		case CodeCheckpointMissing:
			// Fresh origin.
		case CodeCheckpointVersion, CodeVersionMismatch:
			result.Restarted = true
			result.RestartReason = ErrorCode(err)
			if resetErr := store.Reset(ctx); resetErr != nil {
				return nil, resetErr
			}
		default:
			return nil, err
		}
	} else {
		state = snap.State.Clone()
		result.Resumed = true
		result.ResumedFromMilli = state.CommittedTimeMilli
	}
	var steps []TimeStep
	commits := 0
	times := distinctTimes(p.Events, func() int64 {
		if state.Committed {
			return state.CommittedTimeMilli
		}
		return -1
	}())
	for _, at := range times {
		if err := ctx.Err(); err != nil {
			result.Interrupted = true
			break
		}
		step, next, err := advanceTime(ctx, p, opts, state, at)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				result.Interrupted = true
				break
			}
			result.Timeline = &Timeline{ProjectFingerprint: fp, Steps: steps, FinalState: state.Clone()}
			return result, err
		}
		state = next
		steps = append(steps, step)
		saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		saveErr := store.Save(saveCtx, p, state)
		cancel()
		if saveErr != nil {
			result.Timeline = &Timeline{ProjectFingerprint: fp, Steps: steps, FinalState: state.Clone()}
			return result, saveErr
		}
		commits++
		if commitLimit > 0 && commits >= commitLimit {
			result.Interrupted = true
			break
		}
	}
	result.Timeline = &Timeline{ProjectFingerprint: fp, Steps: steps, FinalState: state.Clone()}
	return result, nil
}
