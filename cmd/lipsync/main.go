package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	lipsync "lipsync"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "demo":
		runDemo(os.Args[2:])
	case "serve":
		serve(os.Args[2:])
	case "verify":
		verify(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `lipsync - deterministic subtitle/lip calibration component

Commands:
  demo    generate fixed-seed projects, truth, SQLite checkpoints and diagnostic page
  serve   serve a generated diagnostic directory on localhost
  verify  load generated projects/truths and compare all deterministic paths

Examples:
  go run ./cmd/lipsync demo -out artifacts/demo
  go test ./...
  go run ./cmd/lipsync serve -dir artifacts/demo -addr 127.0.0.1:8099
`)
}

func runDemo(args []string) {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	out := fs.String("out", filepath.Join("artifacts", "demo"), "output directory")
	seed := fs.Int64("seed", 42, "fixed generator seed")
	count := fs.Int("dialogues", 6, "number of generated dialogues")
	workers := fs.Int("workers", lipsync.DefaultMaxWorkers, "maximum scheduling workers")
	buffer := fs.Int("buffer", lipsync.DefaultFrameBufferLimit, "max in-flight frames per worker")
	window := fs.Int("window", lipsync.DefaultWindowSize, "frames per checkpoint window")
	_ = fs.Parse(args)
	report, err := lipsync.RunDemo(context.Background(), lipsync.DemoOptions{
		OutDir:        *out,
		Seed:          *seed,
		DialogueCount: *count,
		WindowSize:    *window,
		MaxWorkers:    *workers,
		FrameBuffer:   *buffer,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("generated %d dialogues / %d frames in %s\n", report.Summary.Dialogues, report.Summary.Frames, *out)
	fmt.Printf("three paths equal=%t truth equal=%t injections=%d/%d\n",
		report.Summary.AllPathsEqual, report.Summary.AllTruthEqual,
		report.Summary.InjectedMatched, report.Summary.InjectedFailures)
	fmt.Printf("diagnostic page: %s\n", filepath.Join(*out, "index.html"))
	if !report.Summary.AllPathsEqual || !report.Summary.AllTruthEqual {
		os.Exit(1)
	}
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dir := fs.String("dir", filepath.Join("artifacts", "demo"), "generated artifact directory")
	addr := fs.String("addr", "127.0.0.1:8099", "local listen address")
	_ = fs.Parse(args)
	info, err := os.Stat(*dir)
	if err != nil || !info.IsDir() {
		log.Fatalf("artifact directory is not available: %s", *dir)
	}
	index := filepath.Join(*dir, "index.html")
	if _, err := os.Stat(index); err != nil {
		log.Fatalf("missing index.html; run demo first: %v", err)
	}
	fileServer := http.FileServer(http.Dir(*dir))
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/" || r.URL.Path == "" {
			http.ServeFile(w, r, index)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
	server := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
	fmt.Printf("serving %s at http://%s/\n", *dir, *addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func verify(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	dir := fs.String("dir", filepath.Join("artifacts", "demo"), "generated artifact directory")
	_ = fs.Parse(args)
	projects, err := lipsync.LoadProjectsFile(filepath.Join(*dir, "projects.json"))
	if err != nil {
		log.Fatal(err)
	}
	truths, err := lipsync.LoadTruthsFile(filepath.Join(*dir, "truths.json"))
	if err != nil {
		log.Fatal(err)
	}
	if len(projects) != len(truths) {
		log.Fatalf("project/truth count mismatch: %d/%d", len(projects), len(truths))
	}
	failed := false
	for i, project := range projects {
		comparisons, err := lipsync.VerifyTruth(project, truths[i])
		if err != nil {
			log.Fatal(err)
		}
		consistent := true
		for _, comparison := range comparisons {
			if !comparison.Consistent {
				consistent = false
				break
			}
		}
		if !consistent {
			failed = true
			fmt.Printf("FAIL %s: %v\n", project.DialogueID, comparisons[0].Mismatch)
		} else {
			fmt.Printf("PASS %s frames=%d transitions=%d\n", project.DialogueID, truths[i].FrameCount, len(truths[i].Transitions))
		}
	}
	if failed {
		os.Exit(1)
	}
}
