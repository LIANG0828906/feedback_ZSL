package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"gaitretarget/internal/gait"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	var err error
	switch args[0] {
	case "generate":
		err = runGenerate(args[1:])
	case "validate":
		err = runValidate(args[1:])
	case "run":
		err = runRetarget(args[1:])
	case "verify":
		err = runVerify(args[1:])
	case "-h", "--help", "help":
		usage()
		return 0
	default:
		usage()
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		var app *gait.AppError
		if errors.As(err, &app) {
			data, _ := json.MarshalIndent(app, "", "  ")
			fmt.Fprintln(os.Stderr, string(data))
		}
		return 1
	}
	return 0
}

func runGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	out := fs.String("out", "data", "output dataset directory")
	seed := fs.Uint64("seed", 42, "fixed deterministic seed")
	sources := fs.Int("sources", 2, "number of source gait loops")
	targets := fs.Int("targets", 3, "number of target skeletons per source")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg := gait.DefaultGeneratorConfig(*seed)
	cfg.SourceCount = *sources
	cfg.TargetCount = *targets
	ds, err := gait.GenerateDataset(cfg)
	if err != nil {
		return err
	}
	if err := gait.WriteDataset(*out, ds); err != nil {
		return err
	}
	fmt.Printf("generated seed=%d sources=%d targets=%d dir=%s\n", *seed, len(ds.Sources), len(ds.Targets), *out)
	for _, source := range ds.Sources {
		fmt.Printf("  source=%s frames=%d cycles=%d\n", source.ID, source.FrameCount, len(source.Cycles))
	}
	return nil
}

func runValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	dataDir := fs.String("data", "data", "dataset directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ds, err := gait.LoadDataset(*dataDir)
	if err != nil {
		return err
	}
	fmt.Printf("validation ok: sources=%d phases=%d targets=%d truths=%d params=%s algorithm=%s\n",
		len(ds.Sources), len(ds.Phases), len(ds.Targets), len(ds.Truths),
		ds.Params.ParameterVersion, ds.Params.AlgorithmVersion)
	return nil
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	dataDir := fs.String("data", "data", "dataset directory")
	sourceID := fs.String("source", "src-walk-001", "source loop id")
	targetID := fs.String("target", "target-compact-01", "target skeleton id")
	store := fs.String("store", filepath.Join("artifacts", "verify-store"), "isolated checkpoint root")
	interruptAfter := fs.Int("interrupt-after", 1, "cycles committed before simulated restart")
	out := fs.String("out", "", "optional verification report JSON path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ds, err := gait.LoadDataset(*dataDir)
	if err != nil {
		return err
	}
	source, phases, truth, err := gait.SelectSource(ds, *sourceID)
	if err != nil {
		return err
	}
	target, err := gait.SelectTarget(ds, *targetID)
	if err != nil {
		return err
	}
	report, err := gait.VerifyThreePaths(context.Background(), source, phases, truth, target, ds.Params, *store, *interruptAfter)
	if err != nil {
		if *out != "" {
			_ = writeJSON(*out, report)
		}
		return err
	}
	if *out != "" {
		if err := writeJSON(*out, report); err != nil {
			return err
		}
	}
	fmt.Printf("three-path verification OK: source=%s target=%s frames=%d cycles=%d realtime_loaded=%d resume_loaded=%d\n",
		report.SourceID, report.TargetID, report.FrameCount, report.CommittedCycles,
		report.RealtimeLoaded, report.ResumeLoaded)
	return nil
}

func runRetarget(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	dataDir := fs.String("data", "data", "dataset directory")
	sourceID := fs.String("source", "src-walk-001", "source loop id")
	targetID := fs.String("target", "target-compact-01", "target skeleton id")
	mode := fs.String("mode", "realtime", "full, realtime or resume")
	store := fs.String("store", filepath.Join("artifacts", "run-store"), "checkpoint directory")
	reset := fs.Bool("reset", false, "delete checkpoint directory before running")
	interruptAfter := fs.Int("interrupt-after", 0, "stop after this many cycle attempts; zero processes all")
	out := fs.String("out", "", "optional result JSON path")
	mappingOut := fs.String("mapping-out", "", "optional phase mapping trace JSON path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ds, err := gait.LoadDataset(*dataDir)
	if err != nil {
		return err
	}
	source, phases, truth, err := gait.SelectSource(ds, *sourceID)
	if err != nil {
		return err
	}
	target, err := gait.SelectTarget(ds, *targetID)
	if err != nil {
		return err
	}
	if *reset && *store != "" {
		if err := os.RemoveAll(*store); err != nil {
			return err
		}
	}
	switch *mode {
	case "full":
		result, mapping, err := gait.FullRetarget(source, phases, truth, target, ds.Params)
		if err != nil {
			return err
		}
		if *out != "" {
			if err := writeJSON(*out, result); err != nil {
				return err
			}
		}
		if *mappingOut != "" {
			if err := writeJSON(*mappingOut, mapping); err != nil {
				return err
			}
		}
		fmt.Printf("full retarget complete: frames=%d events=%d corrections=%d\n",
			result.FrameCount, len(result.Events), len(result.Corrections))
		return nil
	case "realtime", "resume":
		options := gait.EngineOptions{}
		if *store == "" {
			return fmt.Errorf("--store is required for %s mode", *mode)
		}
		options.Store = gait.NewSnapshotStore(*store)
		engine, err := gait.NewEngine(source, phases, truth, target, ds.Params, options)
		if err != nil {
			return err
		}
		runErr := engine.RunCycles(context.Background(), *interruptAfter)
		status := engine.Status()
		fmt.Printf("%s progress: committed=%d total=%d paused=%v\n", *mode, status.Committed, status.Total, status.Paused)
		if report := engine.SnapshotReport(); report.Invalidated {
			fmt.Printf("snapshot invalidated: %s\n", report.Reason)
		}
		if runErr != nil {
			return runErr
		}
		if status.Committed != status.Total {
			fmt.Println("run stopped at a cycle boundary; rerun the same command to resume")
			return nil
		}
		result, err := engine.Result()
		if err != nil {
			return err
		}
		if *out != "" {
			if err := writeJSON(*out, result); err != nil {
				return err
			}
		}
		if *mappingOut != "" {
			if err := writeJSON(*mappingOut, engine.Mapping()); err != nil {
				return err
			}
		}
		fmt.Printf("%s retarget complete: frames=%d events=%d corrections=%d loaded=%d\n",
			*mode, result.FrameCount, len(result.Events), len(result.Corrections),
			len(engine.SnapshotReport().LoadedCycles))
		return nil
	default:
		return fmt.Errorf("unknown mode %q (want full, realtime or resume)", *mode)
	}
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func usage() {
	fmt.Fprintln(os.Stderr, `gaitretarget - deterministic phase-locked biped retargeting

Usage:
  gaitretarget generate [--out data] [--seed 42]
  gaitretarget validate [--data data]
  gaitretarget run --mode full|realtime|resume [flags]
  gaitretarget verify [flags]

Run "gaitretarget <command> --help" for command flags.`)
}
