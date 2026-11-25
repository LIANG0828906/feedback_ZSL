package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"featurepipe/pipeline"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "demo":
		err = runDemo(os.Args[2:])
	case "generate":
		err = runGenerate(os.Args[2:])
	case "run":
		err = runPipeline(os.Args[2:])
	case "anomalies":
		err = runAnomalies(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		if be, ok := pipeline.AsBusinessError(err); ok {
			encoded, _ := json.MarshalIndent(be, "", "  ")
			fmt.Fprintf(os.Stderr, "%s\n", encoded)
			os.Exit(3)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: featurepipe <command> [flags]

commands:
  demo       load v1/v2/v3, show windows/discards, edit invalidation, resume and anomalies
  generate   write deterministic v1/v2/v3 documents
  run        execute one loaded project, optionally with SQLite checkpoint resume
  anomalies  run the stable business-code injection matrix

run "featurepipe <command> -h" for command flags.`)
}

func runDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", pipeline.DefaultSeed, "deterministic generator seed")
	out := fs.String("out", "", "write the full JSON report to this path")
	asJSON := fs.Bool("json", false, "print JSON instead of the text report")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	report, err := pipeline.RunDemo(ctx, *seed)
	if err != nil {
		return err
	}
	if *out != "" {
		if err := writeJSON(*out, report); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", *out)
	}
	if *asJSON || *out == "" {
		if *asJSON {
			return printJSON(report)
		}
		fmt.Print(pipeline.RenderDemoText(report))
	}
	return nil
}

func runGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	seed := fs.Int64("seed", pipeline.DefaultSeed, "deterministic generator seed")
	version := fs.String("version", "all", "v1, v2, v3, or all")
	out := fs.String("out", "", "output directory (or file for a single version)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	sample, err := pipeline.GenerateSample(*seed)
	if err != nil {
		return err
	}
	versions := []string{"v1", "v2", "v3"}
	if *version != "all" {
		versions = []string{*version}
	}
	for _, v := range versions {
		data, err := sample.VersionDocument(v)
		if err != nil {
			return err
		}
		if *out == "" {
			fmt.Println(string(data))
			continue
		}
		path := *out
		if *version == "all" {
			path = filepath.Join(path, v+".json")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
		}
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", path)
	}
	return nil
}

func runPipeline(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	version := fs.String("version", "v2", "built-in document version")
	input := fs.String("input", "", "load a document from this path instead of the built-in sample")
	workers := fs.Int("workers", 4, "feature worker budget")
	buffer := fs.Int("buffer", 64, "open-window buffer per feature")
	commitEvery := fs.Int("commit-every", 8, "events per SQLite commit point")
	dbPath := fs.String("db", "", "SQLite checkpoint path; empty disables persistence")
	noResume := fs.Bool("no-resume", false, "clear and rebuild even when a checkpoint exists")
	asJSON := fs.Bool("json", false, "print the complete result as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	project, err := loadProject(*version, *input)
	if err != nil {
		return err
	}
	opts := pipeline.DefaultRunOptions()
	opts.Workers = *workers
	opts.WindowBufferPerFeature = *buffer
	opts.CommitEvery = *commitEvery
	opts.Resume = !*noResume
	var store *pipeline.CheckpointStore
	if *dbPath != "" {
		store, err = pipeline.OpenCheckpointStore(*dbPath)
		if err != nil {
			return err
		}
		defer store.Close()
		opts.Checkpoint = store
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result, runErr := pipeline.Run(ctx, project, opts)
	if result == nil {
		return runErr
	}
	if *asJSON {
		if err := printJSON(result); err != nil {
			return err
		}
	} else {
		fmt.Print(renderRunResult(result))
	}
	if runErr != nil {
		if pipeline.ErrorCode(runErr) == pipeline.CodeInterrupted {
			fmt.Fprintln(os.Stderr, "run interrupted after a complete checkpoint")
			return nil
		}
		return runErr
	}
	return nil
}

func runAnomalies(args []string) error {
	fs := flag.NewFlagSet("anomalies", flag.ContinueOnError)
	seed := fs.Int64("seed", pipeline.DefaultSeed, "deterministic generator seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	report, err := pipeline.RunDemo(ctx, *seed)
	if err != nil {
		return err
	}
	return printJSON(report.Anomalies)
}

func loadProject(version, input string) (*pipeline.Project, error) {
	if input != "" {
		data, err := os.ReadFile(input)
		if err != nil {
			return nil, err
		}
		doc, err := pipeline.LoadJSON(data)
		if err != nil {
			return nil, err
		}
		return doc.Project, nil
	}
	sample, err := pipeline.GenerateDefault()
	if err != nil {
		return nil, err
	}
	data, err := sample.VersionDocument(version)
	if err != nil {
		return nil, err
	}
	doc, err := pipeline.LoadJSON(data)
	if err != nil {
		return nil, err
	}
	return doc.Project, nil
}

func renderRunResult(result *pipeline.RunResult) string {
	var b []byte
	b = append(b, []byte(fmt.Sprintf(
		"project=%s schema=%s watermark=%t applied=%t workers=%d buffer=%d committed=%d resumed=%d\n",
		result.ProjectID, result.SchemaVersion, result.WatermarkEnabled, result.WatermarkApplied,
		result.WorkersUsed, result.BufferLimit, result.LastCommittedSeq, result.ResumedFromSeq))...)
	for _, path := range result.PathDecisions {
		b = append(b, []byte(fmt.Sprintf("path %s ratio=%d/%d threshold=%d mode=%s reason=%s\n",
			path.FeatureID, path.RatioNum, path.RatioDen, path.Threshold, path.Mode, path.ReasonCode))...)
	}
	for _, w := range result.Windows {
		b = append(b, []byte(fmt.Sprintf("window %s count=%d aggs=%v status=%s recompute=%d\n",
			w.Key, w.ObservationCount, w.Aggregates, w.Status, w.RecomputeCount))...)
	}
	for _, d := range result.Discards {
		b = append(b, []byte(fmt.Sprintf(
			"discard event=%d feature=%s window=%s late_ms=%d reason=%s\n",
			d.EventSeq, d.FeatureID, d.Window, d.LateByMs, d.ReasonCode))...)
	}
	return string(b)
}

func printJSON(value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
