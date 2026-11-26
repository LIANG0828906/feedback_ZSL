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

	"quota-cluster/internal/diagnostic"
	"quota-cluster/internal/sample"
	"quota-cluster/internal/workbench"
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
	case "verify":
		err = runVerify(os.Args[2:])
	case "serve":
		err = runServe(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`quota-workbench commands:
  generate [--seed 42] [--out samples]
  verify   [--seed 42] [--report artifacts/verification-report.json] [--checkpoint artifacts/checkpoint.sqlite]
  serve    [--addr 127.0.0.1:18765] [--seed 42]`)
}

func runGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	seed := fs.Int64("seed", 42, "deterministic seed")
	out := fs.String("out", "samples", "output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	bundle, err := sample.Generate(*seed)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	files := map[string][]byte{
		"project-v1.json":     bundle.V1,
		"project-v2.json":     bundle.V2,
		"project-v3.json":     bundle.V3,
		"truth.json":          bundle.TruthJSON,
		"critical-cases.json": bundle.CriticalJSON,
	}
	for name, data := range files {
		path := filepath.Join(*out, name)
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			return err
		}
		fmt.Println("wrote", path)
	}
	return nil
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	seed := fs.Int64("seed", 42, "deterministic seed")
	reportPath := fs.String("report", "artifacts/verification-report.json", "JSON report output path")
	checkpointPath := fs.String("checkpoint", "artifacts/checkpoint.sqlite", "SQLite checkpoint path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	report, _, err := diagnostic.Verify(*seed, *checkpointPath)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if *reportPath != "" {
		if err := os.MkdirAll(filepath.Dir(*reportPath), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(*reportPath, append(data, '\n'), 0o644); err != nil {
			return err
		}
		fmt.Println("wrote", *reportPath)
	}
	fmt.Println(string(data))
	if !report.OK {
		return errors.New("one or more verification checks failed")
	}
	return nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:18765", "listen address")
	seed := fs.Int64("seed", 42, "deterministic seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	server, err := workbench.New(*seed)
	if err != nil {
		return err
	}
	httpServer := &http.Server{Addr: *addr, Handler: server.Handler(), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		fmt.Printf("quota workbench listening on http://%s (seed=%d)\n", *addr, *seed)
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
