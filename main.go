package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	gw "gatewayproto/internal/gateway"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "version":
		fmt.Println("gatewayproto 0.1.0 (go1.22+, deterministic local prototype)")
	case "generate":
		err = runGenerate(os.Args[2:])
	case "validate":
		err = runValidate(os.Args[2:])
	case "run":
		err = runPipeline(os.Args[2:])
	case "inspect-checkpoint":
		err = runInspect(os.Args[2:])
	case "demo":
		err = runDemo(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		err = gw.NewError(gw.CodeFieldType, "command", "unknown command: %s", os.Args[1])
	}
	if err != nil {
		emitError(err)
		os.Exit(exitCode(err))
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `gatewayproto commands:
  generate [--seed N] [--out DIR]
  validate --schema FILE [--parallel] [--out report.json]
  run --schema FILE --db PATH --export-dir DIR [--parallel] [--crash-after-batch N]
  inspect-checkpoint --db PATH [--expect-version V]
  demo
`)
}

func emitError(err error) {
	ge, ok := gw.AsError(err)
	if !ok {
		ge = gw.NewError("E_INTERNAL", "$", "%v", err)
	}
	_ = json.NewEncoder(os.Stderr).Encode(map[string]any{"ok": false, "error": ge})
}

func exitCode(err error) int {
	ge, ok := gw.AsError(err)
	if !ok {
		return 1
	}
	if ge.Code == gw.CodeSignalGraceful {
		return 130
	}
	return 2
}

func runGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	seed := fs.Int64("seed", gw.DefaultSeed, "fixed deterministic seed")
	out := fs.String("out", "artifacts/sample", "output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	bundle, err := gw.GenerateSample(*seed)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	files := map[string][]byte{
		"canonical-project.json": mustJSON(bundle.Canonical),
		"schema-v1.json":         bundle.Schemas["v1"],
		"schema-v2.json":         bundle.Schemas["v2"],
		"schema-v3.json":         bundle.Schemas["v3"],
		"truth-report.json":      mustJSON(bundle.Truth),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(*out, name), data, 0o644); err != nil {
			return err
		}
	}
	fmt.Printf("generated seed=%d services=%d routes=%d records=%d windows=%d batches=%d dir=%s\n",
		*seed, len(bundle.Canonical.Services), len(bundle.Canonical.Routes), len(bundle.Canonical.Records),
		len(bundle.Truth.Windows), len(bundle.Truth.Batches), *out)
	return nil
}

func mustJSON(v any) []byte {
	data, err := gw.MarshalPretty(v)
	if err != nil {
		panic(err)
	}
	return data
}

func loadProjectFile(path string) (*gw.Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, gw.NewError(gw.CodeCheckpointOpen, "schema", "cannot read schema file: %v", err).WithContext("path", path)
	}
	return gw.LoadSchema(data)
}

func runValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	schemaPath := fs.String("schema", "", "schema JSON file")
	out := fs.String("out", "", "optional report JSON output")
	parallel := fs.Bool("parallel", false, "use bounded parallel service engine")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *schemaPath == "" {
		return gw.NewError(gw.CodeFieldRequired, "--schema", "schema file is required")
	}
	project, err := loadProjectFile(*schemaPath)
	if err != nil {
		return err
	}
	report, err := runEngine(project, *parallel)
	if err != nil {
		return err
	}
	if *out != "" {
		if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(*out, mustJSON(report), 0o644); err != nil {
			return err
		}
	}
	fmt.Printf("valid schema=%s version=%s windows=%d committed_batches=%d drops=%d watermark=%s source=%s\n",
		project.Schema, project.ProjectVersion, len(report.Windows), len(report.Batches), len(report.Dropped),
		watermarkText(report.WatermarkMS), report.WatermarkSource)
	return nil
}

func runEngine(project *gw.Project, parallel bool) (*gw.RunReport, error) {
	if parallel {
		return gw.RunParallel(project)
	}
	engine, err := gw.NewEngine(project)
	if err != nil {
		return nil, err
	}
	return engine.Run()
}

func watermarkText(wm *int64) string {
	if wm == nil {
		return "none"
	}
	return fmt.Sprint(*wm)
}

func runPipeline(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	schemaPath := fs.String("schema", "", "schema JSON file")
	dbPath := fs.String("db", "artifacts/checkpoint.sqlite", "SQLite checkpoint path")
	exportDir := fs.String("export-dir", "artifacts/export", "export batch directory")
	parallel := fs.Bool("parallel", false, "parallel service aggregation")
	crashAfterBatch := fs.Int("crash-after-batch", 0, "send SIGKILL after this many newly committed batches")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *schemaPath == "" {
		return gw.NewError(gw.CodeFieldRequired, "--schema", "schema file is required")
	}
	project, err := loadProjectFile(*schemaPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cp, state, recovery, err := openCheckpointForProject(ctx, *dbPath, project)
	if err != nil {
		return err
	}
	defer cp.Close()

	report, err := runEngine(project, *parallel)
	if err != nil {
		return err
	}
	if _, err := cp.SaveWindowCommit(ctx, project, report.Windows); err != nil {
		return err
	}
	if err := os.MkdirAll(*exportDir, 0o755); err != nil {
		return err
	}

	alreadyExported := map[int]bool{}
	if state != nil {
		for _, batch := range state.Batches {
			alreadyExported[batch.BatchNo] = true
		}
	}
	written := 0
	skipped := 0
	for _, batch := range report.Batches {
		select {
		case <-ctx.Done():
			return gw.NewError(gw.CodeSignalGraceful, "signal", "graceful stop requested at batch boundary: %v", ctx.Err())
		default:
		}
		if alreadyExported[batch.BatchNo] {
			skipped++
			continue
		}
		path := filepath.Join(*exportDir, fmt.Sprintf("batch-%04d.json", batch.BatchNo))
		if err := atomicWriteJSON(path, batch); err != nil {
			return err
		}
		if err := cp.SaveExportBatch(ctx, batch); err != nil {
			return err
		}
		written++
		if *crashAfterBatch > 0 && written >= *crashAfterBatch {
			fmt.Fprintf(os.Stderr, "injecting SIGKILL after %d committed batch(es)\n", written)
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			select {}
		}
	}
	var sequence bytes.Buffer
	sequenceEncoder := json.NewEncoder(&sequence)
	for _, batch := range report.Batches {
		if err := sequenceEncoder.Encode(batch); err != nil {
			return err
		}
	}
	if err := atomicWriteFile(filepath.Join(*exportDir, "export-sequence.jsonl"), sequence.Bytes()); err != nil {
		return err
	}
	manifest := map[string]any{
		"project_version": project.ProjectVersion,
		"recovery":        recovery,
		"batch_count":     len(report.Batches),
		"written":         written,
		"skipped":         skipped,
		"batches":         report.Batches,
	}
	if err := atomicWriteJSON(filepath.Join(*exportDir, "export-manifest.json"), manifest); err != nil {
		return err
	}
	if err := atomicWriteJSON(filepath.Join(filepath.Dir(*dbPath), "run-report.json"), report); err != nil {
		return err
	}
	fmt.Printf("pipeline complete version=%s recovery=%s batches=%d written=%d resumed_skipped=%d export_dir=%s\n",
		project.ProjectVersion, recovery, len(report.Batches), written, skipped, *exportDir)
	return nil
}

func openCheckpointForProject(ctx context.Context, dbPath string, project *gw.Project) (*gw.Checkpoint, *gw.CheckpointState, string, error) {
	cp, err := gw.OpenCheckpoint(dbPath)
	if err != nil {
		return nil, nil, "", err
	}
	state, err := cp.LoadState(ctx, project.ProjectVersion)
	if err == nil {
		return cp, state, "version_match", nil
	}
	ge, ok := gw.AsError(err)
	if !ok || ge.Code != gw.CodeCheckpointVersion {
		_ = cp.Close()
		return nil, nil, "", err
	}
	if err := cp.Close(); err != nil {
		return nil, nil, "", err
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(dbPath + suffix)
	}
	fresh, err := gw.OpenCheckpoint(dbPath)
	if err != nil {
		return nil, nil, "", err
	}
	return fresh, nil, "version_mismatch_full_recompute", nil
}

func atomicWriteJSON(path string, value any) error {
	data, err := gw.MarshalPretty(value)
	if err != nil {
		return err
	}
	return atomicWriteFile(path, data)
}

func atomicWriteFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-export-*")
	if err != nil {
		return gw.NewError(gw.CodeCheckpointWrite, "export.file", "cannot create temporary export: %v", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return gw.NewError(gw.CodeCheckpointWrite, "export.file", "cannot write export: %v", err).WithContext("path", path)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return gw.NewError(gw.CodeCheckpointWrite, "export.file", "cannot atomically publish export: %v", err).WithContext("path", path)
	}
	return nil
}

func runInspect(args []string) error {
	fs := flag.NewFlagSet("inspect-checkpoint", flag.ContinueOnError)
	dbPath := fs.String("db", "artifacts/checkpoint.sqlite", "SQLite checkpoint path")
	expectVersion := fs.String("expect-version", "", "expected project version")
	asJSON := fs.Bool("json", false, "print full state as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cp, err := gw.OpenCheckpoint(*dbPath)
	if err != nil {
		return err
	}
	defer cp.Close()
	state, err := cp.LoadState(context.Background(), *expectVersion)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(state)
	}
	fmt.Printf("version=%s commit_seq=%d windows=%d exported_batches=%d last_batch=%d\n",
		state.ProjectVersion, state.GlobalCommitSeq, len(state.Windows), len(state.Batches), state.LastExportBatchNo)
	return nil
}

func runDemo(_ []string) error {
	dir := filepath.Join("artifacts", "demo")
	if err := runGenerate([]string{"--out", filepath.Join(dir, "sample")}); err != nil {
		return err
	}
	schema := filepath.Join(dir, "sample", "schema-v1.json")
	db := filepath.Join(dir, "checkpoint.sqlite")
	exportDir := filepath.Join(dir, "export")
	if err := runPipeline([]string{"--schema", schema, "--db", db, "--export-dir", exportDir}); err != nil {
		return err
	}
	return nil
}
