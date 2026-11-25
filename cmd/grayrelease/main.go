package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"grayrelease/internal/acceptance"
	"grayrelease/internal/checkpoint"
	"grayrelease/internal/durable"
	"grayrelease/internal/engine"
	"grayrelease/internal/fault"
	"grayrelease/internal/incremental"
	"grayrelease/internal/loader"
	"grayrelease/internal/model"
	"grayrelease/internal/sample"
	"grayrelease/internal/scheduler"
)

func main() {
	if len(os.Args) < 2 {
		printHelp()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "gen":
		err = runGen(os.Args[2:])
	case "demo", "verify":
		err = runDemo(os.Args[2:])
	case "simulate":
		err = runSimulate(os.Args[2:])
	case "parallel-check":
		err = runParallelCheck(os.Args[2:])
	case "backpressure-demo":
		err = runBackpressureDemo(os.Args[2:])
	case "incremental-demo":
		err = runIncrementalDemo(os.Args[2:])
	case "recovery-run", "recovery-resume":
		err = runRecovery(os.Args[1], os.Args[2:])
	case "inject":
		os.Exit(runInject(os.Args[2:]))
	case "help", "-h", "--help":
		printHelp()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		if be, ok := model.AsBizError(err); ok {
			writeJSON(os.Stderr, map[string]any{"ok": false, "error": be})
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func printHelp() {
	fmt.Print(`grayrelease acceptance kernel

Commands:
  gen [--dir samples]
  demo [--samples samples] [--out artifacts/demo-report.json]
  simulate --project samples/project-v2.json --requests samples/requests-v2.json [--out artifacts/sim.json]
  parallel-check --samples samples
  backpressure-demo --samples samples [--route-buffer 2] [--workers 4]
  incremental-demo [--samples samples] [--out artifacts/incremental.json]
  recovery-run --db artifacts/recovery.db [--samples samples] [--crash-tick 1 --crash-stage after-queues]
  recovery-resume --db artifacts/recovery.db [--samples samples]
  inject --kind overlap|bad-scale|missing-service|ambiguous-scale|truncated-checkpoint|version-mismatch|worker-budget|worker-budget-exhausted [--workdir artifacts/inject]
`)
}

func runGen(args []string) error {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	dir := fs.String("dir", "samples", "sample output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := sample.WriteSet(*dir); err != nil {
		return err
	}
	return writeJSON(os.Stdout, map[string]any{"ok": true, "sample_dir": *dir, "seed": sample.Seed})
}

func runDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	samplesDir := fs.String("samples", "samples", "deterministic sample directory")
	out := fs.String("out", "artifacts/demo-report.json", "full JSON report output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	report, err := acceptance.Run(*samplesDir)
	if err != nil {
		return err
	}
	if *out != "" {
		if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
			return err
		}
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(*out, data, 0o644); err != nil {
			return err
		}
	}
	summary := map[string]any{
		"ok": report.OK, "sample_dir": report.SampleDir, "checks": report.Checks,
		"decisions":              len(report.Simulation.Decisions),
		"tick_reports":           len(report.Simulation.Reports),
		"buffer_backpressure":    report.BufferBackpressure,
		"sample_edit_equivalent": report.SampleEdit.EquivalentToFull,
		"weight_edit_equivalent": report.WeightEdit.EquivalentToFull,
		"checkpoint_tick":        report.CheckpointTick,
		"report_path":            *out,
	}
	if !report.OK {
		_ = writeJSON(os.Stdout, summary)
		return model.NewError("E_ACCEPTANCE_FAILED", "one or more acceptance checks failed")
	}
	return writeJSON(os.Stdout, summary)
}

func runSimulate(args []string) error {
	fs := flag.NewFlagSet("simulate", flag.ContinueOnError)
	projectPath := fs.String("project", "samples/project-v2.json", "project JSON")
	requestsPath := fs.String("requests", "samples/requests-v2.json", "requests JSON")
	out := fs.String("out", "", "optional full JSON output file")
	workers := fs.Int("workers", 4, "worker budget")
	routeBuffer := fs.Int("route-buffer", 32, "per-service route buffer")
	mirrorConcurrency := fs.Int("mirror-concurrency", 1, "per-service mirror concurrency")
	shadowBuffer := fs.Int("shadow-buffer", 2, "per-service shadow buffer")
	onFull := fs.String("shadow-on-full", "drop", "drop or defer")
	if err := fs.Parse(args); err != nil {
		return err
	}
	project, err := loader.LoadProjectFile(*projectPath)
	if err != nil {
		return err
	}
	requests, err := loader.LoadRequestsFile(*requestsPath, project.SchemaVersion, project.TickIntervalMS)
	if err != nil {
		return err
	}
	config := engine.Config{
		WorkerBudget: *workers, RouteBufferPerService: *routeBuffer,
		MirrorConcurrencyPerService: *mirrorConcurrency, ShadowBufferPerService: *shadowBuffer,
		ShadowOnFull: *onFull, ShadowLatencyTicks: 1, ShadowTimeoutTicks: 2,
	}
	result, err := engine.Simulate(project, requests, config)
	if err != nil {
		return err
	}
	if *out != "" {
		if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
			return err
		}
		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(*out, data, 0o644); err != nil {
			return err
		}
	}
	dropped := 0
	deferred := 0
	for _, report := range result.Reports {
		dropped += len(report.DroppedSet)
		deferred += len(report.DeferredSet)
	}
	return writeJSON(os.Stdout, map[string]any{
		"ok": true, "decisions": len(result.Decisions), "reports": len(result.Reports),
		"dropped": dropped, "deferred": deferred, "output": *out,
	})
}

func runParallelCheck(args []string) error {
	fs := flag.NewFlagSet("parallel-check", flag.ContinueOnError)
	samplesDir := fs.String("samples", "samples", "sample directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	project, err := loader.LoadProjectFile(filepath.Join(*samplesDir, "project-v2.json"))
	if err != nil {
		return err
	}
	requests, err := loader.LoadRequestsFile(filepath.Join(*samplesDir, "requests-v2.json"), model.SchemaV2, project.TickIntervalMS)
	if err != nil {
		return err
	}
	base := acceptance.DemoConfig()
	one, err := scheduler.RouteParallelByTick(project, requests, withWorkers(base, 1), scheduler.Options{})
	if err != nil {
		return err
	}
	eight, err := scheduler.RouteParallelByTick(project, requests, withWorkers(base, 8), scheduler.Options{})
	if err != nil {
		return err
	}
	equal := len(one.Decisions) == len(eight.Decisions)
	if equal {
		for i := range one.Decisions {
			if !engine.EqualDecisions(one.Decisions[i], eight.Decisions[i]) {
				equal = false
				break
			}
		}
	}
	return writeJSON(os.Stdout, map[string]any{
		"ok": equal, "decisions": len(eight.Decisions),
		"peak_workers_1": one.PeakActive, "peak_workers_8": eight.PeakActive,
	})
}

func runBackpressureDemo(args []string) error {
	fs := flag.NewFlagSet("backpressure-demo", flag.ContinueOnError)
	samplesDir := fs.String("samples", "samples", "sample directory")
	workers := fs.Int("workers", 4, "declared worker budget")
	buffer := fs.Int("route-buffer", 2, "per-service pending request buffer per logical tick")
	if err := fs.Parse(args); err != nil {
		return err
	}
	project, requests, err := loadV2Sample(*samplesDir)
	if err != nil {
		return err
	}
	config := acceptance.DemoConfig()
	config.WorkerBudget = *workers
	config.RouteBufferPerService = *buffer
	output, err := scheduler.RouteParallelByTick(project, requests, config, scheduler.Options{})
	if err != nil {
		return err
	}
	codes := map[string]int{}
	for _, item := range output.Backpressure {
		codes[item.Code]++
	}
	examples := make([]BackpressureView, 0, 3)
	for i, item := range output.Backpressure {
		if i >= 3 {
			break
		}
		examples = append(examples, BackpressureView{
			RequestID: item.RequestID, Service: item.Service, Code: item.Code, Reason: item.Reason,
		})
	}
	return writeJSON(os.Stdout, map[string]any{
		"ok": true, "worker_budget": *workers, "route_buffer_per_service": *buffer,
		"processed": output.Processed, "backpressure_count": len(output.Backpressure),
		"codes": codes, "examples": examples,
	})
}

type BackpressureView struct {
	RequestID string `json:"request_id"`
	Service   string `json:"service"`
	Code      string `json:"code"`
	Reason    string `json:"reason"`
}

func withWorkers(config engine.Config, workers int) engine.Config {
	config.WorkerBudget = workers
	return config
}

func runIncrementalDemo(args []string) error {
	fs := flag.NewFlagSet("incremental-demo", flag.ContinueOnError)
	samplesDir := fs.String("samples", "samples", "sample directory")
	out := fs.String("out", "artifacts/incremental-report.json", "full report output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	project, requests, err := loadV2Sample(*samplesDir)
	if err != nil {
		return err
	}
	manager, err := incremental.NewManager(project, requests, acceptance.DemoConfig())
	if err != nil {
		return err
	}
	weight := 6500
	edit := incremental.Edit{
		ID: "cli-weight-edit", Kind: "rule_weight", Service: "checkout", RuleID: "R-CHK-BETA",
		AtTick: 3, WeightBPS: &weight,
	}
	report, err := manager.Apply(edit)
	if err != nil {
		return err
	}
	if *out != "" {
		if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
			return err
		}
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(*out, data, 0o644); err != nil {
			return err
		}
	}
	return writeJSON(os.Stdout, map[string]any{
		"ok": report.EquivalentToFull, "closure": report.Closure,
		"recomputed_ticks": report.RecomputedTicks, "reused_ticks": report.ReusedTicks,
		"recomputed_requests": report.RecomputedRequests, "reused_requests": report.ReusedRequests,
		"report_path": *out,
	})
}

func runRecovery(command string, args []string) error {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	dbPath := fs.String("db", "artifacts/recovery.db", "sqlite checkpoint path")
	samplesDir := fs.String("samples", "samples", "sample directory")
	endTick := fs.Int("end-tick", sample.TickCount+1, "final logical tick inclusive")
	crashTick := fs.Int("crash-tick", 1, "tick to inject crash")
	crashStage := fs.String("crash-stage", "", "after-routes|after-queues|before-commit|after-commit")
	if command == "recovery-resume" {
		*crashTick = -1
		*crashStage = ""
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	project, requests, err := loadV2Sample(*samplesDir)
	if err != nil {
		return err
	}
	store, err := checkpoint.OpenStore(*dbPath, project.EngineeringVersion)
	if err != nil {
		return err
	}
	defer store.Close()

	var plan *durable.CrashPlan
	if *crashStage != "" {
		plan = &durable.CrashPlan{Tick: *crashTick, Stage: *crashStage}
	}
	runner := durable.NewRunner(project, requests, acceptance.DemoConfig(), store)
	report, err := runner.RunUntil(*endTick, plan)
	if err != nil {
		return err
	}
	committed, err := store.CommittedTick()
	if err != nil {
		return err
	}
	return writeJSON(os.Stdout, map[string]any{
		"ok": true, "db": *dbPath, "committed_tick": committed,
		"started_at_tick": report.StartedAtTick, "processed_ticks": report.ProcessedTicks,
		"recovery": report.Recovery,
	})
}

func runInject(args []string) int {
	fs := flag.NewFlagSet("inject", flag.ContinueOnError)
	kind := fs.String("kind", "", "injection kind")
	workdir := fs.String("workdir", "artifacts/inject", "injection artifact directory")
	if err := fs.Parse(args); err != nil {
		writeJSON(os.Stderr, map[string]any{"ok": false, "error": err.Error()})
		return 2
	}
	bizErr := fault.Inject(*kind, *workdir)
	if bizErr == nil {
		_ = writeJSON(os.Stdout, map[string]any{"ok": true, "kind": *kind})
		return 0
	}
	_ = writeJSON(os.Stdout, map[string]any{"injected": *kind, "rejected": true, "error": bizErr})
	return 1
}

func loadV2Sample(dir string) (*model.Project, []model.Request, error) {
	projectPath := filepath.Join(dir, "project-v2.json")
	requestsPath := filepath.Join(dir, "requests-v2.json")
	if _, err := os.Stat(projectPath); os.IsNotExist(err) {
		if err := sample.WriteSet(dir); err != nil {
			return nil, nil, err
		}
	}
	project, err := loader.LoadProjectFile(projectPath)
	if err != nil {
		return nil, nil, err
	}
	requests, err := loader.LoadRequestsFile(requestsPath, model.SchemaV2, project.TickIntervalMS)
	if err != nil {
		return nil, nil, err
	}
	return project, requests, nil
}

func writeJSON(file *os.File, value any) error {
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
