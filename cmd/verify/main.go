package main

import (
	"flag"
	"fmt"
	"os"

	"multiplexgw/internal/app"
	"multiplexgw/internal/bizerr"
)

func main() {
	data := flag.String("data", app.DefaultDataDir, "deterministic sample directory")
	artifacts := flag.String("artifacts", app.DefaultArtifactDir, "verification output directory")
	jsonOut := flag.Bool("json", false, "print full verification report as JSON")
	flag.Parse()

	report, err := app.Verify(*data, *artifacts)
	if *jsonOut {
		fmt.Println(app.MarshalReport(report))
	} else {
		printSummary(report)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "verification failed: %s\n", err.Error())
		os.Exit(1)
	}
}

func printSummary(r *app.VerificationReport) {
	fmt.Printf("OK=%v cross-version=%v truth=%v numerical=%v incremental=%v parallel=%v checkpoint=%v\n",
		r.OK, r.CrossVersion.Equal, r.Truth.Match, r.Numerical.Match,
		r.Incremental != nil && r.Incremental.AllMatch, r.Parallel.AllOK, r.Checkpoint.Match)
	fmt.Printf("events=%d sends=%d exceptions=%d\n", r.Truth.TraceCount, r.Truth.SendCount, len(r.Exceptions))
	for _, ex := range r.Exceptions {
		mark := "PASS"
		if !ex.Match {
			mark = "FAIL"
		}
		fmt.Printf("  [%s] %-32s %s\n", mark, ex.Case, ex.ActualCode)
	}
	if r.Incremental != nil {
		for _, c := range r.Incremental.Cases {
			mark := "PASS"
			if !c.SemanticEqual || c.Failure != "" {
				mark = "FAIL"
			}
			fmt.Printf("  [%s] incremental %-24s reused=%d recomputed=%d\n", mark, c.Name, c.ReusedEvents, c.RecomputedEvents)
		}
	}
	for name, path := range r.OutputFiles {
		fmt.Printf("artifact %s=%s\n", name, path)
	}
	if !r.OK {
		fmt.Fprintf(os.Stderr, "stable failure code: %s\n", bizerr.Internal)
	}
}
