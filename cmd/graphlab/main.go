package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"graphlab/internal/app"
	"graphlab/internal/generator"
	"graphlab/internal/model"
)

const usage = `graphlab: deterministic graph storage demonstration

Commands:
  generate  Generate all three schema documents and truth data.
  validate  Verify schema equivalence, shard indexes, traversals, layouts and concurrency.
  demo      Run operations, incremental/full comparison and SQLite recovery.
  faults    Inject every requested fault class and print stable business codes.

Examples:
  go run ./cmd/graphlab generate -seed 42 -out samples/seed42
  go run ./cmd/graphlab validate -sample samples/seed42 -workers 4 -buffer 4096
  go run ./cmd/graphlab demo -sample samples/seed42 -checkpoint artifacts/demo/checkpoint.db -out artifacts/demo
  go run ./cmd/graphlab faults -sample samples/seed42 -out artifacts
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "generate":
		err = commandGenerate(os.Args[2:])
	case "validate":
		err = commandValidate(ctx, os.Args[2:])
	case "demo":
		err = commandDemo(ctx, os.Args[2:])
	case "faults":
		err = commandFaults(os.Args[2:])
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		printError(err)
		os.Exit(1)
	}
	if ctx.Err() != nil {
		fmt.Println("graceful shutdown: last complete checkpoint retained")
	}
}

func printError(err error) {
	var be *model.BizError
	walkBiz(err, &be)
	if be != nil {
		b, _ := json.MarshalIndent(be, "", "  ")
		fmt.Fprintln(os.Stderr, string(b))
		return
	}
	fmt.Fprintln(os.Stderr, err)
}

func walkBiz(err error, target **model.BizError) {
	for err != nil {
		if be, ok := err.(*model.BizError); ok {
			*target = be
			return
		}
		type unwrapper interface{ Unwrap() error }
		if u, ok := err.(unwrapper); ok {
			err = u.Unwrap()
			continue
		}
		break
	}
}

func commandGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	seed := fs.Int64("seed", generator.DefaultSeed, "deterministic generator seed")
	out := fs.String("out", "samples/seed42", "output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	sample, err := generator.Generate(*seed)
	if err != nil {
		return err
	}
	if err := sample.Write(*out); err != nil {
		return err
	}
	fmt.Printf("generated seed=%d project=%s core=%s\n", sample.Seed, sample.Core.ID, sample.Truth.CoreDigest)
	fmt.Printf("files: %s\n", filepath.Join(*out, "project_v1.json, project_v2.json, project_v3.json, truth.json"))
	return nil
}

func commandValidate(ctx context.Context, args []string) error {
	opts, err := parseCommon(args, "validate")
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return nil
	default:
	}
	opts.Logf = func(format string, values ...any) {
		fmt.Printf(format+"\n", values...)
	}
	report, err := app.Validate(opts)
	if report != nil {
		if writeErr := app.WriteJSON(filepath.Join(opts.OutputDir, "validation-report.json"), report); writeErr != nil && err == nil {
			err = writeErr
		}
		for _, check := range report.Checks {
			status := "PASS"
			if !check.Passed {
				status = "FAIL"
			}
			fmt.Printf("%s %-42s", status, check.Name)
			if len(check.Diffs) > 0 {
				fmt.Printf(" diffs=%v", check.Diffs)
			}
			fmt.Println()
		}
		if report.AllPassed {
			fmt.Printf("validation PASS core=%s workers=%d buffer=%d\n",
				report.CoreDigest, report.Workers, report.BufferPerWorker)
		}
	}
	return err
}

func commandDemo(ctx context.Context, args []string) error {
	opts := app.DefaultOptions()
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	bindCommon(fs, &opts)
	checkpointPath := fs.String("checkpoint", "artifacts/demo/checkpoint.db", "SQLite checkpoint file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	opts.CheckpointPath = *checkpointPath
	opts.Logf = func(format string, values ...any) {
		fmt.Printf(format+"\n", values...)
	}
	report, err := app.RunDemo(ctx, opts)
	if report != nil {
		fmt.Printf("demo schemas=%d operations=%d incremental-full=%v stopped-on-signal=%v\n",
			len(report.Schemas), len(report.Operations), report.IncrementalFullEqual, report.StoppedOnSignal)
		fmt.Printf("recovery-diffs=%d forced-mismatch=%s fallback-diffs=%d\n",
			len(report.RecoveryQueryDiffs), report.ForcedMismatchCode, len(report.RebuildFallbackDiffs))
		if report.ReportPath != "" {
			fmt.Printf("report: %s\n", report.ReportPath)
		}
	}
	return err
}

func commandFaults(args []string) error {
	opts, err := parseCommon(args, "faults")
	if err != nil {
		return err
	}
	opts.Logf = func(format string, values ...any) {
		fmt.Printf(format+"\n", values...)
	}
	report, err := app.RunFaults(opts)
	if report != nil {
		fmt.Printf("faults all-expected=%v source-unchanged=%v runtime-usable=%v checkpoint-usable=%v\n",
			report.AllExpected, report.SourceUnchanged,
			report.CommittedRuntimeStillUsable, report.OriginalCheckpointUsable)
		if report.ReportPath != "" {
			fmt.Printf("report: %s\n", report.ReportPath)
		}
	}
	return err
}

func parseCommon(args []string, name string) (app.Options, error) {
	opts := app.DefaultOptions()
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	bindCommon(fs, &opts)
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	return opts, nil
}

func bindCommon(fs *flag.FlagSet, opts *app.Options) {
	fs.StringVar(&opts.SampleDir, "sample", opts.SampleDir, "sample directory")
	fs.StringVar(&opts.OutputDir, "out", opts.OutputDir, "output directory")
	fs.IntVar(&opts.Workers, "workers", opts.Workers, "worker budget (1..32)")
	fs.IntVar(&opts.BufferPerWorker, "buffer", opts.BufferPerWorker, "per-worker build/traversal buffer")
	fs.Int64Var(&opts.Seed, "seed", opts.Seed, "seed used if the sample directory is absent")
}
