package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"terrain"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18080", "local diagnostic listen address")
	sample := flag.String("sample", "samples/terrain-seed-42.json", "deterministic sample JSON")
	db := flag.String("db", "artifacts/terrain-checkpoint.db", "SQLite checkpoint path")
	generate := flag.Bool("generate", true, "generate sample if it does not exist")
	flag.Parse()

	if *generate {
		if _, err := os.Stat(*sample); os.IsNotExist(err) {
			project := terrain.GenerateSample(42)
			if err := terrain.WriteProject(*sample, project); err != nil {
				log.Fatalf("generate sample: %v", err)
			}
		}
	}
	project, err := terrain.LoadProject(*sample)
	if err != nil {
		log.Fatalf("load project: %v", err)
	}
	if dir := filepath.Dir(*db); dir != "." && dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	svc, err := terrain.NewService(project, terrain.ServiceOptions{DBPath: *db})
	if err != nil {
		log.Fatalf("open checkpoint service: %v", err)
	}
	defer svc.Close()
	if _, err := svc.RunIncremental(context.Background(), nil); err != nil {
		log.Printf("initial incremental run completed with block failures: %v", err)
	}

	log.Printf("terrain diagnostics: http://%s", *addr)
	server := &http.Server{Addr: *addr, Handler: terrain.NewHandler(svc)}
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
