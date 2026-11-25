package workbench

import (
	"context"
	"os"

	"mvccworkbench/internal/checkpoint"
	"mvccworkbench/internal/domain"
	"mvccworkbench/internal/engine"
)

type RecoveryMode string

const (
	RecoveryResumed     RecoveryMode = "VERSION_MATCHED_RESUME"
	RecoveryFullReplay  RecoveryMode = "VERSION_MISMATCH_FULL_REPLAY"
	RecoveryFaultReplay RecoveryMode = "CHECKPOINT_FAULT_FULL_REPLAY"
	RecoveryFresh       RecoveryMode = "NO_CHECKPOINT_FRESH"
)

type RecoverySession struct {
	Store  *checkpoint.Store
	Runner *engine.StreamRunner
	State  *checkpoint.State
	Mode   RecoveryMode
}

// Recover opens the checkpoint and applies the declared policy: matching
// versions resume; meta-version mismatch wipes old canonical chains and starts
// a full replay; missing checkpoints start fresh.
func Recover(ctx context.Context, project *domain.Project, dbPath string, opts engine.Options) (*RecoverySession, error) {
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return nil, err
	}
	state, err := store.Load(ctx)
	if domain.CodeOf(err) == "E_CHECKPOINT_NOT_FOUND" {
		return &RecoverySession{Store: store, Runner: engine.NewStreamRunner(project, opts), Mode: RecoveryFresh}, nil
	}
	if err != nil {
		if domain.CodeOf(err) == domain.ECkptVersionMismatch {
			if wipeErr := store.Wipe(ctx); wipeErr != nil {
				return nil, wipeErr
			}
			return &RecoverySession{Store: store, Runner: engine.NewStreamRunner(project, opts), Mode: RecoveryFullReplay}, nil
		}
		if domain.CodeOf(err) == domain.ECkptTruncated || domain.CodeOf(err) == domain.ECkptCorrupt {
			if wipeErr := store.Wipe(ctx); wipeErr == nil {
				return &RecoverySession{Store: store, Runner: engine.NewStreamRunner(project, opts), Mode: RecoveryFaultReplay}, nil
			}
			path := store.Path()
			_ = store.Close()
			_ = os.Remove(path)
			_ = os.Remove(path + "-wal")
			_ = os.Remove(path + "-shm")
			rebuilt, openErr := checkpoint.Open(path)
			if openErr != nil {
				return nil, openErr
			}
			return &RecoverySession{Store: rebuilt, Runner: engine.NewStreamRunner(project, opts), Mode: RecoveryFaultReplay}, nil
		}
		_ = store.Close()
		return nil, err
	}
	runner := engine.NewStreamRunner(project, opts)
	if err := runner.RestoreState(state); err != nil {
		_ = store.Close()
		return nil, err
	}
	return &RecoverySession{Store: store, Runner: runner, State: state, Mode: RecoveryResumed}, nil
}

type CrashResumeReport struct {
	DBPath               string       `json:"db_path"`
	CrashAtOp            int          `json:"crash_at_op"`
	CheckpointSeqAtCrash int          `json:"checkpoint_seq_at_crash"`
	RecoveryMode         RecoveryMode `json:"recovery_mode"`
	FinalSeq             int          `json:"final_seq"`
	ProcessedAfterResume []int        `json:"processed_after_resume"`
	ReadCount            int          `json:"read_count"`
	GCCount              int          `json:"gc_count"`
}

func CrashAndResume(ctx context.Context, project *domain.Project, dbPath string, crashAt int, opts engine.Options) (*CrashResumeReport, error) {
	_ = os.Remove(dbPath)
	_ = os.Remove(dbPath + "-wal")
	_ = os.Remove(dbPath + "-shm")

	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return nil, err
	}
	runner := engine.NewStreamRunner(project, opts)
	crashSeq := 0
	for {
		op, done, err := runner.Step(ctx)
		if err != nil {
			return nil, err
		}
		if done {
			break
		}
		if err := runner.SaveCheckpoint(ctx, store); err != nil {
			return nil, err
		}
		crashSeq = runner.LastCommittedSeq()
		if op.Index == crashAt {
			_ = store.Close() // hard process-stop simulation: no deferred work
			break
		}
	}

	session, err := Recover(ctx, project, dbPath, opts)
	if err != nil {
		return nil, err
	}
	defer session.Store.Close()
	after := []int{}
	for {
		before := session.Runner.LastCommittedSeq()
		op, done, err := session.Runner.Step(ctx)
		if err != nil {
			return nil, err
		}
		if done {
			break
		}
		if op.Index > crashAt || before >= crashAt {
			after = append(after, op.Index)
		}
		if err := session.Runner.SaveCheckpoint(ctx, session.Store); err != nil {
			return nil, err
		}
	}
	report := session.Runner.Report()
	state, _ := session.Store.Load(ctx)
	finalSeq := session.Runner.LastCommittedSeq()
	if state != nil {
		finalSeq = state.CommittedOpSeq
	}
	return &CrashResumeReport{
		DBPath:               dbPath,
		CrashAtOp:            crashAt,
		CheckpointSeqAtCrash: crashSeq,
		RecoveryMode:         session.Mode,
		FinalSeq:             finalSeq,
		ProcessedAfterResume: after,
		ReadCount:            len(report.Reads),
		GCCount:              len(report.GC),
	}, nil
}
