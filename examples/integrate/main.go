// Command integrate demonstrates embedding the economy kernel.
//
//	go run ./examples/integrate \
//	  --project artifacts/demo/samples/v2/project.json \
//	  --db artifacts/integrate.db
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"economycore/checkpoint"
	"economycore/engine"
	"economycore/format"
	"economycore/runner"
)

func main() {
	project := flag.String("project", "artifacts/demo/samples/v2/project.json", "project document")
	dbPath := flag.String("db", "artifacts/integrate.db", "SQLite checkpoint path")
	flag.Parse()
	if err := run(context.Background(), *project, *dbPath); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, project, dbPath string) error {
	m, err := format.LoadFile(project)
	if err != nil {
		return err
	}

	// Full settlement populates the node-cycle cache.
	eng, err := engine.New(m, engine.Options{})
	if err != nil {
		return err
	}
	if _, err := eng.RunFull(ctx); err != nil {
		return err
	}

	// A local what-if edit only invalidates the edited node and downstream.
	edit, err := eng.EditRate("forest", 15000)
	if err != nil {
		return err
	}
	incremental, err := eng.RunIncremental(ctx)
	if err != nil {
		return err
	}
	fresh, err := engine.New(eng.Model(), engine.Options{})
	if err != nil {
		return err
	}
	full, err := fresh.RunFull(ctx)
	if err != nil {
		return err
	}
	if err := engine.CompareRuns(incremental, full); err != nil {
		return err
	}

	// Persisted windows are committed atomically and can resume after restart.
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	r, err := runner.New(m, store, runner.Options{})
	if err != nil {
		return err
	}
	report, err := r.RunAll(ctx)
	if err != nil {
		return err
	}

	summary := map[string]any{
		"version":                 m.SourceVersion,
		"incrementalInvalidated":  edit.Invalidated,
		"incrementalUnaffected":   edit.Unaffected,
		"incrementalReusedCycles": incremental.ReusedCycles,
		"complete":                report.Complete,
		"committedWindows":        report.CommittedWindows,
		"cycles":                  len(report.Cycles),
	}
	data, _ := json.MarshalIndent(summary, "", "  ")
	fmt.Println(string(data))
	return nil
}
