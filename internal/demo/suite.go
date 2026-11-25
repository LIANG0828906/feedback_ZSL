package demo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	acoustic "levelacoustic"
	"levelacoustic/internal/sqlstore"
)

var errStopResumeSeed = errors.New("seed cancellation after first room")

type ProgressEvent struct {
	RoomID    int               `json:"roomId"`
	Committed int               `json:"committed"`
	Progress  acoustic.Progress `json:"progress"`
}

type PathOutput struct {
	Name           string                  `json:"name"`
	DatabasePath   string                  `json:"databasePath"`
	Result         *acoustic.RunResult     `json:"result"`
	Restore        *acoustic.RestoreReport `json:"restore,omitempty"`
	ProgressEvents []ProgressEvent         `json:"progressEvents"`
	Error          string                  `json:"error,omitempty"`
}

type PathComparison struct {
	Left        string                   `json:"left"`
	Right       string                   `json:"right"`
	Equal       bool                     `json:"equal"`
	Differences []acoustic.RunDifference `json:"differences,omitempty"`
}

type ErrorBoundSummary struct {
	PathCount            int     `json:"pathCount"`
	MaxCumulativeDB      float64 `json:"maxCumulativeDb"`
	PassAtOneDB          int     `json:"passAtOneDb"`
	PassAtPointOneDB     int     `json:"passAtPointOneDb"`
	PassAtPointZeroOneDB int     `json:"passAtPointZeroOneDb"`
}

type SuiteReport struct {
	Seed              int64                        `json:"seed"`
	Project           *acoustic.Project            `json:"project"`
	Preview           *PathOutput                  `json:"preview"`
	Resume            *PathOutput                  `json:"resume"`
	Full              *PathOutput                  `json:"full"`
	Comparisons       []PathComparison             `json:"comparisons"`
	AllPathsEqual     bool                         `json:"allPathsEqual"`
	Truth             acoustic.TruthComparison     `json:"truth"`
	Invalidation      *acoustic.InvalidationReport `json:"invalidation,omitempty"`
	InvalidationError string                       `json:"invalidationError,omitempty"`
	ErrorBounds       ErrorBoundSummary            `json:"errorBounds"`
}

func RunSuite(ctx context.Context, project *acoustic.Project, seed int64, baseDir string, workers acoustic.WorkerOptions) (*SuiteReport, error) {
	if project == nil {
		return nil, &acoustic.Error{Code: acoustic.CodeSchema, Op: "demo_suite", Message: "project is nil"}
	}
	if baseDir == "" {
		baseDir = os.TempDir()
	}
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, &acoustic.Error{Code: acoustic.CodeCheckpoint, Op: "demo_suite", Message: "cannot create database directory", Cause: err.Error()}
	}

	preview, err := runPreview(ctx, project, filepath.Join(baseDir, "preview-"+uniqueSuffix()+".db"), workers)
	if err != nil {
		return nil, err
	}
	resume, err := runResume(ctx, project, filepath.Join(baseDir, "resume-"+uniqueSuffix()+".db"), workers)
	if err != nil {
		return nil, err
	}
	full, err := runFull(ctx, project, filepath.Join(baseDir, "full-"+uniqueSuffix()+".db"), workers)
	if err != nil {
		return nil, err
	}

	report := &SuiteReport{
		Seed:    seed,
		Project: project,
		Preview: preview,
		Resume:  resume,
		Full:    full,
	}
	report.Comparisons = []PathComparison{
		comparePaths("preview", preview, "full", full),
		comparePaths("resume", resume, "full", full),
		comparePaths("preview", preview, "resume", resume),
	}
	report.AllPathsEqual = true
	for _, comparison := range report.Comparisons {
		if !comparison.Equal {
			report.AllPathsEqual = false
		}
	}
	report.Truth = acoustic.CompareTruth(project, full.Result.ListenerSolutions)
	report.ErrorBounds = summarizeBounds(full.Result.ListenerSolutions)
	if invalidation, err := demoInvalidation(project, preview.Result.ListenerSolutions); err != nil {
		report.InvalidationError = err.Error()
	} else {
		report.Invalidation = &invalidation
	}
	return report, nil
}

func runPreview(ctx context.Context, project *acoustic.Project, databasePath string, workers acoustic.WorkerOptions) (*PathOutput, error) {
	store, err := sqlstore.Open(databasePath)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	output := &PathOutput{Name: "preview", DatabasePath: databasePath}
	var mu sync.Mutex
	session, err := acoustic.NewSession(project, acoustic.SessionOptions{
		Store:         store,
		WorkerOptions: workers,
		AfterCommit: func(roomID, committed int) error {
			mu.Lock()
			output.ProgressEvents = append(output.ProgressEvents, ProgressEvent{
				RoomID:    roomID,
				Committed: committed,
				Progress:  acoustic.Progress{Committed: committed, Total: len(project.Rooms)},
			})
			mu.Unlock()
			return nil
		},
	})
	if err != nil {
		return nil, err
	}
	result, err := session.Run(ctx)
	output.Result = result
	if err != nil {
		output.Error = err.Error()
		return nil, err
	}
	return output, nil
}

func runResume(ctx context.Context, project *acoustic.Project, databasePath string, workers acoustic.WorkerOptions) (*PathOutput, error) {
	output := &PathOutput{Name: "resume", DatabasePath: databasePath}
	store, err := sqlstore.Open(databasePath)
	if err != nil {
		return nil, err
	}
	session, err := acoustic.NewSession(project, acoustic.SessionOptions{
		Store:         store,
		WorkerOptions: workers,
		AfterCommit: func(roomID, committed int) error {
			output.ProgressEvents = append(output.ProgressEvents, ProgressEvent{
				RoomID:    roomID,
				Committed: committed,
				Progress:  acoustic.Progress{Committed: committed, Total: len(project.Rooms)},
			})
			if committed == 1 && len(project.Rooms) > 1 {
				return errStopResumeSeed
			}
			return nil
		},
	})
	if err != nil {
		store.Close()
		return nil, err
	}
	partial, err := session.Run(ctx)
	output.Result = partial
	if !errors.Is(err, errStopResumeSeed) {
		store.Close()
		if err != nil {
			output.Error = err.Error()
			return nil, err
		}
		return nil, &acoustic.Error{Code: acoustic.CodeInternal, Op: "demo_resume", Message: "resume seed did not cancel after the first room"}
	}
	if err := store.Close(); err != nil {
		return nil, err
	}

	reopened, err := sqlstore.Open(databasePath)
	if err != nil {
		return nil, err
	}
	defer reopened.Close()
	resumedSession, err := acoustic.NewSession(project, acoustic.SessionOptions{
		Store:         reopened,
		WorkerOptions: workers,
	})
	if err != nil {
		return nil, err
	}
	restore, err := resumedSession.Restore(ctx)
	if err != nil {
		return nil, err
	}
	output.Restore = restore
	result, err := resumedSession.Run(ctx)
	output.Result = result
	if err != nil {
		output.Error = err.Error()
		return nil, err
	}
	return output, nil
}

func runFull(ctx context.Context, project *acoustic.Project, databasePath string, workers acoustic.WorkerOptions) (*PathOutput, error) {
	store, err := sqlstore.Open(databasePath)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	output := &PathOutput{Name: "full", DatabasePath: databasePath}
	session, err := acoustic.NewSession(project, acoustic.SessionOptions{
		Store:         store,
		WorkerOptions: workers,
		AfterCommit: func(roomID, committed int) error {
			output.ProgressEvents = append(output.ProgressEvents, ProgressEvent{
				RoomID:    roomID,
				Committed: committed,
				Progress:  acoustic.Progress{Committed: committed, Total: len(project.Rooms)},
			})
			return nil
		},
	})
	if err != nil {
		return nil, err
	}
	result, err := session.Run(ctx)
	output.Result = result
	if err != nil {
		output.Error = err.Error()
		return nil, err
	}
	return output, nil
}

func comparePaths(leftName string, left *PathOutput, rightName string, right *PathOutput) PathComparison {
	comparison := PathComparison{Left: leftName, Right: rightName}
	if left == nil || right == nil || left.Result == nil || right.Result == nil {
		comparison.Equal = false
		return comparison
	}
	comparison.Equal, comparison.Differences = acoustic.CompareRunResults(left.Result, right.Result)
	return comparison
}

func summarizeBounds(solutions []acoustic.ListenerSolution) ErrorBoundSummary {
	summary := ErrorBoundSummary{}
	summary.PathCount = acoustic.CountTolerancePasses(solutions, 1e12, 1e12)
	summary.PassAtOneDB = acoustic.CountTolerancePasses(solutions, 1, 1e12)
	summary.PassAtPointOneDB = acoustic.CountTolerancePasses(solutions, 0.1, 1e12)
	summary.PassAtPointZeroOneDB = acoustic.CountTolerancePasses(solutions, 0.01, 1e12)
	for _, solution := range solutions {
		for _, path := range solution.Paths {
			if path.ErrorPropagation.CumulativeUpperBoundDB > summary.MaxCumulativeDB {
				summary.MaxCumulativeDB = path.ErrorPropagation.CumulativeUpperBoundDB
			}
		}
	}
	return summary
}

func demoInvalidation(project *acoustic.Project, cached []acoustic.ListenerSolution) (acoustic.InvalidationReport, error) {
	newProject, err := cloneProject(project)
	if err != nil {
		return acoustic.InvalidationReport{}, err
	}
	found := false
	for _, wall := range newProject.Walls {
		if wall.ID != 101 {
			continue
		}
		for _, opening := range newProject.Openings {
			if opening.ID == 203 {
				found = true
				break
			}
		}
		if !found {
			newProject.Openings = append(newProject.Openings, acoustic.Opening{ID: 203, WallID: wall.ID, Start: 800, Width: 200})
			found = true
		}
	}
	if !found {
		return acoustic.PlanInvalidation(project, newProject, cached)
	}
	if err := newProject.Validate(); err != nil {
		return acoustic.InvalidationReport{}, err
	}
	return acoustic.PlanInvalidation(project, newProject, cached)
}

func cloneProject(project *acoustic.Project) (*acoustic.Project, error) {
	data, err := json.Marshal(project)
	if err != nil {
		return nil, err
	}
	var clone acoustic.Project
	if err := json.Unmarshal(data, &clone); err != nil {
		return nil, err
	}
	return &clone, nil
}

func uniqueSuffix() string {
	return fmt.Sprintf("%d", nextSequence())
}

var sequenceMu sync.Mutex
var sequence int64

func nextSequence() int64 {
	sequenceMu.Lock()
	defer sequenceMu.Unlock()
	sequence++
	return sequence
}

func SortRoomIDsForReport(ids []int) []int {
	result := append([]int(nil), ids...)
	sort.Ints(result)
	return result
}
