package workbench

import (
	"context"
	"time"

	"mvccworkbench/internal/domain"
	"mvccworkbench/internal/engine"
)

type SafeRunReport struct {
	FinalSeq       int    `json:"final_seq"`
	Interrupted    bool   `json:"interrupted"`
	Code           string `json:"code,omitempty"`
	CheckpointPath string `json:"checkpoint_path"`
}

// RunCheckpointed persists every fully applied operation before starting the
// next one. A canceled/signal context therefore stops at the last complete
// commit point. Persistence uses an independent short timeout because the
// caller's context may already be canceled by SIGINT/SIGTERM.
func RunCheckpointed(
	ctx context.Context,
	project *domain.Project,
	dbPath string,
	opts engine.Options,
	onStep func(committedSeq int),
) (*SafeRunReport, error) {
	session, err := Recover(context.Background(), project, dbPath, opts)
	if err != nil {
		return nil, err
	}
	defer session.Store.Close()

	for {
		if err := ctx.Err(); err != nil {
			persistCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			saveErr := session.Runner.SaveCheckpoint(persistCtx, session.Store)
			cancel()
			if saveErr != nil {
				return nil, saveErr
			}
			return &SafeRunReport{
					FinalSeq:       session.Runner.LastCommittedSeq(),
					Interrupted:    true,
					Code:           domain.EInterrupted,
					CheckpointPath: dbPath,
				}, domain.NewError(domain.EInterrupted, "stopped at last complete operation boundary").
					With("committed_op_seq", session.Runner.LastCommittedSeq())
		}
		_, done, err := session.Runner.Step(ctx)
		if err != nil {
			return nil, err
		}
		if done {
			persistCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			saveErr := session.Runner.SaveCheckpoint(persistCtx, session.Store)
			cancel()
			if saveErr != nil {
				return nil, saveErr
			}
			return &SafeRunReport{FinalSeq: session.Runner.LastCommittedSeq(), CheckpointPath: dbPath}, nil
		}
		persistCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		saveErr := session.Runner.SaveCheckpoint(persistCtx, session.Store)
		cancel()
		if saveErr != nil {
			return nil, saveErr
		}
		if onStep != nil {
			onStep(session.Runner.LastCommittedSeq())
		}
	}
}
