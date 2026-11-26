package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	cb "cachebench"
)

func main() {
	out := flag.String("out", "artifacts/demo", "directory for deterministic schema samples and sqlite checkpoint")
	seed := flag.Int64("seed", cb.DefaultSeed, "deterministic generator seed")
	flag.Parse()
	if err := run(*out, *seed); err != nil {
		var de *cb.DomainError
		if errors.As(err, &de) {
			fmt.Fprintf(os.Stderr, "FAILED %s %s\n", de.Code, de.Error())
		} else {
			fmt.Fprintf(os.Stderr, "FAILED %v\n", err)
		}
		os.Exit(1)
	}
}

func run(out string, seed int64) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	bundle, err := cb.GenerateBundle(seed)
	if err != nil {
		return err
	}

	loaded := map[string]*cb.Project{}
	for version, doc := range bundle.Docs {
		path := filepath.Join(out, version+".json")
		if err := os.WriteFile(path, doc, 0o644); err != nil {
			return err
		}
		p, err := cb.Load(doc)
		if err != nil {
			return err
		}
		loaded[version] = p
	}
	fp1 := loaded[cb.SchemaV1].CanonicalFingerprint()
	fp2 := loaded[cb.SchemaV2].CanonicalFingerprint()
	fp3 := loaded[cb.SchemaV3].CanonicalFingerprint()
	if !reflect.DeepEqual(fp1, fp2) || !reflect.DeepEqual(fp1, fp3) {
		return errors.New("schema fingerprints differ")
	}

	parallel, err := cb.ParallelSimulate(bundle.Project, cb.Budget{Workers: 4, BufferPerWorker: 32})
	if err != nil {
		return err
	}
	if !cb.ResultsEqual(bundle.Truth, parallel.Result) {
		return errors.New("parallel result differs from sequential truth")
	}

	inc, err := cb.NewIncrementalEngine(bundle.Project)
	if err != nil {
		return err
	}
	edits := []cb.Edit{
		{ID: "cap-after-20", Kind: cb.EditMaxInflight, NodeID: "cache-north-1", AtMS: 20, IntValue: 3},
		{ID: "weight-zero-after-15", Kind: cb.EditNodeWeight, NodeID: "cache-south-2", AtMS: 15, IntValue: 0},
		{ID: "threshold-after-12", Kind: cb.EditBreakerThreshold, NodeID: "cache-north-2", AtMS: 12, IntValue: 1},
		{ID: "add-after-22", Kind: cb.EditAddRequest, AtMS: 22, Request: &cb.Request{ID: "req-added-demo", AtMS: 22, Candidates: []string{"cache-south-1"}}},
		{ID: "delete-req-0001", Kind: cb.EditDeleteRequest, RequestID: "req-0001"},
	}
	for _, edit := range edits {
		report, err := inc.ApplyEdit(edit)
		if err != nil {
			return err
		}
		full, equal, err := inc.VerifyAgainstFull()
		if err != nil {
			return err
		}
		if !equal {
			_ = full
			return fmt.Errorf("incremental result mismatch after %s", edit.ID)
		}
		fmt.Printf("incremental %-22s reused=%3d recomputed=%3d from=%d\n", edit.ID, report.ReusedCells, report.RecomputedCells, report.FromMS)
	}

	cpPath := filepath.Join(out, "checkpoint.sqlite")
	_ = os.Remove(cpPath)
	store, err := cb.OpenSQLiteCheckpoint(cpPath)
	if err != nil {
		return err
	}
	last := bundle.Truth.Ticks[len(bundle.Truth.Ticks)-1]
	cpData := cb.CheckpointData{ProjectSchemaVersion: cb.SchemaV3, CommittedTick: last.TickMS, Nodes: last.Nodes, Windows: bundle.Truth.Windows}
	if err := store.SaveCommitted(cpData); err != nil {
		return err
	}
	loadedCP, err := store.LoadCommitted()
	if err != nil {
		return err
	}
	if loadedCP.CommittedTick != cpData.CommittedTick || len(loadedCP.Nodes) != len(cpData.Nodes) {
		return errors.New("checkpoint reload mismatch")
	}
	if err := store.Close(); err != nil {
		return err
	}

	critical := cb.RunCriticalConversions(bundle.Project)

	fmt.Println("cachebench deterministic demo")
	fmt.Printf("project=%s seed=%d nodes=%d requests=%d ticks=%d windows=%d\n", bundle.Project.ProjectID, seed, len(bundle.Project.Nodes), len(bundle.Project.Requests), len(bundle.Truth.Ticks), len(bundle.Truth.Windows))
	fmt.Printf("schema load: %s, %s, %s canonical fingerprints equal\n", cb.SchemaV1, cb.SchemaV2, cb.SchemaV3)
	fmt.Printf("routing decisions=%d overflow=%d throughput=%d breaker-open=%d recovered=%d windows-within-tolerance=%d/%d open-final=%v\n",
		countDecisions(bundle.Truth), countOverflow(bundle.Truth), sumThroughput(bundle.Truth), countBreakerOpens(bundle.Truth), countBreakerRecoveries(bundle.Truth), windowsWithin(bundle.Truth.Windows), len(bundle.Truth.Windows), last.OpenCircuits)
	fmt.Printf("parallel workers=%d buffer/worker=%d equal-truth=true backpressure-events=%d node-diagnostics=%d\n", parallel.Workers, parallel.BufferPerWorker, len(parallel.Backpressure), len(parallel.NodeComputeNS))
	fmt.Printf("critical conversions=%d all-within-declared-bounds=%v\n", len(critical), allCriticalOK(critical))
	fmt.Printf("checkpoint=%s committed_tick=%d nodes=%d windows=%d\n", cpPath, loadedCP.CommittedTick, len(loadedCP.Nodes), len(loadedCP.Windows))
	fmt.Println("exception codes:")
	printInjection(cb.InjectNegativeCapacity)
	printInjection(cb.InjectWeightOutOfRange)
	printInjection(cb.InjectBreakerOrder)
	printInjection(cb.InjectMissingNode)
	printBudget("worker", cb.Budget{Workers: 0, BufferPerWorker: 8})
	printBudget("buffer", cb.Budget{Workers: 2, BufferPerWorker: 0})
	if err := demoCheckpointFaults(cpPath, out); err != nil {
		return err
	}
	return nil
}

func countOverflow(r *cb.SimulationResult) int {
	n := 0
	for _, t := range r.Ticks {
		n += len(t.Overflow)
	}
	return n
}

func countDecisions(r *cb.SimulationResult) int {
	n := 0
	for _, t := range r.Ticks {
		n += len(t.Decisions)
	}
	return n
}

func countBreakerOpens(r *cb.SimulationResult) int {
	n := 0
	for _, t := range r.Ticks {
		for _, ev := range t.BreakerEvents {
			if ev.Opened {
				n++
			}
		}
	}
	return n
}

func countBreakerRecoveries(r *cb.SimulationResult) int {
	n := 0
	for _, t := range r.Ticks {
		for _, ev := range t.BreakerEvents {
			if ev.Recovered {
				n++
			}
		}
	}
	return n
}

func windowsWithin(ws []cb.WindowStat) int {
	n := 0
	for _, w := range ws {
		if w.WithinTolerance {
			n++
		}
	}
	return n
}

func sumThroughput(r *cb.SimulationResult) int64 {
	var n int64
	for _, t := range r.Ticks {
		n += t.TotalThroughput
	}
	return n
}

func allCriticalOK(rs []cb.CriticalReport) bool {
	for _, r := range rs {
		if !r.WithinTolerance {
			return false
		}
	}
	return true
}

func printInjection(kind cb.Injection) {
	bundle, _ := cb.GenerateBundle(cb.DefaultSeed)
	_, err := cb.InjectProjectFault(bundle.Project, kind)
	fmt.Printf("  %-28s -> %s\n", kind, cb.CodeOf(err))
}

func printBudget(name string, b cb.Budget) {
	err := cb.ValidateBudget(b)
	fmt.Printf("  budget-%-22s -> %s\n", name, cb.CodeOf(err))
}

func demoCheckpointFaults(goodPath, out string) error {
	truncPath := filepath.Join(out, "truncated.sqlite")
	wrongPath := filepath.Join(out, "version-mismatch.sqlite")
	if err := copyFile(goodPath, truncPath); err != nil {
		return err
	}
	if err := cb.TruncateCheckpointForInjection(truncPath); err != nil {
		return err
	}
	_, err := cb.OpenSQLiteCheckpoint(truncPath)
	code := cb.CodeOf(err)
	if code == "" {
		// Open is lazy; force a query through RecoverOrRebuild or Load.
		store, openErr := cb.OpenSQLiteCheckpoint(truncPath)
		if openErr == nil {
			_, loadErr := store.LoadCommitted()
			code = cb.CodeOf(loadErr)
			_ = store.Close()
		} else {
			code = cb.CodeOf(openErr)
		}
	}
	fmt.Printf("  %-28s -> %s\n", cb.InjectTruncatedCP, code)

	store, err := cb.OpenSQLiteCheckpoint(wrongPath)
	if err != nil {
		return err
	}
	if err := store.SaveCommitted(cb.CheckpointData{Version: "checkpoint.v0", CommittedTick: 1}); err != nil {
		return err
	}
	_, loadErr := store.LoadCommitted()
	fmt.Printf("  %-28s -> %s\n", cb.InjectVersionMismatch, cb.CodeOf(loadErr))
	_ = store.Close()
	return nil
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}
