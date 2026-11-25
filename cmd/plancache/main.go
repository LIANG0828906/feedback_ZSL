package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"plancache"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		if be, ok := plancache.AsBusinessError(err); ok {
			b, _ := json.MarshalIndent(be, "", "  ")
			fmt.Fprintln(os.Stderr, string(b))
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	mode := "report"
	if len(args) > 0 && args[0] != "-" && !startsWithDash(args[0]) {
		mode = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet(mode, flag.ContinueOnError)
	seed := fs.Int64("seed", plancache.DefaultSeed, "deterministic generator seed")
	out := fs.String("out", "", "write JSON report to this file")
	dir := fs.String("dir", "artifacts/samples", "directory for schema sample documents")
	dbPath := fs.String("db", "artifacts/checkpoint/plancache.db", "SQLite checkpoint path")
	crashAt := fs.Int("crash-at", 0, "if non-zero, SIGKILL after pending state for this request sequence")
	workers := fs.Int("workers", 0, "override worker budget")
	buffer := fs.Int("buffer", 0, "override per-session buffer")
	if err := fs.Parse(args); err != nil {
		return err
	}

	p := plancache.GenerateProject(*seed)
	if *workers > 0 {
		p.Runtime.Workers = *workers
	}
	if *buffer > 0 {
		p.Runtime.SessionBuffer = *buffer
	}

	switch mode {
	case "samples":
		return writeSamples(p, *dir)
	case "checkpoint":
		return runCheckpoint(p, *dbPath, *crashAt)
	case "recover":
		return runRecover(p, *dbPath, *out)
	case "report":
		report, _, err := plancache.RunParallelPipeline(p)
		if err != nil {
			return err
		}
		return writeJSON(*out, report)
	default:
		return fmt.Errorf("unknown mode %q (want report, samples, checkpoint, recover)", mode)
	}
}

func startsWithDash(s string) bool { return len(s) > 0 && s[0] == '-' }

func writeSamples(p *plancache.Project, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, version := range []string{plancache.SchemaV1, plancache.SchemaV2, plancache.SchemaV3} {
		b, err := plancache.Render(p, version)
		if err != nil {
			return err
		}
		name := map[string]string{
			plancache.SchemaV1: "project.v1.json",
			plancache.SchemaV2: "project.v2.json",
			plancache.SchemaV3: "project.v3.json",
		}[version]
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			return err
		}
	}
	fmt.Println("wrote schema samples to", dir)
	return nil
}

func runCheckpoint(p *plancache.Project, path string, crashAt int) error {
	cp, err := plancache.OpenCheckpoint(path, p.EngineeringVersion)
	if err != nil {
		return err
	}
	defer cp.Close()
	engine, err := plancache.NewEngine(p)
	if err != nil {
		return err
	}
	reqs := append([]plancache.SessionRequest(nil), p.Requests...)
	sortRequestsBySeq(reqs)
	for _, req := range reqs {
		report, err := engine.ProcessRequest(req)
		if err != nil {
			return err
		}
		entries := engine.ActiveEntries()
		if err := cp.SavePendingEntryState(req.Seq, entries, nil); err != nil {
			return err
		}
		if crashAt == req.Seq {
			_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
			select {}
		}
		if err := cp.CommitPendingRequest(req, *report); err != nil {
			return err
		}
	}
	rec, err := cp.Recover()
	if err != nil {
		return err
	}
	return writeJSON("", map[string]any{"checkpoint": path, "last_seq": rec.LastSeq, "entries": len(rec.Entries), "requests": len(rec.RequestSeqs)})
}

func runRecover(p *plancache.Project, path, out string) error {
	cp, rec, rebuilt, err := plancache.RecoverOrRebuild(path, p)
	if err != nil {
		return err
	}
	defer cp.Close()
	rec.Rebuilt = rebuilt
	return writeJSON(out, rec)
}

func sortRequestsBySeq(reqs []plancache.SessionRequest) {
	for i := 1; i < len(reqs); i++ {
		for j := i; j > 0 && reqs[j].Seq < reqs[j-1].Seq; j-- {
			reqs[j], reqs[j-1] = reqs[j-1], reqs[j]
		}
	}
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if path == "" {
		_, err = os.Stdout.Write(append(b, '\n'))
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
