package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"settlement/internal/tasks"
)

func main() {
	outDir := flag.String("out", "data", "output directory")
	seed := flag.Int64("seed", tasks.DefaultSeed, "deterministic generator seed")
	batchCount := flag.Int("batches", 20, "number of batches")
	tasksPerBatch := flag.Int("tasks-per-batch", 100, "tasks per batch")
	flag.Parse()

	if *batchCount <= 0 || *tasksPerBatch <= 0 {
		log.Fatal("batches and tasks-per-batch must be positive")
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatal(err)
	}
	batches := tasks.Generate(*seed, *batchCount, *tasksPerBatch)
	tasksPath := filepath.Join(*outDir, "tasks.jsonl")
	manifestPath := filepath.Join(*outDir, "manifest.json")
	if err := tasks.WriteJSONL(tasksPath, batches); err != nil {
		log.Fatal(err)
	}
	if err := tasks.WriteManifest(manifestPath, *seed, batches); err != nil {
		log.Fatal(err)
	}
	manifest, err := json.MarshalIndent(map[string]any{
		"tasks_file":      tasksPath,
		"manifest_file":   manifestPath,
		"seed":            *seed,
		"batch_count":     *batchCount,
		"tasks_per_batch": *tasksPerBatch,
		"task_count":      *batchCount * *tasksPerBatch,
	}, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(manifest))
}
