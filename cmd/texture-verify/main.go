package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"texturekernel"
	"texturekernel/sample"
	"texturekernel/store"
	"texturekernel/verify"
)

func main() {
	dbPath := flag.String("db", "", "SQLite checkpoint path")
	runID := flag.String("run", "", "run id to verify")
	planPath := flag.String("plan", "", "plan JSON file")
	truthPath := flag.String("truth", "", "truth JSON file")
	flag.Parse()
	if *dbPath == "" || *runID == "" || *planPath == "" || *truthPath == "" {
		fmt.Fprintln(os.Stderr, "usage: texture-verify -db checkpoint.db -run RUN -plan plan.json -truth truth.json")
		os.Exit(2)
	}
	planData, err := os.ReadFile(*planPath)
	if err != nil {
		fatal(err)
	}
	plan, err := texture.LoadPlan(planData)
	if err != nil {
		fatal(err)
	}
	truth, err := sample.LoadTruth(*truthPath)
	if err != nil {
		fatal(err)
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		fatal(err)
	}
	defer st.Close()
	report, err := verify.VerifyCheckpoint(context.Background(), st, *runID, plan, truth)
	if err != nil {
		if ke, ok := texture.AsKernelError(err); ok && ke.Diff != nil {
			fmt.Fprintf(os.Stderr, "first violation: %s\n", ke.Diff.String())
		}
		fatal(err)
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fatal(err)
	}
	fmt.Printf("%s\n", data)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "texture-verify: %v\n", err)
	os.Exit(1)
}
