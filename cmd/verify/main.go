package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"tacticalworkbench/pkg/tactical"
	"tacticalworkbench/pkg/workbench"
)

type windowDigest struct {
	MemberID string `json:"member_id"`
	Hash     string `json:"hash"`
	States   int    `json:"states"`
}

type report struct {
	Scenario             string                      `json:"scenario"`
	Database             string                      `json:"database"`
	InitialFullMicros    int64                       `json:"initial_full_micros"`
	IncrementalMicros    int64                       `json:"incremental_micros"`
	RestoreMicros        int64                       `json:"restore_micros"`
	FullRecomputeMicros  int64                       `json:"full_recompute_micros"`
	Invalidation         tactical.InvalidationReport `json:"invalidation"`
	CacheHits            int                         `json:"cache_hits"`
	CacheMisses          int                         `json:"cache_misses"`
	ReusedIntervals      int                         `json:"reused_intervals"`
	IncrementalVsRestore string                      `json:"incremental_vs_restore"`
	IncrementalVsFull    string                      `json:"incremental_vs_full"`
	RestoreVsFull        string                      `json:"restore_vs_full"`
	Checkpoint           tactical.CheckpointMeta     `json:"checkpoint"`
	Scheduler            tactical.SchedulerStats     `json:"scheduler"`
	SchedulerCommitOrder bool                        `json:"scheduler_commit_order_equal"`
	SchedulerValuesEqual bool                        `json:"scheduler_values_equal"`
	ParallelCommits      []string                    `json:"parallel_commits"`
	SerialCommits        []string                    `json:"serial_commits"`
	MaxParallelBudget    int                         `json:"max_parallel_budget"`
	StateBufferBudget    int                         `json:"state_buffer_budget"`
	CommitCallbackSerial bool                        `json:"commit_callback_serial"`
	Success              bool                        `json:"success"`
}

func main() {
	scenarioPath := flag.String("scenario", filepath.Join("data", "scenario.json"), "scenario JSON path")
	dbPath := flag.String("db", filepath.Join("artifacts", "acceptance.db"), "SQLite checkpoint path")
	reportPath := flag.String("out", filepath.Join("artifacts", "acceptance-report.json"), "acceptance report path")
	maxParallel := flag.Int("parallel", 4, "maximum simulation window parallelism")
	bufferLimit := flag.Int("buffer", 4, "per-worker state buffer limit")
	flag.Parse()

	result, err := run(context.Background(), *scenarioPath, *dbPath, *maxParallel, *bufferLimit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "acceptance failed:", err)
		if result != nil {
			payload, _ := json.MarshalIndent(result, "", "  ")
			fmt.Fprintln(os.Stdout, string(payload))
		}
		os.Exit(1)
	}
	payload, _ := json.MarshalIndent(result, "", "  ")
	if err := os.MkdirAll(filepath.Dir(*reportPath), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "create report directory failed:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*reportPath, append(payload, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write report failed:", err)
		os.Exit(1)
	}
	fmt.Println(string(payload))
}

func run(ctx context.Context, scenarioPath, dbPath string, maxParallel, bufferLimit int) (*report, error) {
	scenario, err := tactical.LoadScenarioFile(scenarioPath)
	if err != nil {
		return nil, err
	}
	cleanupSQLite(dbPath)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}
	result := &report{
		Scenario: scenarioPath, Database: dbPath,
		MaxParallelBudget: maxParallel, StateBufferBudget: bufferLimit,
		IncrementalVsRestore: "not-run", IncrementalVsFull: "not-run", RestoreVsFull: "not-run",
	}
	engine, err := tactical.NewEngine(*scenario, workbench.DefaultEngineOptions(*scenario))
	if err != nil {
		return result, err
	}
	initialStart := time.Now()
	if _, err := engine.FullRebuild(ctx); err != nil {
		return result, err
	}
	result.InitialFullMicros = time.Since(initialStart).Microseconds()
	store, err := tactical.OpenCheckpointStore(dbPath)
	if err != nil {
		return result, err
	}
	editStart := time.Now()
	edit, err := engine.ApplyRegionEdit(ctx, "bridge-lock", true)
	if err != nil {
		store.Close()
		return result, err
	}
	result.IncrementalMicros = time.Since(editStart).Microseconds()
	result.Invalidation = edit.Invalidation
	result.CacheHits = edit.Stats.CacheHits
	result.CacheMisses = edit.Stats.CacheMisses
	result.ReusedIntervals = edit.Stats.ReusedIntervals
	checkpointMeta, err := engine.SaveCheckpoint(ctx, store)
	if err != nil {
		store.Close()
		return result, err
	}
	result.Checkpoint = checkpointMeta
	if err := store.Close(); err != nil {
		return result, err
	}
	incremental := edit.Snapshot

	restoreStart := time.Now()
	store, err = tactical.OpenCheckpointStore(dbPath)
	if err != nil {
		return result, err
	}
	editedScenario := engine.Scenario()
	restoreEngine, err := tactical.NewEngine(editedScenario, workbench.DefaultEngineOptions(editedScenario))
	if err != nil {
		store.Close()
		return result, err
	}
	resumeMeta, err := restoreEngine.LoadCheckpoint(ctx, store)
	if err != nil {
		store.Close()
		return result, err
	}
	if !resumeMeta.Valid {
		store.Close()
		return result, fmt.Errorf("checkpoint was stale: %s", resumeMeta.StaleReason)
	}
	restored := restoreEngine.Snapshot()
	result.RestoreMicros = time.Since(restoreStart).Microseconds()
	_ = store.Close()

	fullStart := time.Now()
	fullEngine, err := tactical.NewEngine(editedScenario, workbench.DefaultEngineOptions(editedScenario))
	if err != nil {
		return result, err
	}
	full, err := fullEngine.FullRebuild(ctx)
	if err != nil {
		return result, err
	}
	result.FullRecomputeMicros = time.Since(fullStart).Microseconds()

	if err := tactical.CompareSnapshots(incremental, restored); err != nil {
		result.IncrementalVsRestore = err.Error()
		return result, err
	}
	result.IncrementalVsRestore = "equal"
	if err := tactical.CompareSnapshots(incremental, full); err != nil {
		result.IncrementalVsFull = err.Error()
		return result, err
	}
	result.IncrementalVsFull = "equal"
	if err := tactical.CompareSnapshots(restored, full); err != nil {
		result.RestoreVsFull = err.Error()
		return result, err
	}
	result.RestoreVsFull = "equal"

	parallelCommits, parallelStats, parallelValues, err := runScheduler(ctx, incremental, maxParallel, bufferLimit)
	if err != nil {
		return result, err
	}
	serialCommits, _, serialValues, err := runScheduler(ctx, incremental, 1, bufferLimit)
	if err != nil {
		return result, err
	}
	result.Scheduler = parallelStats
	result.ParallelCommits = parallelCommits
	result.SerialCommits = serialCommits
	result.SchedulerCommitOrder = equalStrings(parallelCommits, serialCommits)
	result.SchedulerValuesEqual = equalDigests(parallelValues, serialValues)
	result.CommitCallbackSerial = true
	result.Success = result.SchedulerCommitOrder && result.SchedulerValuesEqual && parallelStats.PeakParallel <= maxParallel
	if !result.Success {
		return result, fmt.Errorf("scheduler consistency or resource budget check failed")
	}
	return result, nil
}

func runScheduler(ctx context.Context, snapshot *tactical.Snapshot, parallel, bufferLimit int) ([]string, tactical.SchedulerStats, []windowDigest, error) {
	scheduler, err := tactical.NewWindowScheduler(parallel, bufferLimit)
	if err != nil {
		return nil, tactical.SchedulerStats{}, nil, err
	}
	windows := make([]tactical.WindowSpec, 0, len(snapshot.MemberOrder))
	for i, memberID := range snapshot.MemberOrder {
		end := 0
		if trace, ok := snapshot.Traces[memberID]; ok && len(trace.States) > 0 {
			end = len(trace.States) - 1
		}
		windows = append(windows, tactical.WindowSpec{
			Index: i, MemberIDs: []string{memberID}, StartTick: 0, EndTick: end,
			Label: "member-" + memberID,
		})
	}
	var commits []string
	var commitConcurrent atomic.Int64
	var commitSerial atomic.Bool
	commitSerial.Store(true)
	values, stats, err := tactical.RunWindows(
		ctx, scheduler, windows,
		func(ctx context.Context, window tactical.WindowSpec, buffer *tactical.StateBuffer) (windowDigest, error) {
			if len(window.MemberIDs) != 1 {
				return windowDigest{}, fmt.Errorf("window %d must contain one member", window.Index)
			}
			memberID := window.MemberIDs[0]
			trace := snapshot.Traces[memberID]
			sum := sha256.New()
			count := 0
			for tick := window.StartTick; tick <= window.EndTick && tick < len(trace.States); tick++ {
				state := trace.States[tick]
				if err := buffer.Push(ctx, state); err != nil {
					return windowDigest{}, err
				}
				fmt.Fprintf(sum, "%d|%d|%d|%d|%d|%d\n", state.Tick, state.Position.X, state.Position.Y,
					state.HeadingMilli, state.Segment, state.FormationDeviationMilli)
				count++
				if buffer.Len() >= bufferLimit {
					_, _ = buffer.Pop()
				}
				if tick%8 == 0 {
					time.Sleep(250 * time.Microsecond)
				}
			}
			return windowDigest{MemberID: memberID, Hash: hex.EncodeToString(sum.Sum(nil)), States: count}, nil
		},
		func(ctx context.Context, window tactical.WindowSpec, value windowDigest) error {
			if commitConcurrent.Add(1) != 1 {
				commitSerial.Store(false)
			}
			defer commitConcurrent.Add(-1)
			commits = append(commits, fmt.Sprintf("%d:%s", window.Index, value.MemberID))
			time.Sleep(100 * time.Microsecond)
			return nil
		},
	)
	if err != nil {
		return nil, stats, values, err
	}
	if !commitSerial.Load() {
		return nil, stats, values, fmt.Errorf("commit callback executed concurrently")
	}
	return commits, stats, values, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalDigests(a, b []windowDigest) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func cleanupSQLite(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
}
