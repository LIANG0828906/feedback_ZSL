package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"

	"parkrelay/internal/scenario"
)

func main() {
	samples := flag.String("samples", "samples/gateways.json", "deterministic gateway sample file")
	out := flag.String("out", "artifacts/validation-report.json", "JSON validation report")
	work := flag.String("work-dir", "artifacts/validation-run", "temporary DB and runtime files")
	maxProcs := flag.Int("gomaxprocs", 2, "logical processor limit")
	memMiB := flag.Int64("memory-limit-mib", 1250, "Go runtime memory limit in MiB")
	flag.Parse()

	runtime.GOMAXPROCS(*maxProcs)
	limit := *memMiB * 1024 * 1024
	debug.SetMemoryLimit(limit)
	report, err := scenario.Run(context.Background(), *samples, *out, *work, *maxProcs, limit)
	if report != nil {
		fmt.Printf("validation all_passed=%v mappings=%d p95_ms=%.3f report=%s\n",
			report.Summary.AllPassed, report.Summary.MappingCount, report.Summary.P95ForwardingMS, *out)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if !report.Summary.AllPassed {
		for _, c := range report.Summary.Checks {
			if !c.Passed {
				fmt.Fprintf(os.Stderr, "FAILED: %s (%s)\n", c.Name, c.Details)
			}
		}
		os.Exit(1)
	}
}
