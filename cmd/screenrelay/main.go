package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"screenrelay/internal/sample"
	"screenrelay/internal/verify"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "sample" {
		writeSampleCommand(os.Args[2:])
		return
	}
	verifyCommand(os.Args[1:])
}

func verifyCommand(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	sampleDir := fs.String("samples", "artifacts/samples", "directory for generated versioned sample documents")
	reportPath := fs.String("report", "artifacts/reports/verification-report.json", "JSON verification report path")
	checkpointDir := fs.String("checkpoints", "artifacts/reports", "directory for local file checkpoints")
	_ = fs.Parse(args)

	report, err := verify.RunAll(context.Background(), *sampleDir, *checkpointDir)
	if err != nil && report == nil {
		fmt.Fprintf(os.Stderr, "verification fatal: %v\n", err)
		os.Exit(2)
	}
	if *reportPath != "" {
		if err := os.MkdirAll(filepath.Dir(*reportPath), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "create report directory: %v\n", err)
			os.Exit(2)
		}
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "marshal report: %v\n", err)
			os.Exit(2)
		}
		if err := os.WriteFile(*reportPath, data, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "write report: %v\n", err)
			os.Exit(2)
		}
	}
	printSummary(report, *reportPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verification fatal: %v\n", err)
		os.Exit(2)
	}
	if !report.OK {
		os.Exit(1)
	}
}

func writeSampleCommand(args []string) {
	fs := flag.NewFlagSet("sample", flag.ExitOnError)
	dir := fs.String("dir", "artifacts/samples", "sample output directory")
	_ = fs.Parse(args)
	set, err := sample.Generate(sample.FixedSeed)
	if err != nil {
		fmt.Fprintf(os.Stderr, "generate sample: %v\n", err)
		os.Exit(2)
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "create sample dir: %v\n", err)
		os.Exit(2)
	}
	for key, data := range set.Raw {
		path := filepath.Join(*dir, "session."+key+".json")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", path, err)
			os.Exit(2)
		}
		fmt.Println(path)
	}
}

func printSummary(report *verify.Report, reportPath string) {
	fmt.Printf("screenrelay verification seed=%d ok=%v\n", report.FixedSeed, report.OK)
	for _, check := range report.Checks {
		mark := "PASS"
		if !check.OK {
			mark = "FAIL"
		}
		code := ""
		if check.Code != "" {
			code = " [" + check.Code + "]"
		}
		fmt.Printf("  %-5s %-32s%s %s\n", mark, check.Name, code, check.Detail)
	}
	for _, path := range report.Paths {
		fmt.Printf("  PATH  %-20s windows=%d commits=%v mismatch=%q\n", path.Name, path.Windows, path.CommitSequence, path.FirstMismatch)
	}
	if reportPath != "" {
		fmt.Println("report:", reportPath)
	}
}
