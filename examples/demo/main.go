package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"audiofp"
	"audiofp/samplegen"
)

func main() {
	dataDir := flag.String("data", "data", "sample data directory")
	keepDB := flag.Bool("keep-db", false, "keep demo SQLite checkpoint instead of replacing it")
	flag.Parse()

	ctx := context.Background()
	material, pcmPath, manifestPath, err := samplegen.WriteDefaultDataset(*dataDir)
	must(err)
	fmt.Printf("sample: %s\npcm=%s\nmanifest=%s\n", material.PCM.ID, pcmPath, manifestPath)

	loaded, err := audiofp.LoadMaterial(pcmPath, manifestPath)
	must(err)

	params := audiofp.DefaultParams()
	baseLimits := audiofp.DefaultResourceLimits()

	concurrencyResults := make([]*audiofp.AnnotationResult, 0, 4)
	for _, workers := range []int{1, 2, 4, 8} {
		limits := baseLimits
		limits.MaxWorkers = workers
		result, err := audiofp.AnnotateFull(ctx, loaded, params, limits)
		must(err)
		concurrencyResults = append(concurrencyResults, result)
		fmt.Printf("concurrency=%d frames=%d events=%d quality=%v\n",
			workers, len(result.Frames), len(result.Intervals), result.Quality.Passed)
	}
	if _, err := audiofp.EnsureIdentical(concurrencyResults...); err != nil {
		fatalf("concurrency fingerprints differ: %v", err)
	}
	fmt.Println("concurrency check: identical frames/events/quality")

	realtime, err := audiofp.AnnotateRealtime(ctx, loaded, params, baseLimits)
	must(err)
	fmt.Printf("realtime: frames=%d events=%d passed=%v accuracy=%.3f\n",
		len(realtime.Frames), len(realtime.Intervals), realtime.Quality.Passed, realtime.Quality.CategoryAccuracy)

	dbPath := filepath.Join(*dataDir, "demo-checkpoint.db")
	if !*keepDB {
		_ = os.Remove(dbPath)
	}
	identity := audiofp.CheckpointIdentity{
		MaterialHash: loaded.ContentHash(), ParamVersion: params.Version(),
		AlgorithmVersion: audiofp.AlgorithmVersion,
	}
	store, snapshot, err := audiofp.OpenAndResumeCheckpoint(ctx, dbPath, identity)
	must(err)

	stopAfter := 40
	partial, err := audiofp.CheckpointRealtime(ctx, loaded, params, baseLimits, store, func(index int) error {
		if index == stopAfter {
			return audiofp.ErrStopAfterWindow
		}
		return nil
	})
	if !errors.Is(err, audiofp.ErrStopAfterWindow) {
		must(err)
	}
	fmt.Printf("simulated stop after window #%d: durable_prefix=%d reset=%v\n",
		stopAfter, partial.RecoveredWindows, snapshot.Reset)
	must(store.Close())

	resumed, err := audiofp.AnnotateResume(ctx, loaded, params, baseLimits, dbPath)
	must(err)
	fmt.Printf("resume: recovered=%d frames=%d events=%d passed=%v reset=%v\n",
		resumed.RecoveredWindows, len(resumed.Frames), len(resumed.Intervals),
		resumed.Quality.Passed, resumed.SnapshotReset)

	report, err := audiofp.EnsureIdentical(concurrencyResults[len(concurrencyResults)-1], realtime, resumed)
	if err != nil {
		if de, ok := audiofp.AsError(err); ok && de.Code == audiofp.CodeDiff {
			detail := de.Details.(*audiofp.DiffReport).FirstDiff
			fatalf("three-path mismatch at %s: %s", detail.Field, detail.Message)
		}
		fatalf("three-path comparison failed: %v", err)
	}
	fmt.Printf("three-path check: consistent frames=%d events=%d\n", report.CheckedFrames, report.CheckedEvents)

	for _, c := range resumed.Quality.Comparisons {
		fmt.Printf("event #%d %-6s [%d,%d) -> %-6s [%d,%d) delta=(%+d,%+d) pass=%v boundary=%v conf=%.3f\n",
			c.EventNo, c.CategoryExpected, c.StartExpected, c.EndExpected,
			c.CategoryActual, c.StartActual, c.EndActual, c.StartDelta, c.EndDelta,
			c.Passed, c.BoundaryAggregation, c.Confidence)
	}
	fmt.Printf("quality: passed=%v category_accuracy=%.3f mean_abs_start=%.1f mean_abs_end=%.1f boundary_deviations=%d\n",
		resumed.Quality.Passed, resumed.Quality.CategoryAccuracy,
		resumed.Quality.MeanAbsStartError, resumed.Quality.MeanAbsEndError,
		len(resumed.Quality.BoundaryDeviations))

	exportPath := filepath.Join(*dataDir, "annotation.json")
	data, err := json.MarshalIndent(resumed, "", "  ")
	must(err)
	must(os.WriteFile(exportPath, data, 0o644))
	fmt.Printf("exported %s\n", exportPath)
}

func must(err error) {
	if err != nil {
		fatalf("%v", err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
