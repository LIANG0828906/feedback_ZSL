package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"iconflow/checkpoint"
	"iconflow/errs"
	"iconflow/generator"
	"iconflow/graph"
	"iconflow/model"
	"iconflow/pipeline"
	"iconflow/sampleio"
	"iconflow/workbench"
)

type verificationReport struct {
	GeneratedAt       string                   `json:"generated_at"`
	Seed              int64                    `json:"seed"`
	SampleDir         string                   `json:"sample_dir"`
	Blocks            int                      `json:"blocks"`
	SchemaLoads       []schemaLoadReport       `json:"schema_loads"`
	LossItems         int                      `json:"loss_items"`
	ConsistencyPassed bool                     `json:"consistency_passed"`
	Diff              *errs.Error              `json:"diff,omitempty"`
	BlockSummaries    []workbench.BlockSummary `json:"block_summaries"`
	SelectiveManifest *graph.Manifest          `json:"selective_manifest"`
	GlobalManifest    *graph.Manifest          `json:"global_manifest"`
	DirtyManifest     *graph.Manifest          `json:"dirty_manifest"`
	CheckpointBackend string                   `json:"checkpoint_backend"`
	SQLiteStages      int                      `json:"sqlite_stages"`
	SQLiteWindows     int                      `json:"sqlite_windows"`
	QueueEvents       int                      `json:"queue_events"`
}

type schemaLoadReport struct {
	Version        string `json:"version"`
	Path           string `json:"path"`
	OK             bool   `json:"ok"`
	Images         int    `json:"images"`
	Blocks         int    `json:"blocks"`
	UpgradeRecords int    `json:"upgrade_records"`
	Traces         int    `json:"traces"`
	Error          string `json:"error,omitempty"`
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "generate":
		generateCmd(os.Args[2:])
	case "verify", "demo":
		verifyCmd(os.Args[2:])
	case "serve":
		serveCmd(os.Args[2:])
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		printUsage()
		os.Exit(2)
	}
}

func printUsage() {
	fmt.Println(`iconflow commands:
  generate [-dir samples/seed-42]
  verify   [-samples samples/seed-42] [-report artifacts/verification-report.json] [-sqlite artifacts/iconflow.sqlite]
  serve    [-addr 127.0.0.1:8088] [-sqlite artifacts/iconflow.sqlite]`)
}

func generateCmd(args []string) {
	fs := flag.NewFlagSet("generate", flag.ExitOnError)
	dir := fs.String("dir", "samples/seed-42", "sample output directory")
	_ = fs.Parse(args)
	project := generator.GenerateProject(generator.FixedSeed)
	set, err := sampleio.WriteSet(*dir, project)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("generated seed=%d blocks=%d dir=%s\n", project.Seed, len(project.SortedBlockCoords()), set.Dir)
	for version, path := range set.Documents {
		fmt.Printf("  %s -> %s\n", version, path)
	}
}

func verifyCmd(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	sampleDir := fs.String("samples", "samples/seed-42", "deterministic sample directory")
	reportPath := fs.String("report", "artifacts/verification-report.json", "JSON report path")
	sqlitePath := fs.String("sqlite", "artifacts/iconflow.sqlite", "SQLite checkpoint path")
	_ = fs.Parse(args)

	ctx := context.Background()
	project := generator.GenerateProject(generator.FixedSeed)
	set, sampleErr := sampleio.WriteSet(*sampleDir, project)
	if sampleErr != nil {
		fatal(sampleErr)
	}

	report := verificationReport{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Seed:        project.Seed, SampleDir: *sampleDir,
		Blocks: len(project.SortedBlockCoords()),
	}
	for _, version := range []string{model.SchemaV1, model.SchemaV2, model.SchemaV3} {
		path := set.Documents[version]
		load, err := sampleio.LoadFile(path)
		entry := schemaLoadReport{Version: version, Path: path}
		if err != nil {
			entry.Error = err.Error()
			report.SchemaLoads = append(report.SchemaLoads, entry)
			continue
		}
		entry.OK = true
		entry.Images = len(load.Project.Images)
		entry.Blocks = len(load.Project.SortedBlockCoords())
		entry.UpgradeRecords = len(load.UpgradeRecords)
		entry.Traces = len(load.Traces)
		report.SchemaLoads = append(report.SchemaLoads, entry)
		if version == model.SchemaV1 && set.Exports[version].Loss != nil {
			report.LossItems = len(set.Exports[version].Loss.Items)
		}
	}

	consistency, err := pipeline.RunConsistency(ctx, project, pipeline.Options{Budget: pipeline.DefaultBudget()})
	if err != nil {
		fatal(err)
	}
	report.ConsistencyPassed = consistency.Passed
	report.Diff = consistency.Diff
	report.BlockSummaries = workbench.SummarizeRun(consistency.Realtime)

	coords := project.SortedBlockCoords()
	target := coords[4].Key()
	only := map[string]bool{target: true}
	report.SelectiveManifest = graph.ProjectGraph(project).Impact([]string{"threshold_scale", "channel_order"}, only)
	report.GlobalManifest = graph.ProjectGraph(project).Impact([]string{"channel_order"}, nil)
	report.DirtyManifest = graph.ImpactDirty(graph.ProjectGraph(project), project.Dirty)

	if err := os.MkdirAll(filepath.Dir(*sqlitePath), 0o755); err != nil {
		fatal(err)
	}
	_ = os.Remove(*sqlitePath)
	_ = os.Remove(*sqlitePath + "-wal")
	_ = os.Remove(*sqlitePath + "-shm")
	_ = os.Remove(*sqlitePath + ".json")
	var store checkpoint.Store
	backend := "sqlite"
	sqliteStore, openErr := checkpoint.OpenSQLite(*sqlitePath)
	if openErr == nil {
		store = sqliteStore
	} else {
		backend = "atomic-json-fallback"
		fileStore, fileErr := checkpoint.OpenFileStore(*sqlitePath + ".json")
		if fileErr != nil {
			fatal(fileErr)
		}
		store = fileStore
	}
	report.CheckpointBackend = backend
	runner, runnerErr := pipeline.NewRunner(ctx, project, pipeline.Options{
		Mode: "sqlite-verification", Store: store, Budget: pipeline.Budget{
			Workers: 2, MaxPixelsPerThread: 1024, MaxQueuedBlocks: 2,
		},
	})
	if runnerErr != nil {
		fatal(runnerErr)
	}
	runReport, runErr := runner.Run(ctx)
	if runErr != nil {
		fatal(runErr)
	}
	history, _ := store.StageHistory(ctx, project.ProjectID)
	windows, _ := store.WindowHistory(ctx, project.ProjectID)
	report.SQLiteStages = len(history)
	report.SQLiteWindows = len(windows)
	report.QueueEvents = len(runReport.QueueEvents)
	_ = store.Close()
	if backend == "sqlite" {
		_ = os.Remove(*sqlitePath + "-wal")
		_ = os.Remove(*sqlitePath + "-shm")
	}

	if err := os.MkdirAll(filepath.Dir(*reportPath), 0o755); err != nil {
		fatal(err)
	}
	data, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(*reportPath, data, 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("samples=%s blocks=%d schema_loads=%d consistency=%v checkpoint=%s stages=%d windows=%d queue_waits=%d report=%s\n",
		*sampleDir, report.Blocks, len(report.SchemaLoads), report.ConsistencyPassed,
		report.CheckpointBackend, report.SQLiteStages, report.SQLiteWindows, report.QueueEvents, *reportPath)
	if !report.ConsistencyPassed {
		fmt.Fprintln(os.Stderr, report.Diff)
		os.Exit(1)
	}
}

func serveCmd(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8088", "listen address")
	sqlitePath := fs.String("sqlite", "artifacts/iconflow.sqlite", "SQLite checkpoint path")
	_ = fs.Parse(args)
	project := generator.GenerateProject(generator.FixedSeed)
	server := workbench.NewServer(project, *sqlitePath)
	fmt.Printf("IconFlow workbench: http://%s\n", *addr)
	if err := http.ListenAndServe(*addr, server.Handler()); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	if domainErr, ok := errs.AsError(err); ok {
		fmt.Fprintln(os.Stderr, domainErr.Error())
		if domainErr.Details != nil {
			data, _ := json.MarshalIndent(domainErr.Details, "", "  ")
			fmt.Fprintln(os.Stderr, string(data))
		}
	} else {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(1)
}
