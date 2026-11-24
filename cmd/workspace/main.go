package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"performanceexpressions/workspace/acceptance"
	"performanceexpressions/workspace/checkpoint"
	"performanceexpressions/workspace/domain"
	"performanceexpressions/workspace/engine"
	"performanceexpressions/workspace/generator"
	"performanceexpressions/workspace/workbench"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = runGenerate(os.Args[2:])
	case "validate":
		err = runValidate(os.Args[2:])
	case "evaluate":
		err = runEvaluate(os.Args[2:])
	case "recover":
		err = runRecover(os.Args[2:])
	case "accept":
		err = runAccept(os.Args[2:])
	case "version":
		fmt.Printf("expression-workspace algorithm=%s schema=%s interpolation=linear,hold,smoothstep\n", domain.AlgorithmVersion, domain.SchemaVersion)
	case "ui":
		fmt.Println("Desktop UI: build with `go build -tags gui -o workspace-desktop ./cmd/desktop` and run ./workspace-desktop")
		fmt.Println("On a machine with Fyne native dependencies installed, the same command builds on Linux, macOS and Windows.")
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		printError(err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `expression workspace

Usage:
  workspace generate [-out samples] [-seed 20261124]
  workspace validate -project samples/aerial-race.json
  workspace evaluate -project samples/aerial-race.json [-out artifacts/evaluation.json]
  workspace recover -project samples/aerial-race.json -db artifacts/checkpoints.db
  workspace accept [-samples samples] [-db artifacts/expression-checkpoints.db] [-report artifacts/acceptance.json] [-markdown artifacts/ACCEPTANCE.md]
  workspace version
  workspace ui`)
}

func runGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	out := fs.String("out", "samples", "sample output directory")
	seed := fs.Int64("seed", generator.DefaultSeed, "deterministic generator seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	index, err := generator.WriteSamples(*out, *seed)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(index, "", "  ")
	fmt.Println(string(b))
	return nil
}

func runValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	projectPath := fs.String("project", "", "project JSON path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *projectPath == "" {
		return domain.NewError(domain.CodeSchema, "-project is required")
	}
	project, err := generator.LoadProject(*projectPath)
	if err != nil {
		return err
	}
	if err := engine.ValidateProject(project, engine.ValidateOptions{RequireTruth: true}); err != nil {
		return err
	}
	fmt.Printf("VALID project=%s segments=%d samples_per_beat=%d\n", project.ID, len(project.Segments), project.SamplesPerBeat)
	return nil
}

func runEvaluate(args []string) error {
	fs := flag.NewFlagSet("evaluate", flag.ContinueOnError)
	projectPath := fs.String("project", "", "project JSON path")
	out := fs.String("out", "", "optional result JSON output")
	interp := fs.String("interpolation", domain.InterpolationLinear, "linear, hold or smoothstep")
	tolerance := fs.Float64("tolerance", 1e-9, "truth comparison tolerance")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *projectPath == "" {
		return domain.NewError(domain.CodeSchema, "-project is required")
	}
	project, err := generator.LoadProject(*projectPath)
	if err != nil {
		return err
	}
	if err := engine.ValidateProject(project, engine.ValidateOptions{RequireTruth: true}); err != nil {
		return err
	}
	params := domain.DefaultParameters()
	params.Interpolation = *interp
	params.Tolerance = *tolerance
	result, err := engine.EvaluateProject(project, params)
	if err != nil {
		return err
	}
	precision, precisionErr := engine.ValidatePrecision(project, result, *tolerance)
	summary := struct {
		ProjectID      string                 `json:"project_id"`
		Segments       int                    `json:"segments"`
		Samples        int                    `json:"samples"`
		Decisions      int                    `json:"decisions"`
		Precision      engine.PrecisionReport `json:"precision"`
		PrecisionError string                 `json:"precision_error,omitempty"`
		Result         domain.ResultSet       `json:"result"`
	}{
		ProjectID: project.ID,
		Segments:  len(result.Segments),
		Precision: precision,
		Result:    result,
	}
	for _, seg := range result.Segments {
		summary.Samples += len(seg.Samples)
		for _, sample := range seg.Samples {
			summary.Decisions += len(sample.Decisions)
		}
	}
	if precisionErr != nil {
		summary.PrecisionError = precisionErr.Error()
	}
	b, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if *out != "" {
		if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s samples=%d decisions=%d precision_ok=%t\n", *out, summary.Samples, summary.Decisions, precision.OK)
	} else {
		fmt.Print(string(b))
	}
	return precisionErr
}

func runRecover(args []string) error {
	fs := flag.NewFlagSet("recover", flag.ContinueOnError)
	projectPath := fs.String("project", "", "base project JSON path")
	dbPath := fs.String("db", "", "SQLite checkpoint path")
	out := fs.String("out", "", "optional recovered project JSON path")
	interp := fs.String("interpolation", domain.InterpolationLinear, "interpolation mode")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *projectPath == "" || *dbPath == "" {
		return domain.NewError(domain.CodeSchema, "-project and -db are required")
	}
	project, err := generator.LoadProject(*projectPath)
	if err != nil {
		return err
	}
	params := domain.DefaultParameters()
	params.Interpolation = *interp
	store, err := checkpoint.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	sess, report, err := workbench.Recover(context.Background(), store, project, params)
	if err != nil {
		return err
	}
	committed := sess.CommittedProject()
	result := sess.CommittedResult()
	b, err := json.MarshalIndent(struct {
		Recovery workbench.RecoveryReport `json:"recovery"`
		Project  domain.Project           `json:"project"`
		Result   domain.ResultSet         `json:"result"`
	}{Recovery: report, Project: committed, Result: result}, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if *out != "" {
		if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			return err
		}
		fmt.Printf("recovered %s invalidated=%t loaded=%d replayed=%d\n", project.ID, report.Invalidated, report.LoadedBatches, report.ReplayedBatches)
		return nil
	}
	fmt.Print(string(b))
	return nil
}

func runAccept(args []string) error {
	fs := flag.NewFlagSet("accept", flag.ContinueOnError)
	samples := fs.String("samples", "samples", "sample output directory")
	db := fs.String("db", "artifacts/expression-checkpoints.db", "SQLite checkpoint path")
	reportPath := fs.String("report", "artifacts/acceptance.json", "JSON acceptance report path")
	markdown := fs.String("markdown", "artifacts/ACCEPTANCE.md", "Markdown acceptance report path")
	seed := fs.Int64("seed", generator.DefaultSeed, "deterministic generator seed")
	interp := fs.String("interpolation", domain.InterpolationLinear, "interpolation mode")
	tolerance := fs.Float64("tolerance", 1e-9, "precision tolerance")
	if err := fs.Parse(args); err != nil {
		return err
	}
	params := domain.DefaultParameters()
	params.Interpolation = *interp
	params.Tolerance = *tolerance
	report, err := acceptance.Run(context.Background(), acceptance.Options{
		SampleDir: *samples,
		DBPath:    *db,
		Seed:      *seed,
		Params:    params,
	})
	if writeErr := acceptance.WriteJSON(*reportPath, report); writeErr != nil {
		return writeErr
	}
	if writeErr := acceptance.WriteMarkdown(*markdown, report); writeErr != nil {
		return writeErr
	}
	if err != nil {
		return err
	}
	fmt.Printf("acceptance=%t report=%s markdown=%s\n", report.OK, *reportPath, *markdown)
	fmt.Printf("three_paths_equal=%t projects=%d faults=%d recovery=%t invalidation=%t concurrency=%t\n",
		report.ThreePaths.Equal, len(report.Projects), len(report.Faults), report.Recovery.OK, report.Invalidation.OK, report.Concurrency.OK)
	return nil
}

func printError(err error) {
	if appErr, ok := domain.AsAppError(err); ok {
		payload := struct {
			Error *domain.AppError `json:"error"`
		}{Error: appErr}
		b, _ := json.MarshalIndent(payload, "", "  ")
		fmt.Fprintln(os.Stderr, string(b))
		return
	}
	fmt.Fprintln(os.Stderr, err)
}
