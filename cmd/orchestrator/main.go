package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"orchestrator"
)

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: orchestrator <gen|run|verify> [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "gen":
		fs := flag.NewFlagSet("gen", flag.ExitOnError)
		dir := fs.String("dir", "samples", "output directory for the project manifest")
		seed := fs.Int64("seed", orchestrator.DefaultSeed, "deterministic seed")
		n := fs.Int("n", 6, "number of dialogues")
		fs.Parse(os.Args[2:])
		p, oerr := orchestrator.Generate(*seed, *n)
		if oerr != nil {
			fatal(oerr)
		}
		if err := orchestrator.SaveProject(*dir, p); err != nil {
			fatal(err)
		}
		fmt.Printf("generated %d dialogues into %s (seed=%d)\n", *n, *dir, *seed)

	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		dir := fs.String("dir", "samples", "project manifest directory")
		db := fs.String("db", "tmp/orchestrator.db", "SQLite checkpoint database path")
		workers := fs.Int("workers", 1, "max parallel workers (1 = serial)")
		fs.Parse(os.Args[2:])
		p, err := orchestrator.LoadProject(*dir)
		if err != nil {
			fatal(err)
		}
		os.MkdirAll("tmp", 0o755)
		store, err := orchestrator.OpenCheckpointStore(*db)
		if err != nil {
			fatal(err)
		}
		defer store.Close()
		var results map[string]*orchestrator.DialogueResult
		var runErr *orchestrator.Error
		if *workers > 1 {
			if err := store.Invalidate(); err != nil {
				fatal(err)
			}
			if err := store.SaveMeta(p.ContentHash()); err != nil {
				fatal(err)
			}
			results, _, runErr = orchestrator.OrchestrateParallel(p, orchestrator.ResourceLimits{MaxWorkers: *workers})
			for _, id := range p.Policy.QueueOrder {
				if r, ok := results[id]; ok {
					if err := store.WriteResult(r); err != nil {
						fatal(err)
					}
				}
			}
		} else {
			results, runErr = orchestrator.RunLive(p, store)
		}
		if runErr != nil {
			fmt.Println("orchestration finished with error:", runErr)
		}
		out, _ := json.MarshalIndent(results, "", "  ")
		fmt.Println(string(out))

	case "verify":
		fs := flag.NewFlagSet("verify", flag.ExitOnError)
		dir := fs.String("dir", "samples", "project manifest directory")
		db := fs.String("db", "tmp/orchestrator.db", "SQLite checkpoint database path")
		fs.Parse(os.Args[2:])
		p, err := orchestrator.LoadProject(*dir)
		if err != nil {
			fatal(err)
		}
		store, err := orchestrator.OpenCheckpointStore(*db)
		if err != nil {
			fatal(err)
		}
		defer store.Close()
		if v := orchestrator.VerifyFromStore(p, store); v != nil {
			fmt.Println("VERIFY FAILED:", v)
			os.Exit(1)
		}
		fmt.Println("VERIFY OK")

	default:
		fmt.Println("unknown subcommand:", os.Args[1])
		os.Exit(2)
	}
}
