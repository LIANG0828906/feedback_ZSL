package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"example.com/columnar/anomaly"
	"example.com/columnar/checkpoint"
	"example.com/columnar/domain"
	"example.com/columnar/engine"
	"example.com/columnar/generator"
	"example.com/columnar/loader"
)

func main() {
	inject := flag.String("inject", "", "run one anomaly case")
	outDir := flag.String("out", "artifacts/sample", "directory for generated schema files and checkpoint")
	noWrite := flag.Bool("no-write", false, "do not write generated files")
	flag.Parse()

	sample, err := generator.Generate(generator.Seed)
	if err != nil {
		fail(err)
	}
	if *inject != "" {
		c, ok := anomaly.Find(*inject)
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown anomaly %q; cases: %s\n", *inject, strings.Join(anomaly.Names(), ", "))
			os.Exit(2)
		}
		err := anomaly.Inject(*inject, sample)
		code := domain.CodeOf(err)
		matched := code == c.Expected
		fmt.Printf("anomaly=%s expected=%s actual=%s matched=%t\n", c.Name, c.Expected, code, matched)
		if err != nil {
			fmt.Printf("context=%s\n", err)
		}
		if !matched {
			os.Exit(1)
		}
		return
	}
	if !*noWrite {
		if err := generator.WriteSample(*outDir, sample); err != nil {
			fail(err)
		}
	}
	if err := runDemo(sample, *outDir, *noWrite); err != nil {
		fail(err)
	}
}

func runDemo(sample *generator.Sample, outDir string, noWrite bool) error {
	fmt.Println("== deterministic fixture ==")
	fmt.Printf("seed=%d table=%s versions=%v\n\n", sample.Seed, sample.Canonical.Table, sample.VersionOrder)

	var results []*domain.RunResult
	for _, name := range sample.VersionOrder {
		p, err := loader.Load(sample.VersionJSON[name])
		if err != nil {
			return err
		}
		r, err := engine.Run(p, engine.Options{})
		if err != nil {
			return err
		}
		results = append(results, r)
		fmt.Printf("[%s] families=%d initial_batches=%d ops=%d scans=%d\n", name, len(p.Families), len(p.Batches), len(p.Operations), len(p.Scans))
		for _, op := range r.Operations {
			fmt.Printf("  op=%02d kind=%-7s changed=%-10v ", op.Index, op.Kind, op.Changed)
			for _, id := range op.Changed {
				if b, ok := batchByID(op.Batches, id); ok {
					fmt.Printf("b%d(rows=%d bytes=%d digest=%s) ", id, b.Rows, b.CompressedBytes, domain.FormatDigest(b.Digest))
				}
			}
			fmt.Printf("check=%t tolerance=%t\n", op.Check.Match, op.Check.WithinTolerance)
		}
		for _, scan := range r.Scans {
			fmt.Printf("  scan=%s read=%v rows=%d rebuilt=%v reused=%v invalid=%v cold=%t hit=%t backpressure=%d\n",
				scan.ScanID, scan.ReadBatches, scan.DecodedRows, scan.RebuiltBatches,
				scan.ReusedBatches, scan.InvalidSources, scan.ColdCache, scan.CacheHit,
				scan.Budget.BackpressureEvents)
		}
		fmt.Println()
	}
	if err := engine.VerifyVersionEquivalence(results); err != nil {
		return fmt.Errorf("version equivalence: %w", err)
	}
	fmt.Println("version-equivalence: OK (operations, digests, dependencies and scans)")
	fmt.Println()

	fmt.Println("== incremental edit ==")
	v2 := results[1].Project
	base := results[1]
	ed := engine.NewEditor(v2)
	if err := ed.SetPatchRange(11, 2, 5); err != nil {
		return err
	}
	if err := ed.SetOperationEncoding(11, "zstd-delta-v2"); err != nil {
		return err
	}
	if err := ed.AddScan(domain.Scan{
		ID: "scan-9", AfterOp: 9, Columns: []string{"id"}, FromRow: 0, ToRow: 220, AtMillis: 235,
	}); err != nil {
		return err
	}
	edited := ed.Project()
	full, err := engine.Run(edited, engine.Options{})
	if err != nil {
		return err
	}
	incremental, stats, err := engine.RunIncremental(base, edited, engine.Options{})
	if err != nil {
		return err
	}
	if err := engine.CompareResults(full, incremental); err != nil {
		return fmt.Errorf("incremental/full mismatch: %w", err)
	}
	fmt.Printf("closure=%v recomputed_ops=%v reused_ops=%v recomputed_scans=%v reused_scans=%v added_scans=%v\n",
		stats.ClosureBatchIDs, stats.RecomputedOps, stats.ReusedOps,
		stats.RecomputedScans, stats.ReusedScans, stats.AddedScans)
	fmt.Println("incremental-vs-full: OK")
	fmt.Println()

	fmt.Println("== SQLite checkpoint ==")
	if noWrite {
		tmp, err := os.MkdirTemp("", "columnar-demo-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		outDir = tmp
	}
	cpDir := filepath.Join(outDir, "checkpoint")
	if err := os.RemoveAll(cpDir); err != nil {
		return err
	}
	dbPath := filepath.Join(cpDir, "state.db")
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	var last domain.CheckpointData
	saved := 0
	if _, err := engine.Run(v2, engine.Options{OnCommit: func(data domain.CheckpointData) error {
		last = data
		saved++
		return store.Save(context.Background(), data)
	}}); err != nil {
		return err
	}
	recovered, err := store.Load(context.Background(), checkpoint.Expectation{
		SchemaVersion: "v2", EngineVersion: engine.EngineVersion,
		ContentDigest: engine.ContentDigest(v2),
	})
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(recovered.Data.Batches, base.FinalBatches) {
		return fmt.Errorf("recovered batches differ from committed result")
	}
	fmt.Printf("checkpoints_written=%d committed_op=%d recovered_batches=%d scan_cache=%d integrity=%s\n",
		saved, recovered.Data.CommittedOp, len(recovered.Data.Batches), len(recovered.Data.ScanCache), recovered.Integrity)

	failed := last
	failed.CommittedOp = 7
	err = store.SaveWithFailpoint(context.Background(), failed, func(stage string) error {
		if stage == "after_batches" {
			return errors.New("simulated process stop before global sequence update")
		}
		return nil
	})
	fmt.Printf("injected_stop=%s\n", domain.CodeOf(err))
	afterFail, err := store.Load(context.Background(), checkpoint.Expectation{SchemaVersion: "v2", EngineVersion: engine.EngineVersion})
	if err != nil {
		return err
	}
	if afterFail.Data.CommittedOp != last.CommittedOp {
		return fmt.Errorf("failed transaction changed committed op: %d", afterFail.Data.CommittedOp)
	}
	fmt.Printf("rollback-kept-committed-op=%d batches-readable=%d\n", afterFail.Data.CommittedOp, len(afterFail.Data.Batches))

	_, err = store.Load(context.Background(), checkpoint.Expectation{SchemaVersion: "v3", EngineVersion: engine.EngineVersion})
	fmt.Printf("version-mismatch=%s\n", domain.CodeOf(err))

	truncatedPath := filepath.Join(cpDir, "truncated.db")
	raw, err := os.ReadFile(dbPath)
	if err != nil {
		return err
	}
	if err := os.WriteFile(truncatedPath, raw[:len(raw)/2], 0o644); err != nil {
		return err
	}
	truncatedStore, err := checkpoint.Open(truncatedPath)
	if truncatedStore != nil {
		_ = truncatedStore.Close()
	}
	fmt.Printf("truncated-checkpoint=%s\n\n", domain.CodeOf(err))

	fmt.Println("== anomaly injections ==")
	for _, c := range anomaly.Cases {
		err := anomaly.Inject(c.Name, sample)
		fmt.Printf("%-20s expected=%-22s actual=%-22s matched=%t\n", c.Name, c.Expected, domain.CodeOf(err), domain.CodeOf(err) == c.Expected)
	}
	return nil
}

func batchByID(batches []domain.Batch, id int) (domain.Batch, bool) {
	for _, b := range batches {
		if b.ID == id {
			return b, true
		}
	}
	return domain.Batch{}, false
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
