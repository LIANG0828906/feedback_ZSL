package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"serviceregistry/checkpoint"
	"serviceregistry/registry"
)

type signalStopError struct {
	Signal syscall.Signal
}

func (e signalStopError) Error() string {
	return "interrupted by signal " + e.Signal.String()
}

func commandRun(args []string) int {
	flags := newFlagSet("run")
	projectPath := flags.String("project", "", "project JSON file")
	checkpointPath := flags.String("checkpoint", "", "SQLite checkpoint path")
	until := flags.Int64("until", -1, "last logical tick (default: project maximum)")
	noResume := flags.Bool("no-resume", false, "ignore an existing checkpoint")
	reportPath := flags.String("report", "", "write the committed run result as JSON")
	stepDelayMS := flags.Int("step-delay-ms", 0, "delay after each commit (diagnostics)")
	crashAfter := flags.Int64("crash-after-commit", -1, "send SIGKILL after committing this tick")
	if err := parseFlags(flags, args); err != nil {
		return fail(err, exitUsage)
	}
	if *projectPath == "" {
		return fail(registry.NewError(registry.CodeSchemaField, "--project is required", registry.ErrorContext{}), exitUsage)
	}
	project, err := registry.LoadProjectFile(*projectPath)
	if err != nil {
		return fail(err, exitBusiness)
	}
	hash, err := project.ModelHash()
	if err != nil {
		return fail(err, exitBusiness)
	}
	identity := checkpoint.Identity{
		EngineVersion:  registry.EngineVersion,
		ProjectVersion: project.ProjectVersion,
		ModelHash:      hash,
	}
	resumeStatus := checkpoint.ResumeStatus{Mode: checkpoint.ResumeModeFullRecompute, Reason: "checkpoint disabled"}
	store := &checkpoint.Store{Path: *checkpointPath}
	committed := []registry.StepResult{}
	var engine *registry.Engine
	if *checkpointPath != "" && !*noResume {
		value, status, err := store.LoadCompatible(identity)
		if err != nil {
			return fail(err, exitBusiness)
		}
		resumeStatus = status
		if status.Mode == checkpoint.ResumeModeFromCheckpoint {
			if value == nil {
				return fail(registry.NewError(registry.CodeCheckpointCorrupt, "resume checkpoint is nil", registry.ErrorContext{Checkpoint: *checkpointPath}), exitBusiness)
			}
			engine, err = registry.RestoreEngine(project, &value.Snapshot)
			if err != nil {
				return fail(err, exitBusiness)
			}
			committed = append(committed, value.Steps...)
		}
	}
	if engine == nil {
		engine, err = registry.NewEngine(project)
		if err != nil {
			return fail(err, exitBusiness)
		}
	}

	var signaled atomic.Int32
	signalChannel := make(chan os.Signal, 1)
	signal.Notify(signalChannel, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signalChannel)
	go func() {
		select {
		case received := <-signalChannel:
			if typed, ok := received.(syscall.Signal); ok {
				signaled.Store(int32(typed))
			} else {
				signaled.Store(int32(syscall.SIGTERM))
			}
		}
	}()

	lastTick := project.MaxEventTick()
	if *until >= 0 {
		lastTick = *until
	}
	observer := func(step registry.StepResult, snapshot registry.Snapshot) error {
		committed = append(committed, step)
		if *checkpointPath != "" {
			if err := store.Save(&checkpoint.Checkpoint{Snapshot: snapshot, Steps: committed}); err != nil {
				return err
			}
			if *crashAfter == step.Tick {
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
				return fmt.Errorf("SIGKILL did not terminate the process")
			}
		}
		if *stepDelayMS > 0 {
			time.Sleep(time.Duration(*stepDelayMS) * time.Millisecond)
		}
		if value := signaled.Load(); value != 0 {
			return signalStopError{Signal: syscall.Signal(value)}
		}
		return nil
	}
	runResult, runErr := engine.RunWithObserver(lastTick, observer)
	interrupted := false
	var interruptedSignal syscall.Signal
	if runErr != nil {
		var stop signalStopError
		if errors.As(runErr, &stop) {
			interrupted = true
			interruptedSignal = stop.Signal
		} else {
			return fail(runErr, exitBusiness)
		}
	}
	if runResult != nil && len(runResult.Steps) > 0 {
		_ = runResult
	}
	if *reportPath != "" {
		if err := writeJSONFile(*reportPath, &registry.RunResult{
			ProjectVersion: project.ProjectVersion,
			ModelHash:      hash,
			SchemaVersion:  project.SchemaVersion,
			Steps:          committed,
		}); err != nil {
			return fail(err, exitBusiness)
		}
	}
	summary := summarizeRun(project, hash, resumeStatus, committed, *checkpointPath, *reportPath)
	summary["interrupted"] = interrupted
	if interrupted {
		summary["signal"] = interruptedSignal.String()
	}
	if interrupted {
		if err := jsonEncodeStdout(summary); err != nil {
			return fail(err, exitInternal)
		}
		if interruptedSignal == syscall.SIGINT {
			return 130
		}
		return 143
	}
	return emit(summary)
}

func summarizeRun(project *registry.Project, hash string, status checkpoint.ResumeStatus, steps []registry.StepResult, checkpointPath, reportPath string) map[string]any {
	transitions := 0
	queries := 0
	dropped := 0
	deferred := 0
	var firstTick int64 = -1
	var lastTick int64 = -1
	for _, step := range steps {
		if firstTick < 0 {
			firstTick = step.Tick
		}
		lastTick = step.Tick
		transitions += len(step.Transitions)
		queries += len(step.Queries)
		dropped += len(step.DroppedProbes)
		deferred += len(step.DeferredProbes)
	}
	absolute, _ := filepath.Abs(checkpointPath)
	return map[string]any{
		"ok":              true,
		"command":         "run",
		"project_version": project.ProjectVersion,
		"model_hash":      hash,
		"schema_version":  project.SchemaVersion,
		"resume":          status,
		"steps":           len(steps),
		"first_tick":      firstTick,
		"last_tick":       lastTick,
		"transitions":     transitions,
		"queries":         queries,
		"dropped_probes":  dropped,
		"deferred_queues": deferred,
		"checkpoint":      checkpointPath,
		"checkpoint_abs":  absolute,
		"report":          reportPath,
	}
}

func absolutePath(path string) string {
	if path == "" {
		return ""
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return absolute
}
