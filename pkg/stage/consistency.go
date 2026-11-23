package stage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type PathReport struct {
	Name                  string         `json:"name"`
	Progress              Progress       `json:"progress"`
	CommitOrder           []int          `json:"commit_order"`
	Partial               bool           `json:"partial"`
	CheckpointInvalidated bool           `json:"checkpoint_invalidated,omitempty"`
	BudgetRecords         []BudgetRecord `json:"budget_records,omitempty"`
	Duration              time.Duration  `json:"duration"`
	Decision              ModeDecision   `json:"decision"`
}

type ConsistencyReport struct {
	ProjectID           string       `json:"project_id"`
	Mode                Mode         `json:"mode"`
	Realtime            PathReport   `json:"realtime"`
	Recovery            PathReport   `json:"recovery"`
	Full                PathReport   `json:"full"`
	FramesCompared      int          `json:"frames_compared"`
	WindowsCompared     int          `json:"windows_compared"`
	BlockedBudgetEvents int          `json:"blocked_budget_events"`
	Consistent          bool         `json:"consistent"`
	FirstDivergence     *ErrorDetail `json:"first_divergence,omitempty"`
}

func VerifyThreePaths(ctx context.Context, project *Project, params Params, workDir string) (ConsistencyReport, error) {
	return VerifyThreePathsWithOptions(ctx, project, params, workDir, false)
}

func VerifyThreePathsWithOptions(ctx context.Context, project *Project, params Params, workDir string, skipTruthCheck bool) (ConsistencyReport, error) {
	report := ConsistencyReport{ProjectID: project.ID}
	if workDir == "" {
		dir, err := os.MkdirTemp("", "stageworkbench-verify-*")
		if err != nil {
			return report, storageError("create verification directory", err)
		}
		defer os.RemoveAll(dir)
		workDir = dir
	} else if err := os.MkdirAll(workDir, 0o755); err != nil {
		return report, storageError("create verification directory", err)
	}
	for _, name := range []string{"realtime.db", "recovery.db", "full.db"} {
		if err := os.Remove(filepath.Join(workDir, name)); err != nil && !os.IsNotExist(err) {
			return report, storageError("reset verification checkpoint", err)
		}
	}
	run := func(mode RunMode, dbName string, stopAfter int, fault bool, faultWindow int) (RunResult, error) {
		runner, err := NewRunner(project, params)
		if err != nil {
			return RunResult{}, err
		}
		report.Mode = runner.Decision().Mode
		return runner.Run(ctx, RunOptions{
			Mode:           mode,
			CheckpointPath: filepath.Join(workDir, dbName),
			StopAfter:      stopAfter,
			FaultInjection: fault,
			FaultWindow:    faultWindow,
			SkipTruthCheck: skipTruthCheck,
		})
	}
	started := time.Now()
	realtimeRun, err := run(RunRealtime, "realtime.db", 0, false, -1)
	if err != nil {
		return report, err
	}
	report.Realtime = pathReport("realtime", realtimeRun, time.Since(started))

	started = time.Now()
	recoveryDB := "recovery.db"
	total := WindowCount(project.DurationMS, params.WithDefaults(project).WindowMS)
	interruptAfter := total / 2
	if interruptAfter < 1 {
		interruptAfter = 1
	}
	if interruptAfter >= total && total > 1 {
		interruptAfter = total - 1
	}
	first, err := run(RunRealtime, recoveryDB, interruptAfter, false, -1)
	if err != nil {
		return report, err
	}
	if !first.Partial && total > 1 {
		return report, newError(CodeState, "recovery run did not stop at the requested checkpoint", "verification", "stop_after")
	}
	second, err := run(RunResume, recoveryDB, 0, false, -1)
	if err != nil {
		return report, err
	}
	second.BudgetLog = append(first.BudgetLog, second.BudgetLog...)
	second.Progress.Committed = len(second.Windows)
	second.Partial = second.Progress.Committed < second.Progress.Total
	report.Recovery = pathReport("recovery", second, time.Since(started))

	started = time.Now()
	fullRun, err := run(RunFull, "full.db", 0, false, -1)
	if err != nil {
		return report, err
	}
	report.Full = pathReport("full", fullRun, time.Since(started))

	left := realtimeRun.Snapshot()
	recovery := second.Snapshot()
	full := fullRun.Snapshot()
	if err := ComparePathSnapshots(left, recovery); err != nil {
		report.Consistent = false
		if de, ok := AsDomainError(err); ok && len(de.Details) > 0 {
			detail := de.Details[0]
			report.FirstDivergence = &detail
		}
		return report, err
	}
	if err := ComparePathSnapshots(left, full); err != nil {
		report.Consistent = false
		if de, ok := AsDomainError(err); ok && len(de.Details) > 0 {
			detail := de.Details[0]
			report.FirstDivergence = &detail
		}
		return report, err
	}
	report.Consistent = true
	report.FramesCompared = len(left.Frames)
	report.WindowsCompared = len(left.CueOffsets)
	report.BlockedBudgetEvents = countBlocked(realtimeRun.BudgetLog) + countBlocked(second.BudgetLog) + countBlocked(fullRun.BudgetLog)
	return report, nil
}

func pathReport(name string, result RunResult, duration time.Duration) PathReport {
	return PathReport{
		Name:                  name,
		Progress:              result.Progress,
		CommitOrder:           append([]int(nil), result.CommitOrder...),
		Partial:               result.Partial,
		CheckpointInvalidated: result.CheckpointInvalidated,
		BudgetRecords:         append([]BudgetRecord(nil), result.BudgetLog...),
		Duration:              duration,
		Decision:              result.Decision,
	}
}

func countBlocked(records []BudgetRecord) int {
	count := 0
	for _, record := range records {
		if record.Event == "blocked" {
			count++
		}
	}
	return count
}

func (r ConsistencyReport) Summary() string {
	return fmt.Sprintf("consistent=%t frames=%d windows=%d blocked=%d mode=%s",
		r.Consistent, r.FramesCompared, r.WindowsCompared, r.BlockedBudgetEvents, r.Mode)
}
