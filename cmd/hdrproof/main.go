package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"hdrproof/internal/apperr"
	"hdrproof/internal/audit"
	"hdrproof/internal/compare"
	"hdrproof/internal/engine"
	"hdrproof/internal/exporter"
	"hdrproof/internal/model"
	"hdrproof/internal/sample"
	"hdrproof/internal/verify"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "generate":
		err = cmdGenerate(os.Args[2:])
	case "run":
		err = cmdRun(ctx, os.Args[2:])
	case "audit":
		err = cmdAudit(ctx, os.Args[2:])
	case "compare":
		err = cmdCompare(ctx, os.Args[2:])
	case "export":
		err = cmdExport(ctx, os.Args[2:])
	case "demo":
		err = cmdDemo(ctx, os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		printError(err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `hdrproof - deterministic HDR to SDR proof pipeline

usage:
  hdrproof generate --out DIR [--seed 42] [--routes 4]
  hdrproof run --samples DIR --db FILE --mode realtime|resume|full [options]
  hdrproof audit --samples DIR --db FILE
  hdrproof compare --left DB --right DB
  hdrproof export --db FILE --out DIR [--samples DIR]
  hdrproof demo [--root artifacts/demo] [--workers 4] [--buffer 2]`)
}

func cmdGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	out := fs.String("out", filepath.Join("artifacts", "demo", "samples"), "sample output directory")
	seed := fs.Int64("seed", 42, "deterministic seed")
	routes := fs.Int("routes", 4, "route count (1..4)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	report, err := sample.Generate(sample.GenerateOptions{Dir: *out, Seed: *seed, Routes: *routes})
	if err != nil {
		return err
	}
	return printJSON(report)
}

func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	samplesDir := fs.String("samples", filepath.Join("artifacts", "demo", "samples"), "sample directory")
	dbPath := fs.String("db", filepath.Join("artifacts", "demo", "run.db"), "SQLite checkpoint path")
	mode := fs.String("mode", "realtime", "realtime|resume|full")
	injection := fs.String("inject", "", "injection JSON")
	logPath := fs.String("log", "", "event log path")
	workers := fs.Int("workers", 4, "maximum parallel route workers")
	buffer := fs.Int("buffer", 2, "per-worker frame buffer limit")
	cancelAfter := fs.Int("cancel-after", 0, "cancel after N committed segments")
	autoRetry := fs.Bool("auto-retry", false, "retry one-shot controlled segment failures")
	schedulePath := fs.String("param-schedule", "", "JSON parameter update schedule")
	params := paramsFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	p, err := params()
	if err != nil {
		return err
	}
	schedule, err := loadSchedule(*schedulePath)
	if err != nil {
		return err
	}
	var closer func()
	sink, closer, err := logSink(*logPath)
	if err != nil {
		return err
	}
	defer closer()
	cfg := engine.Config{
		DatasetDir: *samplesDir, DBPath: *dbPath, InjectionPath: *injection,
		Mode: engine.Mode(*mode), Params: p, MaxWorkers: *workers,
		FrameBufferPerWorker: *buffer, CancelAfterCommits: *cancelAfter,
		AutoRetryInjectedFailure: *autoRetry, ParamSchedule: schedule, EventSink: sink,
	}
	result, runErr := engine.Execute(ctx, cfg)
	if result != nil {
		if err := printJSON(result); err != nil {
			return err
		}
	}
	return runErr
}

func loadSchedule(path string) ([]engine.ParamUpdate, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var updates []engine.ParamUpdate
	if err := json.Unmarshal(b, &updates); err == nil {
		return updates, nil
	}
	var wrapper struct {
		Updates []engine.ParamUpdate `json:"updates"`
	}
	if err := json.Unmarshal(b, &wrapper); err != nil {
		return nil, fmt.Errorf("parameter schedule must be an array or {\"updates\": [...]}: %w", err)
	}
	return wrapper.Updates, nil
}

func paramsFlags(fs *flag.FlagSet) func() (model.Params, error) {
	targetPeak := fs.Float64("target-peak", 100, "SDR target peak nits")
	exposure := fs.Float64("exposure", 0, "exposure EV")
	toneMap := fs.String("tone-map", "aces", "aces|reinhard")
	gamut := fs.String("gamut", "desaturate", "desaturate|clip")
	outputBits := fs.Int("output-bits", 8, "SDR output bits (8)")
	paramsFile := fs.String("params-file", "", "JSON parameter file")
	return func() (model.Params, error) {
		p := model.Params{
			TargetPeakNits: *targetPeak, ExposureEV: *exposure, ToneMap: *toneMap,
			GamutMapping: *gamut, OutputBits: *outputBits,
		}
		if *paramsFile != "" {
			b, err := os.ReadFile(*paramsFile)
			if err != nil {
				return p, err
			}
			if err := json.Unmarshal(b, &p); err != nil {
				return p, err
			}
		}
		return p, nil
	}
}

func cmdAudit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	samplesDir := fs.String("samples", filepath.Join("artifacts", "demo", "samples"), "sample directory")
	dbPath := fs.String("db", filepath.Join("artifacts", "demo", "full.db"), "SQLite snapshot")
	if err := fs.Parse(args); err != nil {
		return err
	}
	result, err := audit.Audit(ctx, *samplesDir, *dbPath)
	if result != nil {
		_ = printJSON(result)
	}
	return err
}

func cmdCompare(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	left := fs.String("left", "", "left SQLite snapshot")
	right := fs.String("right", "", "right SQLite snapshot")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *left == "" || *right == "" {
		return fmt.Errorf("--left and --right are required")
	}
	result, err := compare.Snapshots(ctx, *left, *right)
	if result != nil {
		_ = printJSON(result)
	}
	return err
}

func cmdExport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	dbPath := fs.String("db", "", "SQLite snapshot")
	outDir := fs.String("out", "", "export directory")
	samplesDir := fs.String("samples", "", "optional sample directory for HDR previews")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dbPath == "" || *outDir == "" {
		return fmt.Errorf("--db and --out are required")
	}
	summary, err := exporter.Run(ctx, *dbPath, *outDir, *samplesDir)
	if err != nil {
		return err
	}
	return printJSON(summary)
}

func cmdDemo(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	root := fs.String("root", filepath.Join("artifacts", "demo"), "acceptance output root")
	workers := fs.Int("workers", 4, "maximum parallel workers")
	buffer := fs.Int("buffer", 2, "per-worker frame buffer")
	if err := fs.Parse(args); err != nil {
		return err
	}
	report, err := verify.RunAcceptance(ctx, verify.Options{Root: *root, Workers: *workers, Buffer: *buffer})
	if report != nil {
		_ = printJSON(report)
	}
	return err
}

func logSink(path string) (func(engine.Event), func(), error) {
	if path == "" {
		return nil, func() {}, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, err
	}
	enc := json.NewEncoder(f)
	return func(ev engine.Event) {
		_ = enc.Encode(ev)
	}, func() { _ = f.Close() }, nil
}

func printJSON(v interface{}) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func printError(err error) {
	if err == nil {
		return
	}
	var ae *apperr.Error
	if errors.As(err, &ae) {
		fmt.Fprintln(os.Stderr, ae.Error())
		return
	}
	fmt.Fprintln(os.Stderr, err)
}
