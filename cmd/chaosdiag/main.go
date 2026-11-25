package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"chaosdiag/checkpoint"
	"chaosdiag/engine"
	"chaosdiag/gen"
	"chaosdiag/incremental"
	"chaosdiag/loader"
	"chaosdiag/model"
	"chaosdiag/reconcile"
	"chaosdiag/workbench"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "verify":
		err = runVerify()
	case "samples":
		err = runSamples(os.Args[2:])
	case "serve":
		err = runServe(os.Args[2:])
	case "demo":
		err = runDemo(os.Args[2:])
	case "anomaly":
		err = runAnomaly(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		var ce *model.Error
		if errors.As(err, &ce) {
			fmt.Fprintf(os.Stderr, "%s %s: %s\n", ce.Code, ce.Field, ce.Message)
			if ce.Context != nil {
				raw, _ := json.MarshalIndent(ce.Context, "", "  ")
				fmt.Fprintln(os.Stderr, string(raw))
			}
		} else {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`chaosdiag commands:
  verify                     run deterministic acceptance checks
  samples [--dir DIR]        write generated v1/v2/v3 JSON and truth JSON
  serve [--addr ADDR]        start local diagnostic workbench
  demo [--version v3] [--stop 10] [--checkpoint PATH]
  anomaly --type TYPE        print stable business code for an injected anomaly

anomaly types: cycle_dependency, missing_service_target, negative_delay,
zero_timeout_budget, concurrency_limit_out_of_range,
truncated_checkpoint, version_mismatch_snapshot`)
}

func runVerify() error {
	bundle, err := gen.Generate()
	if err != nil {
		return err
	}
	fmt.Printf("seed=%d project=%s version=%s\n", bundle.Seed, bundle.Canonical.ProjectID, bundle.Canonical.ProjectVersion)
	for _, version := range []string{model.SchemaV1, model.SchemaV2, model.SchemaV3} {
		p, err := loader.Load(bundle.Docs[version])
		if err != nil {
			return fmt.Errorf("load %s: %w", version, err)
		}
		report := engine.Run(p)
		verdicts := reconcile.ReconcileAll(p, report.Results)
		matches := 0
		for _, rr := range report.Results {
			truth := findTruth(bundle, rr.RequestID)
			if truth != nil && equalJSON(rr, *truth) {
				matches++
			}
		}
		fmt.Printf("%s: services=%d edges=%d instructions=%d requests=%d truth_hop_match=%d/%d verdicts=%d\n",
			short(version), len(p.Services), len(p.Edges), len(p.Instructions), len(p.Requests),
			matches, len(bundle.Truth.Results), len(verdicts))
	}

	eng, err := incremental.NewEngine(bundle.Canonical)
	if err != nil {
		return err
	}
	delay := int64(100)
	inc, err := eng.Apply(incremental.Mutation{Kind: "instruction_amplitude", InstructionID: "I-DELAY-INV", DelayMS: &delay})
	if err != nil {
		return err
	}
	full := incremental.FullReport(eng.Project())
	if !equalJSON(inc.Report.Results, full.Results) {
		return fmt.Errorf("incremental report mismatch")
	}
	fmt.Printf("incremental: recomputed=%v reused=%d closure_nodes=%d full_recompute=match\n",
		inc.Recomputed, len(inc.Reused), len(inc.Closure.Nodes))

	path := filepath.Join("artifacts", "checkpoints", "verify.sqlite")
	_ = os.Remove(path)
	store, err := checkpoint.OpenStore(path)
	if err != nil {
		return err
	}
	ctx := context.Background()
	stopped, err := checkpoint.RunFresh(ctx, bundle.Canonical, store, 10)
	if err != nil {
		return err
	}
	cp, err := store.Load()
	if err != nil {
		return err
	}
	if err := store.Close(); err != nil {
		return err
	}
	expected, err := checkpoint.LoadExpected(path, bundle.Canonical.ProjectVersion)
	if err != nil {
		return err
	}
	store2, err := checkpoint.OpenStore(path)
	if err != nil {
		return err
	}
	resumed, err := checkpoint.Resume(ctx, bundle.Canonical, store2, *expected, 0)
	if err != nil {
		return err
	}
	_ = store2.Close()
	fullReport := engine.Run(bundle.Canonical)
	if !equalJSON(resumed.Report.Results, fullReport.Results) {
		return fmt.Errorf("checkpoint resume mismatch")
	}
	fmt.Printf("checkpoint: path=%s stop_tick=%d persisted_queue=%d resumed_tick=%d results=match\n",
		path, stopped.LastTick, len(cp.Queue), resumed.LastTick)
	fmt.Println("VERIFY OK")
	return nil
}

func runSamples(args []string) error {
	fs := flag.NewFlagSet("samples", flag.ContinueOnError)
	dir := fs.String("dir", filepath.Join("artifacts", "samples"), "output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	bundle, err := gen.Generate()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}
	files := map[string][]byte{
		"project-v1.json": bundle.Docs[model.SchemaV1],
		"project-v2.json": bundle.Docs[model.SchemaV2],
		"project-v3.json": bundle.Docs[model.SchemaV3],
	}
	truth, err := json.MarshalIndent(bundle.Truth, "", "  ")
	if err != nil {
		return err
	}
	files["truth.json"] = truth
	for name, content := range files {
		path := filepath.Join(*dir, name)
		if err := os.WriteFile(path, content, 0o644); err != nil {
			return err
		}
		fmt.Println(path)
	}
	return nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8088", "listen address")
	cpPath := fs.String("checkpoint-dir", filepath.Join("artifacts", "checkpoints"), "checkpoint directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	server, err := workbench.NewServer(*cpPath)
	if err != nil {
		return err
	}
	httpServer := &http.Server{Addr: *addr, Handler: server.Handler(), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	fmt.Printf("ChaosDiag workbench: http://%s\n", *addr)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func runDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	version := fs.String("version", "v3", "schema version v1/v2/v3")
	cpPath := fs.String("checkpoint", filepath.Join("artifacts", "checkpoints", "demo.sqlite"), "SQLite checkpoint path")
	stop := fs.Int("stop", 0, "stop after this complete logical tick")
	pauseMS := fs.Int("tick-pause-ms", 0, "optional pause between ticks to demonstrate signal-safe interruption")
	if err := fs.Parse(args); err != nil {
		return err
	}
	bundle, err := gen.Generate()
	if err != nil {
		return err
	}
	v := workbench.NormalizeVersion(*version)
	p, err := loader.Load(bundle.Docs[v])
	if err != nil {
		return err
	}
	store, err := checkpoint.OpenStore(*cpPath)
	if err != nil {
		return err
	}
	defer store.Close()
	ctx, stopSignal := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignal()
	out, runErr := checkpoint.RunFreshWithOptions(ctx, p, store, checkpoint.RunOptions{
		StopAfterTick: *stop, TickPause: time.Duration(*pauseMS) * time.Millisecond,
	})
	if runErr != nil && errors.Is(runErr, context.Canceled) {
		fmt.Printf("signal received; last complete checkpoint tick=%d path=%s\n", out.LastTick, *cpPath)
		return nil
	}
	if runErr != nil {
		return runErr
	}
	for _, rr := range out.Report.Results {
		fmt.Printf("%-22s tick=%-2d %-9s total=%4dms hits=%v\n", rr.RequestID, rr.Tick, rr.Status, rr.TotalMS, rr.HitInstructionIDs)
	}
	fmt.Printf("checkpoint=%s last_tick=%d stopped_early=%v\n", *cpPath, out.LastTick, out.StoppedEarly)
	return nil
}

func runAnomaly(args []string) error {
	fs := flag.NewFlagSet("anomaly", flag.ContinueOnError)
	anomalyType := fs.String("type", "", "anomaly type")
	cpDir := fs.String("checkpoint-dir", filepath.Join("artifacts", "checkpoints"), "checkpoint directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	svc, err := workbench.NewService(*cpDir)
	if err != nil {
		return err
	}
	resp := svc.Anomaly(workbench.AnomalyRequest{Type: *anomalyType})
	raw, _ := json.MarshalIndent(resp, "", "  ")
	fmt.Println(string(raw))
	if !resp.OK {
		return nil
	}
	return nil
}

func findTruth(b *gen.Bundle, id string) *model.RequestResult {
	for i := range b.Truth.Results {
		if b.Truth.Results[i].RequestID == id {
			return &b.Truth.Results[i]
		}
	}
	return nil
}

func equalJSON(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

func short(v string) string {
	if len(v) >= 2 {
		return v[len(v)-2:]
	}
	return v
}
