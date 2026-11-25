package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"channelpipe/internal/acceptance"
	"channelpipe/internal/sample"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "generate" {
		generateCommand(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "demo" {
		demoCommand(os.Args[2:])
		return
	}
	demoCommand(os.Args[1:])
}

func demoCommand(arguments []string) {
	flags := flag.NewFlagSet("demo", flag.ExitOnError)
	out := flags.String("out", "", "output directory; defaults to a temporary directory")
	tiny := flags.Bool("tiny", false, "use a small fast sample set")
	workers := flags.Int("workers", 2, "maximum chunk worker parallelism")
	printJSON := flags.Bool("json", false, "print full JSON report to stdout")
	_ = flags.Parse(arguments)

	outputDir := *out
	if outputDir == "" {
		dir, err := os.MkdirTemp("", "channelpipe-demo-")
		if err != nil {
			fatal(err)
		}
		outputDir = dir
	}
	report, err := acceptance.Run(acceptance.Config{OutputDir: outputDir, Tiny: *tiny, Workers: *workers})
	if err != nil {
		writeJSONQuiet(filepath.Join(outputDir, "demo-error.json"), map[string]string{"error": err.Error()})
		fatal(err)
	}
	if *printJSON {
		data, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(data))
	} else {
		verifyOK := report.IndependentVerify != nil && report.IndependentVerify.OK
		threeOK := report.ThreePaths != nil && report.ThreePaths.OK
		failureOK := report.MainFailure != nil && report.MainFailure.Recovered
		fmt.Printf("ok=%t output=%s independent_verify=%t three_paths=%t failure_recovered=%t\n", report.OK, outputDir, verifyOK, threeOK, failureOK)
	}
	fmt.Printf("\nvalidation report: %s\n", filepath.Join(outputDir, "validation-report.md"))
	if !report.OK {
		os.Exit(2)
	}
}

func generateCommand(arguments []string) {
	flags := flag.NewFlagSet("generate", flag.ExitOnError)
	out := flags.String("out", filepath.Join("artifacts", "sample"), "sample output directory")
	tiny := flags.Bool("tiny", false, "generate tiny samples for tests")
	_ = flags.Parse(arguments)

	set, err := sample.Generate(sample.Options{Tiny: *tiny})
	if err != nil {
		fatal(err)
	}
	written, err := sample.WriteDirectory(*out, set, 128, 128)
	if err != nil {
		fatal(err)
	}
	data, _ := json.MarshalIndent(written.Manifest, "", "  ")
	fmt.Println(string(data))
}

func writeJSONQuiet(path string, value any) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}

func fatal(err error) {
	_, _ = fmt.Fprintf(os.Stderr, "%s %v\n", time.Now().Format(time.RFC3339), err)
	os.Exit(1)
}
