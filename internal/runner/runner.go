package runner

import (
	"os"
	"path/filepath"

	"obsproj/internal/checkpoint"
	"obsproj/internal/kernel"
	"obsproj/internal/model"
)

type Options struct {
	Engine     kernel.EngineOptions
	StoreDir   string
	SplitIndex int
}

func DefaultOptions() Options {
	return Options{
		Engine:     kernel.DefaultEngineOptions(),
		SplitIndex: 3,
	}
}

func RunFullReplay(session *model.Session, options Options) (model.RunResult, error) {
	engine, err := kernel.NewEngine(session, options.Engine)
	if err != nil {
		return model.RunResult{}, err
	}
	windows := engine.WindowNumbers()
	return execute(engine, nil, windows, model.RunResult{})
}

func RunRealtime(session *model.Session, options Options) (model.RunResult, error) {
	if options.StoreDir == "" {
		dir, err := os.MkdirTemp("", "obs-realtime-*")
		if err != nil {
			return model.RunResult{}, model.NewError(model.CodeIO, "failed to create checkpoint temp directory")
		}
		options.StoreDir = dir
	}
	engine, err := kernel.NewEngine(session, options.Engine)
	if err != nil {
		return model.RunResult{}, err
	}
	store, openErr := checkpoint.Open(options.StoreDir, session.Fingerprint())
	if openErr != nil {
		return model.RunResult{}, openErr
	}
	return execute(engine, store, engine.WindowNumbers(), model.RunResult{})
}

// RunFirstPhase executes and commits an initial prefix. It is separated from
// ResumeFromCheckpoint so tests and demos can simulate an interruption.
func RunFirstPhase(session *model.Session, options Options) (model.RunResult, error) {
	if options.StoreDir == "" {
		return model.RunResult{}, model.NewError(model.CodeIO, "checkpoint store directory is required")
	}
	engine, err := kernel.NewEngine(session, options.Engine)
	if err != nil {
		return model.RunResult{}, err
	}
	windows := engine.WindowNumbers()
	split := options.SplitIndex
	if split <= 0 {
		split = 1
	}
	if split >= len(windows) {
		split = len(windows) - 1
	}
	if split < 1 {
		return model.RunResult{}, model.NewError(model.CodeParam, "resume split requires at least two windows")
	}
	store, openErr := checkpoint.Open(options.StoreDir, session.Fingerprint())
	if openErr != nil {
		return model.RunResult{}, openErr
	}
	result, runErr := execute(engine, store, windows[:split], model.RunResult{})
	if runErr != nil {
		return result, runErr
	}
	result.Final = engine.FinalSnapshot()
	return result, nil
}

func ResumeFromCheckpoint(session *model.Session, options Options) (model.RunResult, model.RecoveryReport, error) {
	if options.StoreDir == "" {
		return model.RunResult{}, model.RecoveryReport{}, model.NewError(model.CodeIO, "checkpoint store directory is required")
	}
	store, openErr := checkpoint.Open(options.StoreDir, session.Fingerprint())
	if openErr != nil {
		return model.RunResult{}, model.RecoveryReport{}, openErr
	}
	commits, report, loadErr := store.Load()
	if loadErr != nil {
		return model.RunResult{}, report, loadErr
	}
	if len(commits) == 0 {
		return model.RunResult{}, report, model.NewError(model.CodeState, "no valid committed window to resume from")
	}
	last := commits[len(commits)-1]
	engine, restoreErr := kernel.Restore(session, last.Snapshot, options.Engine)
	if restoreErr != nil {
		return model.RunResult{}, report, restoreErr
	}
	allWindows := engine.WindowNumbers()
	remaining := make([]int, 0, len(allWindows))
	for _, window := range allWindows {
		if window > last.Window {
			remaining = append(remaining, window)
		}
	}
	result := model.RunResult{Commits: commits}
	result, runErr := execute(engine, store, remaining, result)
	if runErr != nil {
		return result, report, runErr
	}
	result.Final = engine.FinalSnapshot()
	return result, report, nil
}

func RunResume(session *model.Session, options Options) (model.RunResult, error) {
	if options.StoreDir == "" {
		dir, err := os.MkdirTemp("", "obs-resume-*")
		if err != nil {
			return model.RunResult{}, model.NewError(model.CodeIO, "failed to create checkpoint temp directory")
		}
		options.StoreDir = dir
	}
	first, err := RunFirstPhase(session, options)
	if err != nil {
		return first, err
	}
	result, _, err := ResumeFromCheckpoint(session, options)
	if err != nil {
		return result, err
	}
	result.Queue = append(first.Queue, result.Queue...)
	return result, nil
}

func execute(engine *kernel.Engine, store *checkpoint.Store, windows []int, result model.RunResult) (model.RunResult, error) {
	for _, window := range windows {
		commit, queue, err := engine.ProcessWindow(window)
		if err != nil {
			return result, err
		}
		if store != nil {
			if err := store.Commit(commit); err != nil {
				return result, err
			}
		}
		result.Commits = append(result.Commits, commit)
		result.Queue = append(result.Queue, queue...)
	}
	result.Final = engine.FinalSnapshot()
	return result, nil
}

// NewTempStoreDir creates a clean directory suitable for an isolated replay.
func NewTempStoreDir(prefix string) (string, error) {
	dir, err := os.MkdirTemp("", prefix+"*")
	if err != nil {
		return "", err
	}
	return filepath.Clean(dir), nil
}
