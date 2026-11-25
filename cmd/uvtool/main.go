package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"

	"uvenhance/internal/apperr"
	"uvenhance/internal/cache"
	"uvenhance/internal/engine"
	"uvenhance/internal/samplegen"
	"uvenhance/internal/yuv"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "gen":
		err = cmdGen(os.Args[2:])
	case "inspect":
		err = cmdInspect(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "edits":
		err = cmdEdits(os.Args[2:])
	case "verify", "compare":
		err = cmdVerify(os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		err = apperr.Meta("command", "unknown command "+os.Args[1])
	}
	if err != nil {
		raw, _ := json.MarshalIndent(err, "", "  ")
		fmt.Fprintln(os.Stderr, string(raw))
		os.Exit(exitCode(err))
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `uvtool - deterministic YUV 4:2:0 planar filter pipeline

commands:
  gen      generate fixed-seed samples and truth metadata
  inspect  load, validate and optionally verify truth for one sample
  run      run one parameter set (supports graceful cancel/restart)
  edits    apply a fixed edit sequence to one persistent cache
  verify   compare incremental, recovery and full recompute paths

run "uvtool <command> -h" for command flags.`)
}

func cmdGen(args []string) error {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	out := fs.String("out", "samples", "sample root directory")
	seed := fs.Uint64("seed", 20261125, "fixed PRNG seed")
	if err := fs.Parse(args); err != nil {
		return apperr.Meta("flags", err.Error())
	}
	ctx, stop := signalContext()
	defer stop()
	specs := []samplegen.Config{
		{ID: "game_patch_320x240", Width: 320, Height: 240},
		{ID: "game_patch_1608x904", Width: 1608, Height: 904},
	}
	for i, spec := range specs {
		dir := filepath.Join(*out, fmt.Sprintf("sample_%02d", i+1))
		md, err := samplegen.Generate(ctx, dir, spec, *seed+uint64(i))
		if err != nil {
			return err
		}
		fmt.Println(samplegen.Summary(md))
	}
	return nil
}

func cmdInspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	sampleDir := fs.String("sample", "samples/sample_01", "sample directory")
	checkTruth := fs.Bool("truth", true, "verify metadata truth against plane data")
	if err := fs.Parse(args); err != nil {
		return apperr.Meta("flags", err.Error())
	}
	sample, err := yuv.LoadSample(*sampleDir)
	if err != nil {
		return err
	}
	if *checkTruth {
		if err := yuv.ValidateTruth(context.Background(), sample); err != nil {
			return err
		}
	}
	out := map[string]any{
		"metadata": sample.Metadata,
		"hashes": map[string]string{
			"Y": sample.Y.ContentHash(), "U": sample.U.ContentHash(), "V": sample.V.ContentHash(),
		},
		"truth_validated": *checkTruth,
	}
	return printJSON(out)
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	sampleDir := fs.String("sample", "samples/sample_01", "sample directory")
	paramsPath := fs.String("params", "configs/default_params.json", "parameter JSON")
	dbPath := fs.String("db", "work/cache.db", "SQLite cache snapshot")
	outDir := fs.String("out", "work/run", "output directory")
	paramVersion := fs.Int("param-version", 1, "parameter schema version")
	alignmentVersion := fs.Int("alignment-version", 1, "chroma alignment version")
	workers := fs.Int("workers", 2, "maximum worker count")
	bufferMiB := fs.Int("buffer-mib", 128, "plane buffer cap per worker")
	cancelAfter := fs.Int("cancel-after-stages", 0, "gracefully cancel after N newly committed stages")
	heatMin := fs.Float64("heat-min", 0, "heatmap normalization minimum")
	heatMax := fs.Float64("heat-max", 0.25, "heatmap normalization maximum")
	if err := fs.Parse(args); err != nil {
		return apperr.Meta("flags", err.Error())
	}
	clearRunArtifacts(*outDir)
	sample, err := yuv.LoadSample(*sampleDir)
	if err != nil {
		return err
	}
	params, err := yuv.LoadParams(*paramsPath)
	if err != nil {
		return err
	}
	store, err := cache.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	ctx, stop := signalContext()
	defer stop()
	result, runErr := engine.New(store).Run(ctx, sample, params, engine.Options{
		ParamVersion: *paramVersion, AlignmentVersion: *alignmentVersion,
		Workers: *workers, BufferBytesPerWorker: int64(*bufferMiB) << 20,
		CancelAfterCommits: *cancelAfter,
	})
	if runErr != nil {
		if result != nil {
			_ = writeJSON(filepath.Join(*outDir, "partial_run.json"), runSummary(result))
		}
		return runErr
	}
	if err := engine.ValidateOutput(sample, result.Final); err != nil {
		return err
	}
	heat, err := engine.WriteArtifacts(ctx, *outDir, sample, result, *heatMin, *heatMax)
	if err != nil {
		return err
	}
	report, err := engine.BuildErrorReport(ctx, sample, params, result)
	if err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(*outDir, "error_report.json"), report); err != nil {
		return err
	}
	summary := runSummary(result)
	summary["heatmap"] = heat
	summary["error_report"] = "error_report.json"
	summary["final_digest"] = result.Final.Digest()
	if err := writeJSON(filepath.Join(*outDir, "run_report.json"), summary); err != nil {
		return err
	}
	return printJSON(summary)
}

func cmdEdits(args []string) error {
	fs := flag.NewFlagSet("edits", flag.ContinueOnError)
	sampleDir := fs.String("sample", "samples/sample_01", "sample directory")
	editsPath := fs.String("edits", "configs/fixed_edits.json", "fixed edit sequence")
	dbPath := fs.String("db", "work/edits.db", "SQLite cache snapshot")
	outDir := fs.String("out", "work/edits", "output directory")
	workers := fs.Int("workers", 2, "maximum worker count")
	bufferMiB := fs.Int("buffer-mib", 128, "plane buffer cap per worker")
	heatMin := fs.Float64("heat-min", 0, "heatmap normalization minimum")
	heatMax := fs.Float64("heat-max", 0.25, "heatmap normalization maximum")
	if err := fs.Parse(args); err != nil {
		return apperr.Meta("flags", err.Error())
	}
	clearRunArtifacts(*outDir)
	sample, err := yuv.LoadSample(*sampleDir)
	if err != nil {
		return err
	}
	seq, err := engine.LoadEditSequence(*editsPath)
	if err != nil {
		return err
	}
	store, err := cache.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	ctx, stop := signalContext()
	defer stop()
	eng := engine.New(store)
	var final *engine.RunResult
	steps := make([]map[string]any, 0, len(seq.Steps))
	for _, step := range seq.Steps {
		res, err := eng.RunLive(ctx, sample, step.Params, engine.Options{
			ParamVersion: seq.ParamVersion, AlignmentVersion: seq.AlignmentVersion,
			Workers: *workers, BufferBytesPerWorker: int64(*bufferMiB) << 20,
		})
		if err != nil {
			return err
		}
		final = res
		steps = append(steps, map[string]any{
			"name": step.Name, "invalidations": res.Invalidations,
			"recomputed": res.Recomputed, "cache_hits": res.CacheHits, "stats": res.Stats,
		})
	}
	heat, err := engine.WriteArtifacts(ctx, *outDir, sample, final, *heatMin, *heatMax)
	if err != nil {
		return err
	}
	report, err := engine.BuildErrorReport(ctx, sample, seq.Steps[len(seq.Steps)-1].Params, final)
	if err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(*outDir, "error_report.json"), report); err != nil {
		return err
	}
	out := map[string]any{
		"sequence": seq.Name, "steps": steps, "final_digest": final.Final.Digest(),
		"heatmap": heat, "snapshot_uid": final.SnapshotUID,
	}
	if err := writeJSON(filepath.Join(*outDir, "edits_report.json"), out); err != nil {
		return err
	}
	return printJSON(out)
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	sampleDir := fs.String("sample", "samples/sample_01", "sample directory")
	editsPath := fs.String("edits", "configs/fixed_edits.json", "fixed edit sequence")
	workDir := fs.String("workdir", "work/verify", "verification work directory")
	cancelStep := fs.Int("cancel-step", 2, "edit step that is interrupted")
	cancelAfter := fs.Int("cancel-after-stages", 1, "committed stages before graceful cancel")
	workers := fs.Int("workers", 2, "maximum worker count")
	bufferMiB := fs.Int("buffer-mib", 128, "plane buffer cap per worker")
	heatMin := fs.Float64("heat-min", 0, "heatmap normalization minimum")
	heatMax := fs.Float64("heat-max", 0.25, "heatmap normalization maximum")
	if err := fs.Parse(args); err != nil {
		return apperr.Meta("flags", err.Error())
	}
	seq, err := engine.LoadEditSequence(*editsPath)
	if err != nil {
		return err
	}
	ctx, stop := signalContext()
	defer stop()
	report, err := engine.VerifyThreePaths(ctx, *sampleDir, seq, engine.VerifyOptions{
		WorkDir: *workDir, CancelStep: *cancelStep, CancelAfterCommits: *cancelAfter,
		Workers: *workers, BufferMiB: *bufferMiB, HeatNormMin: *heatMin, HeatNormMax: *heatMax,
	})
	if err != nil {
		return err
	}
	return printJSON(report)
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func runSummary(result *engine.RunResult) map[string]any {
	keys := make(map[string]string, len(result.Stages))
	ids := make([]string, 0, len(result.Stages))
	for id := range result.Stages {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		keys[id] = result.Stages[id].Key
	}
	out := map[string]any{
		"complete": result.Complete, "snapshot_id": result.SnapshotID,
		"snapshot_uid": result.SnapshotUID, "invalidations": result.Invalidations,
		"recomputed": result.Recomputed, "cache_hits": result.CacheHits,
		"committed": result.Committed, "stats": result.Stats, "stage_keys": keys,
	}
	if result.Final != nil {
		out["output"] = map[string]any{
			"width": result.Final.Width, "height": result.Final.Height,
			"bit_depth": result.Final.BitDepth,
			"quant_min": result.Final.QuantMin, "quant_max": result.Final.QuantMax,
			"digest": result.Final.Digest(),
		}
	}
	return out
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return apperr.IO("create report directory", err)
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return apperr.IO("encode json", err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return apperr.IO("write json", err)
	}
	return nil
}

func clearRunArtifacts(dir string) {
	for _, name := range []string{
		"run_report.json", "partial_run.json", "edits_report.json", "error_report.json",
		"enhanced.yuv444p", "original.yuv444p", "difference.pgm",
	} {
		_ = os.Remove(filepath.Join(dir, name))
	}
}

func printJSON(value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return apperr.IO("encode output", err)
	}
	fmt.Println(string(raw))
	return nil
}

func exitCode(err error) int {
	switch apperr.Code(err) {
	case apperr.CodeMeta:
		return 2
	case apperr.CodePlane:
		return 3
	case apperr.CodePara:
		return 4
	case apperr.CodeDiff:
		return 5
	case apperr.CodeCache:
		return 6
	case apperr.CodeCanceled:
		return 7
	case apperr.CodeResource:
		return 8
	default:
		return 9
	}
}
