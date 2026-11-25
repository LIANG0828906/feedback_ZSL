package workbench

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"sre-timeline/internal/sample"
)

func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return runReplCommand(ctx, []string{}, stdin, stdout, stderr)
	}
	switch args[0] {
	case "help", "-h", "--help":
		printHelp(stdout)
		return 0
	case "generate":
		return runGenerate(ctx, args[1:], stdout, stderr)
	case "verify":
		return runVerify(ctx, args[1:], stdout, stderr)
	case "repl", "workbench":
		return runReplCommand(ctx, args[1:], stdin, stdout, stderr)
	case "demo":
		return runDemo(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command: %s\n\n", args[0])
		printHelp(stderr)
		return 2
	}
}

func printHelp(w io.Writer) {
	fmt.Fprintln(w, `SRE timeline workbench

Usage:
  sre-workbench generate [--seed 42] [--dir out/samples]
  sre-workbench verify   [--seed 42] [--dir artifacts/verify] [--workers 4] [--buffer 4096]
  sre-workbench repl     [--seed 42] [--workers 4] [--buffer 4096] [--db artifacts/checkpoints/repl.db]
  sre-workbench demo

REPL commands:
  load v1|v2|v3 | timeline | intervals [service] | review [topN] | candidates [interval-id]
  propagate <service> [generation]
  edit threshold <service> <bp> | edit event-time <event-id> <relative-seconds>
  edit add-config <service> <event-id> <relative-seconds> [weight-bp]
  edge add <caller> <callee> | edge delete <caller> <callee>
  diff | diagnostics | checkpoint save [round] | checkpoint load | checkpoint recover | checkpoint mismatch | checkpoint truncate
  inject time-reversal|negative-threshold|cycle|scale|unknown-field|backpressure-buffer|backpressure-workers
  help | exit`)
}

func runGenerate(_ context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "out/samples", "output directory")
	seed := fs.Int64("seed", sample.DefaultSeed, "deterministic generator seed")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	_, files, err := sample.WriteWorkspace(*dir, *seed)
	if err != nil {
		fmt.Fprintf(stderr, "generate failed: %v\n", err)
		return 1
	}
	data, _ := json.MarshalIndent(files, "", "  ")
	fmt.Fprintln(stdout, string(data))
	return 0
}

func runVerify(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	seed := fs.Int64("seed", sample.DefaultSeed, "deterministic generator seed")
	dir := fs.String("dir", "artifacts/verify", "verification output directory")
	workers := fs.Int("workers", 4, "analysis worker budget")
	buffer := fs.Int("buffer", 4096, "per-service event buffer limit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	report, err := RunVerification(ctx, VerifyOptions{Seed: *seed, Dir: *dir, Workers: *workers, Buffer: *buffer})
	if err != nil {
		fmt.Fprintf(stderr, "verify aborted: %v\n", err)
		return 1
	}
	for _, check := range report.Checks {
		mark := "PASS"
		if !check.Pass {
			mark = "FAIL"
		}
		fmt.Fprintf(stdout, "[%s] %s", mark, check.Name)
		if check.Code != "" {
			fmt.Fprintf(stdout, " (%s)", check.Code)
		}
		fmt.Fprintln(stdout)
		if check.Error != "" {
			fmt.Fprintf(stdout, "       %s\n", strings.ReplaceAll(check.Error, "\n", "\n       "))
		}
	}
	fmt.Fprintf(stdout, "\nseed=%d go=%s intervals=%d top=%s\nreport=%s\n", report.Seed, report.GoVersion, report.IntervalCount, report.TopCandidate, report.ReportPath)
	if report.Passed {
		fmt.Fprintln(stdout, "VERIFY PASS")
		return 0
	}
	fmt.Fprintln(stdout, "VERIFY FAIL")
	return 1
}
