package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"tacticalworkbench/internal/sample"
)

func main() {
	seed := flag.Int64("seed", sample.DefaultSeed, "deterministic sample seed")
	out := flag.String("out", filepath.Join("data", "scenario.json"), "output scenario path")
	flag.Parse()

	scenario, err := sample.Generate(*seed)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sample generation failed:", err)
		os.Exit(1)
	}
	payload, err := json.MarshalIndent(scenario, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "encode scenario failed:", err)
		os.Exit(1)
	}
	payload = append(payload, '\n')
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "create output directory failed:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, payload, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write scenario failed:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d bytes, seed=%d)\n", *out, len(payload), *seed)
}
