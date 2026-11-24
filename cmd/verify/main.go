package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"screenverify/internal/dataset"
	"screenverify/internal/experiment"
	"screenverify/internal/model"
	"screenverify/internal/rverr"
	"screenverify/internal/store"
	"screenverify/internal/verify"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "experiment" {
		experimentCmd(os.Args[2:])
		return
	}
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "run" {
		args = args[1:]
	}
	runCmd(args)
}

func runCmd(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	sample := fs.String("sample", filepath.Join("data", "sample", "noise"), "clip directory containing truth.json")
	dbPath := fs.String("db", filepath.Join("artifacts", "verify-run.db"), "SQLite checkpoint path")
	reportPath := fs.String("report", filepath.Join("artifacts", "report.json"), "JSON report output path")
	parallelism := fs.Int("parallelism", 2, "maximum evidence worker parallelism")
	bufferPixels := fs.Int64("buffer-pixels-per-thread", verify.DefaultRuntime().BufferPixelsPerThread, "per-thread scratch pixel buffer cap")
	blockSize := fs.Int("block-size", verify.DefaultParams().BlockSize, "square noise block size; must divide frame width/height")
	window := fs.Int("window", verify.DefaultParams().ResidualWindow, "previous-frame residual window size")
	gridX := fs.Int("grid-x", verify.DefaultParams().GridX, "watermark sampling grid columns")
	gridY := fs.Int("grid-y", verify.DefaultParams().GridY, "watermark sampling grid rows")
	threshold := fs.Float64("threshold", verify.DefaultParams().TamperThreshold, "final tamper conviction threshold in [0,1]")
	wmThreshold := fs.Float64("watermark-threshold", verify.DefaultParams().WatermarkThreshold, "watermark confidence threshold in [0,1]")
	residualZ := fs.Float64("residual-z-threshold", verify.DefaultParams().ResidualZThreshold, "residual Z-score decision threshold")
	noiseZ := fs.Float64("noise-z-threshold", verify.DefaultParams().NoiseZThreshold, "block noise Z-score outlier threshold")
	edgeThreshold := fs.Float64("edge-threshold", verify.DefaultParams().EdgeThreshold, "edge consistency threshold in [0,1]")
	wmRegion := fs.String("watermark-region", "", "optional override x,y,w,h for corrected watermark region")
	batchFrames := fs.Int("batch-frames", verify.DefaultParams().BatchFrames, "frames per committed checkpoint batch")
	failOnce := fs.String("inject-failure-once", "", "optional frame:evidence_type transient fault, e.g. 8:noise_variance")
	retry := fs.Bool("retry-failed", true, "retry injected/transient failed evidence")
	fresh := fs.Bool("fresh", true, "remove the local SQLite file before a reproducible one-shot run")
	_ = fs.Parse(args)

	params := verify.DefaultParams()
	params.BlockSize = *blockSize
	params.ResidualWindow = *window
	params.GridX = *gridX
	params.GridY = *gridY
	params.TamperThreshold = *threshold
	params.WatermarkThreshold = *wmThreshold
	params.ResidualZThreshold = *residualZ
	params.NoiseZThreshold = *noiseZ
	params.EdgeThreshold = *edgeThreshold
	params.BatchFrames = *batchFrames
	if *wmRegion != "" {
		parts := strings.Split(*wmRegion, ",")
		if len(parts) != 4 {
			fatal(rverr.Field(rverr.EPara, "watermark_region", "expected x,y,width,height"))
		}
		values := make([]int, 4)
		for i, part := range parts {
			v, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil {
				fatal(rverr.Field(rverr.EPara, "watermark_region", "all coordinates must be integers"))
			}
			values[i] = v
		}
		params.WatermarkRegion = &model.Rect{X: values[0], Y: values[1], Width: values[2], Height: values[3]}
	}
	rt := verify.RuntimeConfig{Parallelism: *parallelism, BufferPixelsPerThread: *bufferPixels}
	if *failOnce != "" {
		frame, typ, err := parseFault(*failOnce)
		if err != nil {
			fatal(err)
		}
		rt.FailureArmed = true
		rt.FailureFrame = frame
		rt.FailureType = typ
	}

	ctx := context.Background()
	if *fresh {
		_ = os.Remove(*dbPath)
		_ = os.Remove(*dbPath + "-wal")
		_ = os.Remove(*dbPath + "-shm")
	}
	set, err := dataset.Load(*sample)
	if err != nil {
		fatal(err)
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		fatal(rverr.Wrap(rverr.EStore, "open sqlite", err))
	}
	defer st.Close()
	eng, err := verify.NewEngine(set, params, rt, st)
	if err != nil {
		fatal(err)
	}
	if _, err := eng.EnsureIdentity(ctx, "cli-run-"+verify.ParamVersion(params)); err != nil {
		fatal(err)
	}
	for b := 0; b < eng.BatchCount(); b++ {
		_, err := eng.RunBatch(ctx, b)
		if err != nil && *retry && rverr.CodeOf(err) == rverr.EFault {
			if rerr := eng.RetryFailedBatch(ctx, b); rerr != nil {
				fatal(rerr)
			}
			continue
		}
		if err != nil {
			fatal(err)
		}
	}
	report := eng.BuildReport(nil)
	if err := writeJSON(*reportPath, report); err != nil {
		fatal(err)
	}
	fmt.Printf("clip=%s frames=%d decisions=%d intervals=%d recompute=%d hits=%d report=%s db=%s\n",
		report.Clip, len(report.Decisions), len(report.Decisions), len(report.Intervals), report.Stats.RecomputeCount,
		report.Stats.HitCount, *reportPath, *dbPath)
	for _, iv := range report.Intervals {
		fmt.Printf("interval %s %d-%d kinds=%v\n", iv.Kind, iv.StartFrame, iv.EndFrame, iv.EvidenceTypes)
	}
}

func experimentCmd(args []string) {
	fs := flag.NewFlagSet("experiment", flag.ExitOnError)
	dataDir := fs.String("data", filepath.Join("data", "sample"), "generated dataset directory")
	outPath := fs.String("out", filepath.Join("artifacts", "experiment-report.json"), "experiment JSON output")
	dbDir := fs.String("db-dir", filepath.Join("artifacts", "experiment-dbs"), "directory for experiment SQLite files")
	parallelism := fs.Int("parallelism", 4, "maximum parallelism")
	bufferPixels := fs.Int64("buffer-pixels-per-thread", 24576, "per-thread pixel buffer cap")
	blockSize := fs.Int("block-size", 16, "initial block size")
	threshold := fs.Float64("threshold", verify.DefaultParams().TamperThreshold, "tamper threshold")
	cancelAfter := fs.Int("cancel-after", 2, "committed batches before simulated restart")
	_ = fs.Parse(args)

	if err := os.RemoveAll(*dbDir); err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(*dbDir, 0o755); err != nil {
		fatal(err)
	}
	base := verify.DefaultParams()
	base.BlockSize = *blockSize
	base.TamperThreshold = *threshold
	rt := verify.RuntimeConfig{Parallelism: *parallelism, BufferPixelsPerThread: *bufferPixels}
	ctx := context.Background()

	clips := []string{"noise", "watermark", "clean"}
	type clipResult struct {
		ThreeWay   *experiment.ThreeWayReport `json:"three_way"`
		TruthMatch []bool                     `json:"truth_interval_matches"`
	}
	report := struct {
		OK         bool                          `json:"ok"`
		Clips      map[string]clipResult         `json:"clips"`
		Injections []*experiment.InjectionResult `json:"injections"`
		Errors     []string                      `json:"errors,omitempty"`
	}{OK: true, Clips: map[string]clipResult{}}

	for _, clip := range clips {
		dir := filepath.Join(*dataDir, clip)
		tw, err := experiment.RunThreeWay(ctx, dir, *dbDir, base, rt, *cancelAfter)
		cr := clipResult{ThreeWay: tw}
		if err != nil {
			report.OK = false
			report.Errors = append(report.Errors, clip+": "+err.Error())
		}
		set, lerr := dataset.Load(dir)
		if lerr == nil && tw != nil {
			for _, iv := range tw.Realtime.Report.Intervals {
				cr.TruthMatch = append(cr.TruthMatch, verify.IntervalMatchesTruth(iv, set.Truth))
			}
		}
		report.Clips[clip] = cr
	}

	for _, c := range experiment.DefaultInjectionCases(base) {
		res, err := experiment.RunInjection(ctx, filepath.Join(*dataDir, "noise"), *dbDir, rt, c)
		if err != nil {
			report.OK = false
			if res != nil && res.Diff != nil {
				report.Errors = append(report.Errors, "injection "+c.Name+": "+res.Diff.Message)
			} else {
				report.Errors = append(report.Errors, "injection "+c.Name+": "+err.Error())
			}
		}
		if res != nil {
			report.Injections = append(report.Injections, res)
		}
	}

	if err := writeJSON(*outPath, report); err != nil {
		fatal(err)
	}
	fmt.Printf("experiment ok=%v clips=%d injections=%d report=%s\n", report.OK, len(clips), len(report.Injections), *outPath)
	if !report.OK {
		for _, msg := range report.Errors {
			fmt.Fprintln(os.Stderr, msg)
		}
		os.Exit(2)
	}
}

func parseFault(s string) (int, verify.EvidenceType, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, "", rverr.Field(rverr.EPara, "inject-failure-once", "expected frame:evidence_type")
	}
	frame, err := strconv.Atoi(parts[0])
	if err != nil || frame <= 0 {
		return 0, "", rverr.Field(rverr.EPara, "inject-failure-once", "frame must be positive integer")
	}
	typ := verify.EvidenceType(parts[1])
	switch typ {
	case verify.EvWatermark, verify.EvTimeline, verify.EvResidual, verify.EvNoise, verify.EvEdge, verify.EvDecision:
	default:
		return 0, "", rverr.Field(rverr.EPara, "inject-failure-once", "unknown evidence type")
	}
	return frame, typ, nil
}

func writeJSON(path string, v any) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func fatal(err error) {
	if e := rverr.As(err); e != nil {
		b, _ := json.MarshalIndent(e, "", "  ")
		fmt.Fprintln(os.Stderr, string(b))
		os.Exit(2)
	}
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
