package main

import (
	"context"
	"flag"
	"path/filepath"
	"time"

	"qualitybench/internal/accept"
	"qualitybench/internal/core"
)

func cmdAccept(args []string) error {
	fs := flag.NewFlagSet("accept", flag.ContinueOnError)
	out := fs.String("out", "artifacts/acceptance", "acceptance output directory")
	seed := fs.Int64("seed", 424242, "deterministic seed")
	images := fs.Int("images", 3, "number of generated frames")
	workers := fs.Int("workers", 4, "worker pool size")
	resumeAfter := fs.Int("resume-after", 67, "block-step commits before resume probe")
	delayMS := fs.Int("delay-ms", 0, "per-task delay for diagnostics")
	if err := fs.Parse(args); err != nil {
		return core.Schema("args", "%v", err)
	}
	report, err := accept.Run(context.Background(), accept.Options{
		Out: *out, Seed: *seed, ImageCount: *images, Workers: *workers,
		ResumeAfter: *resumeAfter, TaskDelay: time.Duration(*delayMS) * time.Millisecond,
	})
	if report != nil {
		if printErr := printJSON(map[string]any{
			"passed":       report.Passed,
			"report":       filepath.Join(*out, "report.json"),
			"summary":      filepath.Join(*out, "summary.txt"),
			"elapsed_ns":   report.ElapsedNS,
			"checks":       len(report.Checks),
			"failed":       failedChecks(report),
			"comparisons":  report.Comparisons,
			"verification": report.Verification,
		}); printErr != nil {
			return printErr
		}
	}
	return err
}

func failedChecks(report *accept.Report) []string {
	var out []string
	for _, c := range report.Checks {
		if !c.Passed {
			out = append(out, c.Name)
		}
	}
	return out
}
