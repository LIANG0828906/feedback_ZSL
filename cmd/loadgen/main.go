package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"feedback_zsl/internal/loadgen"
)

func main() {
	configPath := flag.String("config", "configs/scenario.json", "scenario JSON file")
	outputPath := flag.String("output", "artifacts/scenario-report.json", "JSON report output file")
	flag.Parse()
	data, err := os.ReadFile(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var sc loadgen.Scenario
	if err := json.Unmarshal(data, &sc); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	report, err := loadgen.Run(ctx, sc)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out, _ := json.MarshalIndent(report, "", "  ")
	if *outputPath != "" {
		_ = os.MkdirAll("artifacts", 0o755)
		_ = os.WriteFile(*outputPath, append(out, '\n'), 0o644)
	}
	fmt.Println(string(out))
}
