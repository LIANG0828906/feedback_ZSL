package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/liang/collector-backpressure/bpderr"
	"github.com/liang/collector-backpressure/checkpoint"
	"github.com/liang/collector-backpressure/engine"
	"github.com/liang/collector-backpressure/model"
	"github.com/liang/collector-backpressure/sample"
	"github.com/liang/collector-backpressure/schema"
)

type summary struct {
	OK                bool             `json:"ok"`
	Code              bpderr.Code      `json:"code,omitempty"`
	Message           string           `json:"message,omitempty"`
	ProjectID         string           `json:"project_id,omitempty"`
	ProjectVersion    string           `json:"project_version,omitempty"`
	SchemaVersion     string           `json:"schema_version,omitempty"`
	ResumedFromTick   *int             `json:"resumed_from_tick,omitempty"`
	TicksReturned     int              `json:"ticks_returned"`
	LastTick          int              `json:"last_tick"`
	DropCount         int              `json:"drop_count"`
	SignalCount       int              `json:"signal_count"`
	ObservedCodes     []string         `json:"observed_codes,omitempty"`
	AgentElapsedNanos map[string]int64 `json:"agent_elapsed_ns,omitempty"`
	CheckpointTick    *int             `json:"checkpoint_tick,omitempty"`
}

type crashSink struct {
	store      *checkpoint.Store
	crashAfter int
}

func (s *crashSink) SaveSnapshot(ctx context.Context, p *model.Project, snap engine.Snapshot) error {
	if err := s.store.SaveSnapshot(ctx, p, snap); err != nil {
		return err
	}
	if s.crashAfter >= 0 && snap.Tick == s.crashAfter {
		// Simulates SIGKILL/power loss immediately after a durable commit.
		os.Exit(137)
	}
	return nil
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "sample":
		if err := sampleCmd(os.Args[2:]); err != nil {
			fatal(err)
		}
	case "run":
		code := runCmd(os.Args[2:])
		os.Exit(code)
	case "version":
		fmt.Println("bpctl 0.1.0; go module collector-backpressure")
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: bpctl <sample|run|version> [flags]")
}

func sampleCmd(args []string) error {
	fs := flag.NewFlagSet("sample", flag.ContinueOnError)
	dir := fs.String("dir", "samples", "directory for deterministic v1/v2/v3 JSON samples")
	seed := fs.Int64("seed", sample.DefaultSeed, "deterministic generator seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p := sample.GenerateProject(*seed)
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}
	for _, version := range []string{schema.V1, schema.V2, schema.V3} {
		data, err := schema.RenderJSON(version, p)
		if err != nil {
			return err
		}
		path := filepath.Join(*dir, version+".json")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return err
		}
		fmt.Println(path)
	}
	return nil
}

func runCmd(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	schemaName := fs.String("schema", schema.V3, "schema version: collector.v1, collector.v2, collector.v3")
	dbPath := fs.String("db", "", "SQLite checkpoint path; empty disables persistence")
	workers := fs.Int("workers", engine.DefaultBudget().Workers, "worker budget [1,64]")
	queue := fs.Int("queue", engine.DefaultBudget().PerAgentQueue, "per-agent pending queue budget [1,100000]")
	tickDelay := fs.Duration("tick-delay", 0, "optional delay between ticks for signal drills")
	crashAfter := fs.Int("crash-after-tick", -1, "exit immediately after durable checkpoint at this tick")
	outPath := fs.String("out", "", "optional JSON summary output file")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	p := sample.GenerateProject(sample.DefaultSeed)
	data, err := schema.RenderJSON(*schemaName, p)
	if err != nil {
		return printError(err, *outPath)
	}
	loaded, err := schema.LoadJSON(data)
	if err != nil {
		return printError(err, *outPath)
	}

	budget := engine.Budget{Workers: *workers, PerAgentQueue: *queue, SchedulerQueue: 128}
	runnerOpts := []engine.Option{engine.WithTickDelay(*tickDelay)}
	var store *checkpoint.Store
	var resumedFrom *int
	if *dbPath != "" {
		var openErr error
		store, openErr = checkpoint.Open(ctx, *dbPath)
		if openErr != nil {
			return printError(openErr, *outPath)
		}
		defer store.Close()
		cp, loadErr := store.LoadMatching(ctx, loaded, false)
		if loadErr != nil {
			var biz *bpderr.Error
			if errors.As(loadErr, &biz) && biz.Code == bpderr.ECheckpointVersion {
				fmt.Fprintf(os.Stderr, "checkpoint version mismatch; resetting and recomputing from start: %v\n", loadErr)
				if err := store.Reset(ctx); err != nil {
					return printError(err, *outPath)
				}
			} else {
				return printError(loadErr, *outPath)
			}
		} else if cp != nil {
			resumedFrom = &cp.LastTick
			runnerOpts = append(runnerOpts, engine.WithSink(&crashSink{store: store, crashAfter: *crashAfter}))
			runner, err := engine.NewRunner(budget, runnerOpts...)
			if err != nil {
				return printError(err, *outPath)
			}
			sim, runErr := runner.RunFromSnapshot(ctx, loaded, &cp.Snapshot)
			return finishRun(sim, runErr, loaded, resumedFrom, store, *outPath)
		}
		runnerOpts = append(runnerOpts, engine.WithSink(&crashSink{store: store, crashAfter: *crashAfter}))
	}

	runner, err := engine.NewRunner(budget, runnerOpts...)
	if err != nil {
		return printError(err, *outPath)
	}
	sim, runErr := runner.Run(ctx, loaded)
	return finishRun(sim, runErr, loaded, resumedFrom, store, *outPath)
}

func finishRun(sim *engine.Simulation, runErr error, p *model.Project, resumedFrom *int, store *checkpoint.Store, outPath string) int {
	summary := summary{
		ProjectID: p.ID, ProjectVersion: p.Version, SchemaVersion: p.SchemaVersion,
		ResumedFromTick: resumedFrom, AgentElapsedNanos: map[string]int64{},
	}
	if sim != nil {
		summary.TicksReturned = len(sim.Result.Ticks)
		if len(sim.Result.Ticks) > 0 {
			summary.LastTick = sim.Result.Ticks[len(sim.Result.Ticks)-1].Tick
		}
		codeSet := map[string]struct{}{}
		for _, tick := range sim.Result.Ticks {
			summary.DropCount += len(tick.Drops)
			summary.SignalCount += len(tick.Signals)
			for _, code := range tick.ObservedCodes {
				codeSet[code] = struct{}{}
			}
			for _, agent := range tick.Agents {
				summary.AgentElapsedNanos[agent.AgentID] = agent.ElapsedNanoseconds
			}
		}
		for code := range codeSet {
			summary.ObservedCodes = append(summary.ObservedCodes, code)
		}
	}
	if store != nil {
		if cp, err := store.LoadLatest(context.Background()); err == nil && cp != nil {
			tick := cp.LastTick
			summary.CheckpointTick = &tick
		}
	}
	if runErr != nil {
		if biz, ok := bpderr.As(runErr); ok {
			summary.OK = false
			summary.Code = biz.Code
			summary.Message = biz.Message
		} else {
			summary.OK = false
			summary.Code = "E_RUN_FAILED"
			summary.Message = runErr.Error()
		}
		writeSummary(summary, outPath)
		if summary.Code == bpderr.EInterrupted {
			return 130
		}
		return 1
	}
	summary.OK = true
	writeSummary(summary, outPath)
	return 0
}

func writeSummary(summary summary, outPath string) {
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	fmt.Println(string(data))
	if outPath != "" {
		if err := os.WriteFile(outPath, data, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}
}

func printError(err error, outPath string) int {
	summary := summary{OK: false}
	if biz, ok := bpderr.As(err); ok {
		summary.Code = biz.Code
		summary.Message = biz.Error()
	} else {
		summary.Code = "E_CLI"
		summary.Message = err.Error()
	}
	writeSummary(summary, outPath)
	return 1
}

func fatal(err error) {
	code := printError(err, "")
	os.Exit(code)
}
