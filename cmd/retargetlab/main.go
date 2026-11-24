package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"retargetlab/internal/engine"
	"retargetlab/internal/generator"
	"retargetlab/internal/model"
	"retargetlab/internal/panel"
	"retargetlab/internal/solver"
	"retargetlab/internal/store"
	"retargetlab/internal/verify"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "generate":
		runGenerate(os.Args[2:])
	case "verify":
		runVerify(os.Args[2:])
	case "serve":
		runServe(os.Args[2:])
	case "crash-helper":
		runCrashHelper(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `RetargetLab

Commands:
  generate      Generate a deterministic local sample workspace
  verify        Run the acceptance verification suite
  serve         Start the visual diagnostic panel
  crash-helper  Internal helper used to demonstrate forced-process recovery

Run a command with -h for flags.`)
}

func runGenerate(args []string) {
	fs := flag.NewFlagSet("generate", flag.ExitOnError)
	out := fs.String("out", "data/sample.json", "workspace JSON output")
	seed := fs.Int64("seed", 42, "deterministic sample seed")
	frames := fs.Int("frames", 32, "frame count")
	phase := fs.Int("phase", 8, "frames per action phase")
	_ = fs.Parse(args)
	ws, err := generator.Generate(generator.Options{Seed: *seed, FrameCount: *frames, PhaseSize: *phase})
	if err != nil {
		fail(err)
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fail(err)
	}
	if err := model.SaveWorkspace(*out, ws); err != nil {
		fail(err)
	}
	fmt.Printf("generated seed=%d frames=%d targets=%d source_hash=%s mapping_hash=%s\n",
		ws.Seed, len(ws.Clip.Frames), len(ws.Targets), ws.Source.Hash(), ws.Mapping.Hash())
}

func runVerify(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	sample := fs.String("sample", "data/sample.json", "workspace JSON")
	targetName := fs.String("target", "reference", "target variant")
	_ = fs.Parse(args)
	ws, err := loadOrGenerate(*sample)
	if err != nil {
		fail(err)
	}
	report := verify.Run(context.Background(), ws, *targetName, os.TempDir())
	b, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println(string(b))
	if !report.Passed {
		os.Exit(1)
	}
}

func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	sample := fs.String("sample", "data/sample.json", "workspace JSON")
	targetName := fs.String("target", "reference", "target variant")
	dbPath := fs.String("db", "data/checkpoints.db", "SQLite checkpoint path")
	addr := fs.String("addr", "127.0.0.1:8080", "HTTP listen address")
	workers := fs.Int("workers", 4, "maximum solver workers")
	window := fs.Int("window", 4, "checkpoint window size")
	queue := fs.Int("queue", 8, "bounded task queue per worker")
	buffer := fs.Int("buffer", 2, "per-worker state buffer limit")
	_ = fs.Parse(args)
	ws, err := loadOrGenerate(*sample)
	if err != nil {
		fail(err)
	}
	target, ok := ws.TargetByName(*targetName)
	if !ok {
		fail(model.Err(model.E_PARAM, "serve", fmt.Sprintf("target %q not found", *targetName)))
	}
	truth, _ := ws.TruthByTarget(*targetName)
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o755); err != nil {
		fail(err)
	}
	db, err := store.Open(*dbPath)
	if err != nil {
		fail(err)
	}
	defer db.Close()
	e, err := engine.New(engine.Config{
		Source: ws.Source, Clip: ws.Clip, Target: target, Mapping: ws.Mapping,
		Params: solver.DefaultParams(), Store: db, WindowSize: *window,
		MaxWorkers: *workers, QueuePerWorker: *queue, BufferPerWorker: *buffer,
	})
	if err != nil {
		fail(err)
	}
	report, err := e.Recover(context.Background())
	if err != nil {
		fail(err)
	}
	fmt.Printf("recovery: loaded_frames=%d loaded_windows=%d invalidated=%t", report.LoadedFrames, report.LoadedWindows, report.Invalidated)
	if report.Reason != "" {
		fmt.Printf(" reason=%s", report.Reason)
	}
	fmt.Println()

	var truthPtr *model.TargetTruth
	if truth.Target != "" {
		truthPtr = &truth
	}
	srv := &http.Server{
		Addr: *addr, Handler: panel.New(e, truthPtr).Handler(),
		ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second,
	}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	fmt.Printf("RetargetLab panel: http://%s\n", *addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fail(err)
	}
}

func runCrashHelper(args []string) {
	fs := flag.NewFlagSet("crash-helper", flag.ExitOnError)
	sample := fs.String("sample", "data/sample.json", "workspace JSON")
	targetName := fs.String("target", "reference", "target variant")
	dbPath := fs.String("db", "data/crash.db", "SQLite checkpoint path")
	prefix := fs.Int("prefix", 12, "frames to commit before process exit")
	_ = fs.Parse(args)
	ws, err := loadOrGenerate(*sample)
	if err != nil {
		fail(err)
	}
	target, ok := ws.TargetByName(*targetName)
	if !ok {
		fail(model.Err(model.E_PARAM, "crash-helper", "target not found"))
	}
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o755); err != nil {
		fail(err)
	}
	db, err := store.Open(*dbPath)
	if err != nil {
		fail(err)
	}
	e, err := engine.New(engine.Config{
		Source: ws.Source, Clip: ws.Clip, Target: target, Mapping: ws.Mapping,
		Params: solver.DefaultParams(), Store: db, WindowSize: 4, MaxWorkers: 4,
		QueuePerWorker: 2, BufferPerWorker: 2,
	})
	if err != nil {
		fail(err)
	}
	rec, err := e.Recover(context.Background())
	if err != nil {
		fail(err)
	}
	start := rec.LoadedFrames
	end := start + *prefix - 1
	if end >= len(ws.Clip.Frames) {
		end = len(ws.Clip.Frames) - 1
	}
	if start <= end {
		if _, err := e.SolveRange(context.Background(), start, end); err != nil {
			fail(err)
		}
	}
	// Deliberately skip Close: committed SQLite windows remain durable while
	// uncommitted work disappears with the process.
	os.Exit(23)
}

func loadOrGenerate(path string) (model.Workspace, error) {
	ws, err := model.LoadWorkspace(path)
	if err == nil {
		return ws, nil
	}
	if !os.IsNotExist(unwrapIO(err)) {
		return model.Workspace{}, err
	}
	ws, genErr := generator.Generate(generator.DefaultOptions())
	if genErr != nil {
		return model.Workspace{}, genErr
	}
	if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr != nil {
		return model.Workspace{}, mkErr
	}
	if saveErr := model.SaveWorkspace(path, ws); saveErr != nil {
		return model.Workspace{}, saveErr
	}
	return ws, nil
}

func unwrapIO(err error) error {
	var ce *model.CodedError
	if ok := asCoded(err, &ce); ok && ce.Cause != nil {
		return ce.Cause
	}
	return err
}

func asCoded(err error, target **model.CodedError) bool {
	ce, ok := err.(*model.CodedError)
	if ok {
		*target = ce
	}
	return ok
}

func fail(err error) {
	b, _ := json.Marshal(err)
	if len(b) == 0 || string(b) == "{}" {
		fmt.Fprintln(os.Stderr, err)
	} else {
		fmt.Fprintln(os.Stderr, string(b))
	}
	os.Exit(1)
}
