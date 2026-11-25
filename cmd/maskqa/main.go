package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"maskqa/internal/checkpoint"
	"maskqa/internal/runner"
	"maskqa/internal/sample"
	"maskqa/internal/server"
	"maskqa/internal/verify"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "generate":
		generateCmd(os.Args[2:])
	case "verify":
		verifyCmd(os.Args[2:])
	case "serve":
		serveCmd(os.Args[2:])
	case "force-kill-demo":
		forceKillCmd(os.Args[2:])
	case "resume-demo":
		resumeDemoCmd(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println(`maskqa commands:
  generate [--data data] [--seed 20261126]
  verify   [--data data] [--artifacts artifacts] [--seed 20261126] [--parallel 4]
  serve    [--data data] [--addr 127.0.0.1:18080] [--seed 20261126]
  force-kill-demo [--data data] [--project glyph-48] [--after 7]
  resume-demo     [--data data] [--project glyph-48]`)
}

func generateCmd(args []string) {
	fs := flag.NewFlagSet("generate", flag.ExitOnError)
	data := fs.String("data", "data", "local data directory")
	seed := fs.Int64("seed", 20261126, "deterministic seed")
	_ = fs.Parse(args)
	assets := sample.GenerateAll(*seed)
	manifest, err := sample.WriteWorkspace(*data, *seed, assets)
	must(err)
	fmt.Printf("generated %d projects in %s (seed=%d)\n", len(manifest.Projects), *data, *seed)
}

func verifyCmd(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	data := fs.String("data", "data", "local data directory")
	artifactsDir := fs.String("artifacts", "artifacts", "artifact report directory")
	seed := fs.Int64("seed", 20261126, "deterministic seed")
	parallel := fs.Int("parallel", 4, "max worker parallelism")
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	report, err := verify.RunAll(ctx, *data, *seed, *parallel)
	must(err)
	must(report.WriteJSON(filepath.Join(*data, "verification-report.json")))
	must(report.WriteJSON(filepath.Join(*artifactsDir, "verification-report.json")))
	must(os.WriteFile(filepath.Join(*artifactsDir, "verification-report.md"), []byte(report.Markdown()), 0o644))
	fmt.Print(report.Markdown())
	if !report.OK {
		os.Exit(1)
	}
}

func serveCmd(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	data := fs.String("data", "data", "local data directory")
	addr := fs.String("addr", "127.0.0.1:18080", "listen address")
	seed := fs.Int64("seed", 20261126, "deterministic seed if data is absent")
	_ = fs.Parse(args)

	if _, err := os.Stat(filepath.Join(*data, "manifest.json")); err != nil {
		assets := sample.GenerateAll(*seed)
		_, err := sample.WriteWorkspace(*data, *seed, assets)
		must(err)
	}
	ln, err := net.Listen("tcp", *addr)
	must(err)
	srv := server.New(*data)
	fmt.Printf("maskqa diagnostic UI: http://%s/ (data=%s)\n", *addr, *data)
	must(http.Serve(ln, srv.Handler()))
}

func forceKillCmd(args []string) {
	fs := flag.NewFlagSet("force-kill-demo", flag.ExitOnError)
	data := fs.String("data", "data", "local data directory")
	projectID := fs.String("project", "glyph-48", "project id")
	after := fs.Int("after", 7, "committed strokes before hard exit")
	_ = fs.Parse(args)
	p, err := sample.LoadProject(filepath.Join(*data, "samples", *projectID, "project.internal.json"))
	must(err)
	dbPath := filepath.Join(*data, "checkpoints", "force-kill-demo.db")
	_ = os.Remove(dbPath)
	_ = os.Remove(dbPath + "-wal")
	_ = os.Remove(dbPath + "-shm")
	store, err := checkpoint.Open(dbPath)
	must(err)
	cfg := runner.DefaultConfig("force-kill-demo")
	cfg.KillAfterCommits = *after
	_, err = runner.Run(context.Background(), p, store, cfg, nil)
	if err != runner.ErrForceTerminated {
		must(err)
	}
	// Intentionally do not call store.Close or run deferred report writing:
	// this mirrors SIGKILL immediately after a durable stroke transaction.
	fmt.Printf("forced boundary after %d durable commits; exiting 137 without report\n", *after)
	os.Exit(137)
}

func resumeDemoCmd(args []string) {
	fs := flag.NewFlagSet("resume-demo", flag.ExitOnError)
	data := fs.String("data", "data", "local data directory")
	projectID := fs.String("project", "glyph-48", "project id")
	_ = fs.Parse(args)
	p, err := sample.LoadProject(filepath.Join(*data, "samples", *projectID, "project.internal.json"))
	must(err)
	dbPath := filepath.Join(*data, "checkpoints", "force-kill-demo.db")
	store, err := checkpoint.Open(dbPath)
	must(err)
	defer store.Close()
	cfg := runner.DefaultConfig("resume-demo")
	cfg.Mode = "force-kill-resume"
	result, err := runner.Run(context.Background(), p, store, cfg, nil)
	must(err)
	fmt.Printf("resumed and verified %s: commits=%d domains=%d final_mask_bytes=%d\n", p.ID, len(result.CommitSequence), result.Topology.DomainCount, len(result.FinalMask.Pix))
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
