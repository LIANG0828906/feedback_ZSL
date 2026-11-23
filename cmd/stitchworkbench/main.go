package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"stitchworkbench/internal/samplegen"
	"stitchworkbench/internal/server"
	"stitchworkbench/internal/session"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "local listen address")
	dataDir := flag.String("data", "data/samples", "deterministic sample directory")
	workDir := flag.String("work", "data/workbench", "checkpoint, snapshot and artifact work directory")
	seed := flag.Int64("seed", 42, "sample seed when no samples exist")
	sets := flag.Int("sets", 5, "sample sets to generate when no samples exist")
	noGenerate := flag.Bool("no-generate", false, "do not generate samples automatically")
	flag.Parse()
	if _, err := os.Stat(*dataDir); os.IsNotExist(err) && !*noGenerate {
		if err := samplegen.Generate(samplegen.Config{OutDir: *dataDir, Seed: *seed, Sets: *sets}); err != nil {
			fmt.Fprintln(os.Stderr, "sample generation failed:", err)
			os.Exit(1)
		}
	}
	svc, err := session.NewService(*dataDir, *workDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	srv := server.New(svc)
	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           srv,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	fmt.Printf("stitch workbench listening on http://%s\n", *addr)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
