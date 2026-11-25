package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"combatjudge/internal/sample"
	"combatjudge/internal/web"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8099", "diagnostic page listen address")
	work := flag.String("work", ".diagnostic", "directory for SQLite checkpoints and dashboard.json")
	samplesDir := flag.String("samples", "samples", "directory for generated deterministic samples")
	once := flag.Bool("once", false, "run all paths, write dashboard.json and exit without serving HTTP")
	flag.Parse()

	if _, err := sample.GenerateDirectory(*samplesDir); err != nil {
		panic(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dash, err := web.BuildDashboard(ctx, *work)
	if err != nil {
		panic(err)
	}
	payload, err := json.MarshalIndent(dash, "", "  ")
	if err != nil {
		panic(err)
	}
	if err := os.MkdirAll(*work, 0o755); err != nil {
		panic(err)
	}
	jsonPath := filepath.Join(*work, "dashboard.json")
	if err := os.WriteFile(jsonPath, payload, 0o644); err != nil {
		panic(err)
	}

	fmt.Printf("samples ready in %s\n", *samplesDir)
	fmt.Printf("dashboard data written to %s\n", jsonPath)
	fmt.Printf("compare: live=%v resume=%v incrementalMatch=%v\n", dash.LiveMismatch == nil, dash.ResumeMismatch == nil, dash.IncrementalMatch)
	if *once {
		if dash.LiveMismatch != nil || dash.ResumeMismatch != nil || !dash.IncrementalMatch {
			os.Exit(2)
		}
		return
	}

	srv := &http.Server{Addr: *addr, Handler: web.NewHandler(dash), ReadHeaderTimeout: 5 * time.Second}
	fmt.Printf("diagnostic page: http://%s/\n", *addr)
	if err := srv.ListenAndServe(); err != nil {
		panic(err)
	}
}
