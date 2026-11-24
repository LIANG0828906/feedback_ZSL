package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"volumelab/pkg/verify"
)

func main() {
	out := flag.String("out", "artifacts/verification.json", "verification report path")
	seed := flag.Int64("seed", 42, "deterministic sample seed")
	flag.Parse()
	report, err := verify.Run(context.Background(), *seed)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verification failed: %v\n", err)
		os.Exit(1)
	}
	if err := verify.Write(report, *out); err != nil {
		fmt.Fprintf(os.Stderr, "cannot write verification report: %v\n", err)
		os.Exit(1)
	}
	for _, check := range report.Checks {
		status := "PASS"
		if !check.Pass {
			status = "FAIL"
		}
		fmt.Printf("%s  %-34s %s\n", status, check.Name, check.Detail)
	}
	fmt.Printf("\nreport: %s\nallPass: %v\n", *out, report.AllPass)
	if !report.AllPass {
		os.Exit(1)
	}
}
