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

	"tunnelworkbench/internal/checkpoint"
	"tunnelworkbench/internal/forward"
	"tunnelworkbench/internal/verify"
	"tunnelworkbench/internal/workbench"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	var err error
	switch cmd {
	case "verify", "demo":
		err = runVerify(args)
	case "gen":
		err = runGen(args)
	case "inject":
		err = runInject(args)
	case "serve":
		err = runServe(args)
	case "run-checkpoint":
		err = runCheckpoint(args)
	case "repl", "shell":
		err = runRepl(args)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		var biz *forward.BizError
		if errors.As(err, &biz) {
			fmt.Fprintf(os.Stderr, "%s at %s: %s\n", biz.Code, biz.Path, biz.Message)
			if biz.Context != nil {
				raw, _ := json.MarshalIndent(biz.Context, "", "  ")
				fmt.Fprintln(os.Stderr, string(raw))
			}
		} else {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`tunnelbench commands:
  verify [--workdir artifacts/verify]   Run all local acceptance checks
  gen [--out artifacts/sample]          Write three schema docs, events and truth
  inject --kind CODE                    Inject one deterministic anomaly and print E_xxx
  serve [--addr 127.0.0.1:8088]         Start interactive diagnostic workbench
  repl [--schema v2]                    Start terminal diagnostic shell
  run-checkpoint --db PATH [--schema v2] Run event stream with signal-safe SQLite commits`)
}

func runInject(args []string) error {
	fs := flag.NewFlagSet("inject", flag.ContinueOnError)
	kind := fs.String("kind", "", "anomaly kind")
	workDir := fs.String("workdir", filepath.Join("artifacts", "workbench"), "local state directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *kind == "" {
		return forward.NewBizError(forward.CodeFieldRequired, "kind", "anomaly kind is required")
	}
	svc, err := workbench.NewService(*workDir)
	if err != nil {
		return err
	}
	res := svc.Inject(*kind, nil)
	raw, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(raw))
	return nil
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	workDir := fs.String("workdir", filepath.Join("artifacts", "verify"), "verification workspace")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := os.MkdirAll(*workDir, 0o755); err != nil {
		return err
	}
	report := verify.RunAll(context.Background(), *workDir)
	raw, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(filepath.Join(*workDir, "verification-report.json"), raw, 0o644); err != nil {
		return err
	}
	passed := 0
	for _, c := range report.Checks {
		mark := "PASS"
		if !c.OK {
			mark = "FAIL"
		} else {
			passed++
		}
		fmt.Printf("[%s] %s/%s — %s\n", mark, c.Category, c.Name, c.Detail)
	}
	fmt.Printf("\n%d/%d checks passed; JSON report: %s\n", passed, len(report.Checks), filepath.Join(*workDir, "verification-report.json"))
	if !report.OK {
		return fmt.Errorf("verification failed")
	}
	return nil
}

func runGen(args []string) error {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	out := fs.String("out", filepath.Join("artifacts", "sample"), "output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	sample := forward.GenerateSample(forward.DefaultSeed)
	for schema, raw := range sample.Documents {
		path := filepath.Join(*out, "engineering-"+schema+".json")
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			return err
		}
		fmt.Println("wrote", path)
	}
	writeJSON := func(name string, v any) error {
		raw, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		path := filepath.Join(*out, name)
		return os.WriteFile(path, raw, 0o644)
	}
	if err := writeJSON("events.json", sample.Events); err != nil {
		return err
	}
	if err := writeJSON("truth.json", sample.Truth); err != nil {
		return err
	}
	if err := writeJSON("canonical-project.json", sample.Project); err != nil {
		return err
	}
	fmt.Println("seed", sample.Seed, "event_count", len(sample.Events), "packet_count", sample.Truth.PacketCount)
	return nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8088", "listen address")
	workDir := fs.String("workdir", filepath.Join("artifacts", "workbench"), "local state directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	svc, err := workbench.NewService(*workDir)
	if err != nil {
		return err
	}
	server := workbench.NewServer(svc)
	httpServer := &http.Server{Addr: *addr, Handler: server.Handler(), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		fmt.Printf("tunnel workbench listening on http://%s (Ctrl-C to stop)\n", *addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "server error:", err)
			os.Exit(1)
		}
	}()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return httpServer.Shutdown(shutdownCtx)
}

func runCheckpoint(args []string) error {
	fs := flag.NewFlagSet("run-checkpoint", flag.ContinueOnError)
	db := fs.String("db", filepath.Join("artifacts", "checkpoint.db"), "SQLite checkpoint path")
	schema := fs.String("schema", forward.SchemaV2, "schema version to load")
	workDir := fs.String("workdir", filepath.Join("artifacts", "workbench"), "generator workspace")
	if err := fs.Parse(args); err != nil {
		return err
	}
	svc, err := workbench.NewService(*workDir)
	if err != nil {
		return err
	}
	eng, err := svc.Load(*schema)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, err := checkpoint.Open(ctx, *db)
	if err != nil {
		return err
	}
	defer store.Close()
	result, err := checkpoint.RunAndCheckpoint(ctx, store, eng.Project, eng.Events)
	if err != nil {
		return err
	}
	fmt.Printf("last_committed_seq=%d interrupted=%v db=%s\n", result.LastCommitted, result.Interrupted, *db)
	return nil
}
