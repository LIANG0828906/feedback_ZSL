package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"prosodyalign/internal/generator"
	"prosodyalign/internal/pipeline"
	"prosodyalign/internal/schema"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "verify":
		verifyCmd(os.Args[2:])
	case "schema-check":
		schemaCmd(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: prosodyctl <verify|schema-check> [flags]")
}

func verifyCmd(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	workdir := fs.String("workdir", "artifacts/checkpoints", "SQLite checkpoint directory")
	out := fs.String("out", "artifacts/verification.json", "JSON report output")
	_ = fs.Parse(args)

	report, err := pipeline.RunThreeWay(*workdir, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify failed: %v\n", err)
		os.Exit(1)
	}
	if *out != "" {
		if err := os.MkdirAll(dir(*out), 0o755); err != nil {
			panic(err)
		}
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			panic(err)
		}
		if err := os.WriteFile(*out, data, 0o644); err != nil {
			panic(err)
		}
	}
	fmt.Printf("three_way_equal=%v commit_sequences_equal=%v epochs=%d\n", report.Comparison.AllEqual, report.Comparison.CommitSequencesEqual, report.Comparison.ComparedEpochs)
	if mm := report.Comparison.FirstMismatch; mm != nil {
		fmt.Printf("first_mismatch epoch=%d fragment=%s frame=%d field=%s expected=%v actual=%v\n", mm.Epoch, mm.FragmentID, mm.FrameIndex, mm.Field, mm.Expected, mm.Actual)
	}
	for _, t := range report.Truth {
		fmt.Printf("truth %s envelope_exact=%v accent_error=%d within_tol=%v fit_exact=%v", t.DialogID, t.EnvelopeExact, t.AccentErrorSamples, t.AccentWithinTol, t.FitExact)
		if t.FirstFitField != "" {
			fmt.Printf(" first_fit=%s", t.FirstFitField)
		}
		fmt.Println()
	}
	if !report.Comparison.AllEqual {
		os.Exit(1)
	}
}

func schemaCmd(args []string) {
	fs := flag.NewFlagSet("schema-check", flag.ExitOnError)
	out := fs.String("out", "artifacts/schema-check.json", "JSON report output")
	_ = fs.Parse(args)
	suite, err := generator.GenerateSuite(generator.FixedSeed, 1)
	if err != nil {
		panic(err)
	}
	d := suite.Dialogs[0]
	report := map[string]any{}
	for version, data := range d.Docs {
		p, loadReport, err := schema.Load(data)
		if err != nil {
			panic(err)
		}
		report[string(version)] = map[string]any{
			"loaded_id": p.ID, "phonemes": len(p.Phonemes), "content_hash": p.ContentHash,
			"load_report": loadReport,
		}
	}
	data, _ := json.MarshalIndent(report, "", "  ")
	if *out != "" {
		_ = os.MkdirAll(dir(*out), 0o755)
		if err := os.WriteFile(*out, data, 0o644); err != nil {
			panic(err)
		}
	}
	fmt.Println(string(data))
}

func dir(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "."
}
