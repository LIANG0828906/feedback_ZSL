package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	queryopt "queryopt"
)

type verifyReport struct {
	Seed            int64                      `json:"seed"`
	LoadedSchemas   []string                   `json:"loaded_schemas"`
	ParallelResults []queryopt.ScheduledQuery  `json:"parallel_results"`
	SensitivityQ2   *queryopt.QuerySensitivity `json:"sensitivity_q2,omitempty"`
	IncrementalEdit *queryopt.EditReport       `json:"incremental_edit,omitempty"`
	TruthQueries    []queryopt.QueryTruth      `json:"truth_queries"`
}

func main() {
	mode := flag.String("mode", "verify", "verify | persist | recover | crash-demo | inject")
	seed := flag.Int64("seed", queryopt.DefaultSeed, "deterministic generator seed")
	dbPath := flag.String("db", "queryopt.db", "SQLite database path")
	kind := flag.String("kind", "", "fault kind for -mode=inject")
	flag.Parse()

	var err error
	switch *mode {
	case "verify":
		err = runVerify(*seed)
	case "persist":
		err = runPersist(*seed, *dbPath, false)
	case "recover":
		err = runRecover(*dbPath)
	case "crash-demo":
		err = runPersist(*seed, *dbPath, true)
	case "inject":
		err = runInject(*seed, *kind, *dbPath)
	default:
		err = fmt.Errorf("unknown mode %q", *mode)
	}
	if err != nil {
		var coded *queryopt.Error
		if errors.As(err, &coded) {
			writeJSON(map[string]any{"ok": false, "code": coded.Code, "message": coded.Message, "context": coded.Context})
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runVerify(seed int64) error {
	sample, err := queryopt.GenerateSample(seed)
	if err != nil {
		return err
	}
	schemas := []string{queryopt.SchemaV1, queryopt.SchemaV2, queryopt.SchemaV3}
	for _, version := range schemas {
		if _, err := queryopt.LoadProject(version, sample.Wire[version]); err != nil {
			return err
		}
	}
	est, err := queryopt.EstimateProject(sample.Project, sample.Bindings)
	if err != nil {
		return err
	}
	parallel, err := queryopt.RunParallel(sample.Project, est, sample.Bindings, queryopt.OptionsFromProject(sample.Project))
	if err != nil {
		return err
	}
	frequent := queryopt.BindingSet{Global: map[int]int64{1: 5, 2: 25, 3: 1}}
	sensitivity, err := queryopt.AnalyzeSensitivity(sample.Project, sample.Bindings, frequent, queryopt.OptionsFromProject(sample.Project))
	if err != nil {
		return err
	}
	report := verifyReport{Seed: seed, LoadedSchemas: schemas, ParallelResults: parallel.Results, TruthQueries: sample.Truth.Queries}
	for i := range sensitivity.Queries {
		if sensitivity.Queries[i].QueryID == "q2_account_txn" {
			q := sensitivity.Queries[i]
			report.SensitivityQ2 = &q
		}
	}
	engine, err := queryopt.NewIncrementalEngine(sample.Project, sample.Bindings, queryopt.OptionsFromProject(sample.Project))
	if err != nil {
		return err
	}
	buckets := []queryopt.Bucket{}
	for _, b := range engine.Project().Indexes.BucketsByColumn["col_order_date"] {
		buckets = append(buckets, *b)
	}
	buckets[0].Rows = 30_000
	edit, err := engine.EditHistogram("col_order_date", buckets)
	if err != nil {
		return err
	}
	report.IncrementalEdit = edit
	writeJSON(report)
	return nil
}

func runPersist(seed int64, path string, crash bool) error {
	if err := os.MkdirAll(filepath.Dir(pathOrDot(path)), 0o755); err != nil {
		return err
	}
	ctx := context.Background()
	sample, err := queryopt.GenerateSample(seed)
	if err != nil {
		return err
	}
	est, plans, err := queryopt.FullRecompute(sample.Project, sample.Bindings, queryopt.OptionsFromProject(sample.Project))
	if err != nil {
		return err
	}
	store, err := queryopt.OpenStore(ctx, path)
	if err != nil {
		return err
	}
	defer store.Close()
	for _, q := range sample.Project.Queries {
		plan := plans[q.ID]
		if plan == nil {
			continue
		}
		rec := queryopt.CommittedQuery{Ordinal: q.Ordinal, QueryID: q.ID, Plan: plan, Estimates: est}
		if crash && q.Ordinal == 2 {
			err := store.CommitQueryWithCrashAfterState(ctx, rec)
			writeJSON(map[string]any{"crash_injected": true, "after_query": q.ID, "error": err.Error()})
			return nil
		}
		if err := store.CommitQuery(ctx, rec); err != nil {
			return err
		}
	}
	cursor, _ := store.GlobalCommittedOrdinal(ctx)
	rows, err := store.LoadCommitted(ctx)
	if err != nil {
		return err
	}
	writeJSON(map[string]any{"db": path, "committed_ordinal": cursor, "loaded_queries": len(rows)})
	return nil
}

func runRecover(path string) error {
	ctx := context.Background()
	store, err := queryopt.OpenStore(ctx, path)
	if err != nil {
		return err
	}
	defer store.Close()
	recovered, err := store.Recover(ctx)
	if err != nil {
		return err
	}
	cursor, _ := store.GlobalCommittedOrdinal(ctx)
	rows, err := store.LoadCommitted(ctx)
	if err != nil {
		return err
	}
	writeJSON(map[string]any{"db": path, "recovered": recovered, "committed_ordinal": cursor, "loaded_queries": len(rows)})
	return nil
}

func runInject(seed int64, kind, dbPath string) error {
	_ = seed
	ctx := context.Background()
	env, err := queryopt.InjectFault(kind)
	if err != nil {
		return err
	}
	var opErr error
	switch kind {
	case queryopt.FaultHistogramReversed, queryopt.FaultMissingTableEdge, queryopt.FaultNegativeRows:
		opErr = queryopt.ValidateProject(env.Project)
	case queryopt.FaultSelectivityScale:
		_, opErr = queryopt.LoadProject(env.LoadVersion, env.Data)
	case queryopt.FaultWorkerBudgetInvalid, queryopt.FaultBufferBudgetInvalid:
		opErr = queryopt.ValidateBudget(env.Workers, env.Buffer)
	case queryopt.FaultSnapshotMismatch:
		_, opErr = queryopt.LoadSnapshot(env.LoadVersion, env.Data)
	case queryopt.FaultTruncatedCheckpoint:
		sample, err := queryopt.GenerateSample(queryopt.DefaultSeed)
		if err != nil {
			return err
		}
		store, err := queryopt.OpenStore(ctx, dbPath)
		if err != nil {
			return err
		}
		defer store.Close()
		if err := store.SaveCheckpoint(ctx, "engineering", queryopt.SchemaV3, sample.Wire[queryopt.SchemaV3]); err != nil {
			return err
		}
		if err := store.InjectTruncatedCheckpoint(ctx, "engineering"); err != nil {
			return err
		}
		_, opErr = store.LoadCheckpoint(ctx, "engineering", queryopt.SchemaV3)
	}
	var coded *queryopt.Error
	if errors.As(opErr, &coded) {
		writeJSON(map[string]any{"injected": kind, "ok": false, "code": coded.Code, "message": coded.Message, "context": coded.Context})
		return nil
	}
	return opErr
}

func pathOrDot(p string) string {
	if filepath.Dir(p) == "" {
		return "."
	}
	return p
}

func writeJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
