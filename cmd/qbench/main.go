package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"qualitybench/internal/core"
	"qualitybench/internal/dataset"
	"qualitybench/internal/generator"
	"qualitybench/internal/pipeline"
	"qualitybench/internal/store"
)

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = cmdGenerate(os.Args[2:])
	case "validate":
		err = cmdValidate(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "compare":
		err = cmdCompare(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "accept":
		err = cmdAccept(os.Args[2:])
	case "version":
		fmt.Println("qbench", version)
		return
	case "-h", "--help", "help":
		usage()
		return
	default:
		err = core.Para(0, "command", "unknown command %q", os.Args[1])
	}
	if err != nil {
		printError(err)
		os.Exit(exitCode(err))
	}
}

func usage() {
	fmt.Print(`qbench - deterministic screenshot degradation benchmark

Commands:
  generate  generate screenshots, project, ordered chains, edits and truth
  validate  validate project/truth schema, ranges, block grid and baseline hashes
  run       execute one project image chain into SQLite checkpoints
  compare   compare persisted block artifacts from two runs
  verify    recompute metrics/attribution from persisted blocks and truth only
  serve     serve the local diagnostic page
  accept    run the complete local acceptance suite and write a report

Run "qbench <command> -h" for command flags.
`)
}

func cmdGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	out := fs.String("out", "samples", "output directory")
	seed := fs.Int64("seed", 424242, "deterministic seed")
	images := fs.Int("images", 3, "number of frames")
	width := fs.Int("width", 192, "frame width")
	height := fs.Int("height", 128, "frame height")
	block := fs.Int("block-size", 16, "block size")
	share := fs.Float64("share", 0.22, "attribution share")
	tol := fs.Float64("metric-tolerance", 0.08, "metric interval tolerance")
	if err := fs.Parse(args); err != nil {
		return core.Schema("args", "%v", err)
	}
	opts := generator.DefaultOptions()
	opts.Seed, opts.ImageCount, opts.Width, opts.Height = *seed, *images, *width, *height
	opts.BlockSize, opts.AttributionShare, opts.MetricTolerance = *block, *share, *tol
	ds, _, err := generator.WriteDataset(*out, opts)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{
		"out":                 *out,
		"project_fingerprint": ds.ProjectFP,
		"truth_fingerprint":   ds.TruthFP,
		"images":              len(ds.Project.Images),
		"block_size":          ds.Project.BlockSize,
		"steps_per_image":     len(ds.Project.Chains[0].Steps),
	})
}

func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	project := fs.String("project", "samples/project.json", "project JSON")
	truth := fs.String("truth", "samples/truth.json", "truth JSON")
	if err := fs.Parse(args); err != nil {
		return core.Schema("args", "%v", err)
	}
	ds, err := dataset.Load(*project, *truth)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{
		"valid":               true,
		"images":              len(ds.Project.Images),
		"blocks_per_image":    (ds.Project.Width / ds.Project.BlockSize) * (ds.Project.Height / ds.Project.BlockSize),
		"project_fingerprint": ds.ProjectFP,
		"truth_fingerprint":   ds.TruthFP,
	})
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	project := fs.String("project", "samples/project.json", "project JSON")
	truth := fs.String("truth", "samples/truth.json", "truth JSON")
	dbPath := fs.String("db", "artifacts/qbench.db", "SQLite database")
	runID := fs.String("run-id", "live", "run identifier")
	mode := fs.String("mode", "full", "live, resume or full")
	imageIDs := fs.String("images", "", "comma-separated image ids (default all)")
	workers := fs.Int("workers", 4, "worker count")
	buffer := fs.Int64("buffer-bytes", 4<<20, "shared pixel buffer budget")
	threadBuffer := fs.Int64("thread-buffer-bytes", 1<<20, "per-thread pixel buffer limit")
	stopAfter := fs.Int("stop-after", 0, "stop after N block-step commits")
	persistent := fs.Bool("persistent-cache", true, "read content-addressed cache from SQLite")
	delayMS := fs.Int("delay-ms", 0, "artificial per-task delay for diagnostics")
	if err := fs.Parse(args); err != nil {
		return core.Schema("args", "%v", err)
	}
	ds, err := dataset.Load(*project, *truth)
	if err != nil {
		return err
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	r := runnerConfig(*runID, pipeline.Mode(*mode), *workers, *threadBuffer, *buffer,
		*persistent, *stopAfter, *delayMS)
	ids := splitIDs(*imageIDs)
	ctx := context.Background()
	results, err := runAllWithRunner(ctx, ds, st, r, ids)
	if err != nil {
		return err
	}
	return printJSON(summarizeResults(results))
}

func cmdCompare(args []string) error {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	dbPath := fs.String("db", "artifacts/qbench.db", "SQLite database")
	runA := fs.String("run-a", "live", "first run")
	runB := fs.String("run-b", "full", "second run")
	images := fs.String("images", "", "comma-separated image ids")
	if err := fs.Parse(args); err != nil {
		return core.Schema("args", "%v", err)
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	report, err := compareRuns(context.Background(), st, *runA, *runB, splitIDs(*images))
	if report != nil {
		if printErr := printJSON(report); printErr != nil {
			return printErr
		}
	}
	return err
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	dbPath := fs.String("db", "artifacts/qbench.db", "SQLite database")
	runID := fs.String("run-id", "live", "run identifier")
	truthPath := fs.String("truth", "samples/truth.json", "truth JSON")
	if err := fs.Parse(args); err != nil {
		return core.Schema("args", "%v", err)
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	report, err := verifyRun(context.Background(), st, *runID, *truthPath)
	if report != nil {
		if printErr := printJSON(report); printErr != nil {
			return printErr
		}
	}
	return err
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func printError(err error) {
	if e, ok := core.As(err); ok {
		fmt.Fprintln(os.Stderr, e.JSON())
		return
	}
	fmt.Fprintln(os.Stderr, err)
}

func exitCode(err error) int {
	switch core.CodeOf(err) {
	case core.CodeDiff:
		return 9
	case core.CodeState:
		return 8
	case core.CodeStage:
		return 7
	case core.CodeBlock:
		return 6
	case core.CodePara:
		return 5
	case core.CodeRange:
		return 4
	case core.CodeSchema:
		return 3
	case core.CodeCanceled:
		return 130
	default:
		return 1
	}
}

func splitIDs(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func summarizeResults(results []*pipeline.Result) []map[string]any {
	out := make([]map[string]any, 0, len(results))
	for _, r := range results {
		out = append(out, map[string]any{
			"run_id":            r.RunID,
			"image_id":          r.ImageID,
			"mode":              r.Mode,
			"status":            r.Status,
			"completed":         r.Completed,
			"interrupted":       r.Interrupted,
			"invalidated":       r.Invalidated,
			"fallback_full":     r.FallbackFull,
			"chain_fingerprint": r.ChainFingerprint,
			"cache":             r.Cache,
			"buffer_waits":      len(r.BufferWaits),
			"queue_waits":       len(r.QueueWaits),
			"elapsed_ns":        r.Elapsed,
			"version_id":        r.VersionID,
		})
	}
	return out
}

func isCanceled(err error) bool {
	return errors.Is(err, context.Canceled) || core.CodeOf(err) == core.CodeCanceled
}
