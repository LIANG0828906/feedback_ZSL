package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"example.com/silhouette"
)

func main() {
	seed := flag.Int64("seed", silhouette.DefaultSeed, "deterministic generator seed")
	out := flag.String("out", "", "optional JSON report path")
	flag.Parse()
	report, err := silhouette.Verify(context.Background(), *seed)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verification error:", err)
		os.Exit(1)
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "encode report:", err)
		os.Exit(1)
	}
	if *out != "" {
		if err := os.WriteFile(*out, append(data, '\n'), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "write report:", err)
			os.Exit(1)
		}
	}
	fmt.Println(silhouette.CompactVerification(report))
	if !report.AllPassed {
		os.Exit(2)
	}
}
