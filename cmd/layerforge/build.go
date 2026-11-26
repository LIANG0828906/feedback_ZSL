package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"layerforge/internal/apperr"
	"layerforge/internal/checkpoint"
	"layerforge/internal/engine"
	"layerforge/internal/loader"
	"layerforge/internal/sqlitefile"
)

func runBuild(args []string) error {
	flags := flag.NewFlagSet("build", flag.ContinueOnError)
	manifest := flags.String("manifest", "", "manifest file")
	dbPath := flags.String("db", "", "SQLite checkpoint path")
	reset := flags.Bool("reset", false, "remove the checkpoint before building")
	interruptAfter := flags.Int("interrupt-after", 0, "stop after this many complete rounds")
	latencyMS := flags.Int64("latency-ms", -1, "override interaction latency budget")
	maxRounds := flags.Int("max-rounds", -1, "override declared round count")
	threads := flags.Int("threads", -1, "override worker limit")
	memory := flags.Int64("memory-bytes", -1, "override per-thread memory budget")
	buffer := flags.Int("buffer", -1, "override per-thread queue capacity")
	asJSON := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *manifest == "" || *dbPath == "" {
		return apperr.New(apperr.SchemaRequired, "build", "flags", "-manifest and -db are required")
	}
	loaded, err := loader.LoadFile(*manifest)
	if err != nil {
		return err
	}
	if *reset {
		if err := os.Remove(*dbPath); err != nil && !os.IsNotExist(err) {
			return apperr.Wrap(apperr.CheckpointIO, "build", *dbPath, err)
		}
	}
	var override *engine.BudgetOverride
	if *latencyMS >= 0 || *maxRounds >= 0 || *threads >= 0 || *memory >= 0 || *buffer >= 0 {
		budget := loaded.Project.Budget
		if *latencyMS >= 0 {
			budget.Latency = time.Duration(*latencyMS) * time.Millisecond
		}
		if *maxRounds >= 0 {
			budget.MaxRounds = *maxRounds
		}
		if *threads >= 0 {
			budget.ThreadLimit = *threads
		}
		if *memory >= 0 {
			budget.MemoryPerThread = *memory
		}
		if *buffer >= 0 {
			budget.BufferPerThread = *buffer
		}
		override = &engine.BudgetOverride{
			Latency:         budget.Latency,
			MaxRounds:       budget.MaxRounds,
			ThreadLimit:     budget.ThreadLimit,
			MemoryPerThread: budget.MemoryPerThread,
			BufferPerThread: budget.BufferPerThread,
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	eng, err := engine.New(loaded.Project, checkpoint.Open(*dbPath))
	if err != nil {
		return err
	}
	result, buildErr := eng.Build(ctx, engine.BuildOptions{
		Budget:              override,
		InterruptAfterRound: *interruptAfter,
		CommitHook:          commitFaultHookFromEnv(),
	})
	if *asJSON {
		if err := writeJSON(result); err != nil {
			return err
		}
	} else {
		printBuildResult(result, *dbPath)
	}
	if buildErr != nil {
		return buildErr
	}
	return nil
}

func commitFaultHookFromEnv() func(round int, stage sqlitefile.CommitStage) error {
	stage := os.Getenv("LAYERFORGE_FAULT_STAGE")
	atRound := 0
	if value := os.Getenv("LAYERFORGE_FAULT_ROUND"); value != "" {
		if _, err := fmt.Sscanf(value, "%d", &atRound); err != nil {
			return nil
		}
	}
	if stage == "" || atRound <= 0 {
		return nil
	}
	return func(round int, current sqlitefile.CommitStage) error {
		if round == atRound && string(current) == stage {
			return fmt.Errorf("injected commit fault at round %d stage %s", round, current)
		}
		return nil
	}
}

func printBuildResult(result engine.BuildResult, dbPath string) {
	fmt.Fprintf(os.Stdout, "project=%s hash=%s hits=%d builds=%d rounds=%d\n",
		result.ProjectID, result.ProjectHash, result.HitCount, result.BuildCount, len(result.Rounds))
	if result.Recovery.Reset {
		fmt.Fprintf(os.Stdout, "recovery=reset code=%s reason=%s\n", result.Recovery.Code, result.Recovery.Reason)
	}
	for _, round := range result.Rounds {
		fmt.Fprintf(os.Stdout, "round %d built=%v latency_ms=%d/%d remaining_ms=%d workers=%d/%d memory_peak=%d\n",
			round.Round, layerIDs(round.Built), round.LatencyUsedMS, round.LatencyBudgetMS,
			round.LatencyRemainingMS, round.WorkersUsed, round.WorkerLimit, round.PeakMemoryBytes)
		for _, deferred := range round.Deferred {
			fmt.Fprintf(os.Stdout, "  deferred step=%d layer=%s rank=%d reason=%s code=%s\n",
				deferred.Step, deferred.LayerID, deferred.Rank, deferred.Reason, deferred.Code)
		}
	}
	for _, degradation := range result.Degradations {
		fmt.Fprintf(os.Stdout, "degraded code=%s strategy=%s impact=%v reason=%s\n",
			degradation.Code, degradation.Strategy, degradation.ImpactLayers, degradation.Reason)
	}
	for _, decision := range result.Decisions {
		hit := "miss"
		if decision.Hit {
			hit = "hit"
		}
		fmt.Fprintf(os.Stdout, "step %d %-8s %-4s key=%s digest=%s source=%s\n",
			decision.Step, decision.Name, hit, decision.CacheKey, decision.Digest, decision.Source)
	}
	fmt.Fprintf(os.Stdout, "checkpoint=%s codes=%v\n", dbPath, result.Codes)
}

func layerIDs(layers []engine.BuiltLayer) []string {
	out := make([]string, 0, len(layers))
	for _, layer := range layers {
		out = append(out, layer.LayerID)
	}
	return out
}
