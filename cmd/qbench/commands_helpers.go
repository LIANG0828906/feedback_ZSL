package main

import (
	"context"
	"time"

	"qualitybench/internal/checkpoint"
	"qualitybench/internal/compare"
	"qualitybench/internal/dataset"
	"qualitybench/internal/pipeline"
	"qualitybench/internal/runner"
	"qualitybench/internal/verify"
)

func runnerConfig(runID string, mode pipeline.Mode, workers int, threadBuffer, buffer int64,
	persistent bool, stopAfter, delayMS int) runner.Config {
	return runner.Config{
		RunID: runID, Mode: mode, Workers: workers,
		MaxThreadBufferBytes: threadBuffer, BufferCapacityBytes: buffer,
		PersistentCache: persistent, StopAfter: stopAfter,
		TaskDelay: time.Duration(delayMS) * time.Millisecond,
	}
}

func runAllWithRunner(ctx context.Context, ds *dataset.Dataset, st checkpoint.Store,
	cfg runner.Config, imageIDs []string) ([]*pipeline.Result, error) {
	return runner.RunImages(ctx, ds, st, cfg, imageIDs)
}

func compareRuns(ctx context.Context, st checkpoint.Store, runA, runB string,
	imageIDs []string) (*compare.Report, error) {
	return compare.Runs(ctx, st, runA, runB, imageIDs)
}

func verifyRun(ctx context.Context, st checkpoint.Store, runID, truthPath string) (*verify.Report, error) {
	truth, fp, err := dataset.LoadTruth(truthPath)
	if err != nil {
		return nil, err
	}
	return verify.Run(ctx, st, runID, truth, fp)
}
