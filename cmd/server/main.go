package main

import (
	"context"
	"flag"
	"log"
	"path/filepath"

	"audiofp"
	"audiofp/samplegen"
	httpserver "audiofp/server"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "HTTP listen address")
	dataDir := flag.String("data", "data", "deterministic sample data directory")
	dbPath := flag.String("db", "", "SQLite checkpoint path (default: <data>/checkpoint.db)")
	workers := flag.Int("workers", 4, "maximum extraction worker parallelism")
	flag.Parse()

	material, pcmPath, manifestPath, err := samplegen.WriteDefaultDataset(*dataDir)
	if err != nil {
		log.Fatalf("write deterministic sample: %v", err)
	}
	loaded, err := audiofp.LoadMaterial(pcmPath, manifestPath)
	if err != nil {
		log.Fatalf("load material: %v", err)
	}
	if *dbPath == "" {
		*dbPath = filepath.Join(*dataDir, "server-checkpoint.db")
	}

	limits := audiofp.DefaultResourceLimits()
	if *workers > 0 {
		limits.MaxWorkers = *workers
	}
	srv := httpserver.New(httpserver.Config{
		Material: loaded, Params: audiofp.DefaultParams(), Limits: limits, DBPath: *dbPath,
	})
	if err := srv.InitialAnnotation(context.Background()); err != nil {
		if audiofp.HasCode(err, audiofp.CodeQual) {
			log.Printf("initial quality check reported E_QUAL; UI will show details: %v", err)
		} else {
			log.Fatalf("initial annotation: %v", err)
		}
	}
	log.Printf("material %s hash=%s generated seed=%d", material.PCM.ID, material.ContentHash()[:16], material.PCM.Seed)
	log.Fatal(srv.ListenAndServe(*addr))
}
