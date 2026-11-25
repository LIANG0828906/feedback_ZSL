package app

import (
	"context"
	"fmt"
	"path/filepath"

	"graphlab/internal/checkpoint"
	"graphlab/internal/engine"
	"graphlab/internal/model"
)

type OperationDisplay struct {
	Report                 engine.OperationReport `json:"report"`
	IncrementalVsFullDiffs []string               `json:"incremental_vs_full_diffs"`
	KeySetDiffs            []string               `json:"key_set_diffs"`
	TruthDiffs             []string               `json:"truth_diffs"`
}

type DemoReport struct {
	SampleDir              string                  `json:"sample_dir"`
	GeneratedSample        bool                    `json:"generated_sample"`
	Seed                   int64                   `json:"seed"`
	CoreDigest             string                  `json:"core_digest"`
	Schemas                []SchemaSummary         `json:"schemas"`
	InitialShardMetrics    []engine.ShardMetric    `json:"initial_shard_metrics"`
	SchemaQueryDiffs       map[string][]string     `json:"schema_query_diffs"`
	InitialQueries         []QueryDisplay          `json:"initial_queries"`
	InitialShardTruthDiffs []string                `json:"initial_shard_truth_diffs"`
	InitialQueryTruthDiffs []string                `json:"initial_query_truth_diffs"`
	Operations             []OperationDisplay      `json:"operations"`
	IncrementalFullEqual   bool                    `json:"incremental_full_equal"`
	OperationTruthMatched  bool                    `json:"operation_truth_matched"`
	StoppedOnSignal        bool                    `json:"stopped_on_signal"`
	CheckpointPath         string                  `json:"checkpoint_path"`
	InitialCheckpoint      checkpoint.CommitReport `json:"initial_checkpoint"`
	FinalHeader            checkpoint.Header       `json:"final_checkpoint_header"`
	RecoveryQueryDiffs     []string                `json:"recovery_query_diffs"`
	ForcedMismatchCode     string                  `json:"forced_mismatch_code"`
	RebuildFallbackDiffs   []string                `json:"rebuild_fallback_diffs"`
	FinalQueryDigest       string                  `json:"final_query_digest"`
	ReportPath             string                  `json:"report_path,omitempty"`
}

func RunDemo(ctx context.Context, opts Options) (*DemoReport, error) {
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	docs, truth, generated, err := LoadOrGenerate(opts)
	if err != nil {
		return nil, err
	}
	bundle, err := loadBundle(docs)
	if err != nil {
		return nil, err
	}
	report := &DemoReport{
		SampleDir: opts.SampleDir, GeneratedSample: generated, Seed: truth.Seed,
		CoreDigest: bundle.CoreDigest, CheckpointPath: opts.CheckpointPath,
		IncrementalFullEqual: true, OperationTruthMatched: true,
		SchemaQueryDiffs: make(map[string][]string),
	}
	v1 := bundle.Projects[model.SchemaV1]
	for _, version := range []string{model.SchemaV1, model.SchemaV2, model.SchemaV3} {
		p := bundle.Projects[version]
		report.Schemas = append(report.Schemas, SchemaSummary{
			Version: version, SchemaVersion: p.SchemaVersion,
			HierarchyEnabled: p.HierarchyEnabled, HierarchyDegraded: p.HierarchyDegraded,
			DegradationNotes: p.DegradationNotes, CoreDigest: model.CoreDigest(p),
			CoreDifferences: model.CompareCore(v1, p), ObservedAtMillis: p.ObservedAtMillis,
			CacheCapacityBytes: p.CacheCapacityByte,
		})
	}
	logf("loaded v1/v2/v3 core=%s equivalent=true (v3 hierarchy=%v degraded=%v)",
		bundle.CoreDigest, bundle.Projects[model.SchemaV3].HierarchyEnabled,
		bundle.Projects[model.SchemaV3].HierarchyDegraded)

	rt, err := engine.NewRuntime(v1, opts.BuildOptions())
	if err != nil {
		return nil, err
	}
	report.InitialShardMetrics = append([]engine.ShardMetric(nil), rt.Metrics...)
	logf("shard build metrics: %v", report.InitialShardMetrics)
	report.InitialShardTruthDiffs = CompareShardTruth(rt, truth)
	initialResults, err := rt.EvaluateAll(engine.LayoutIndex)
	if err != nil {
		return nil, err
	}
	report.InitialQueryTruthDiffs = CompareTruthQueries(initialResults, truth.Queries)
	report.InitialQueries = ToQueryDisplay(initialResults)
	if len(report.InitialShardTruthDiffs) > 0 || len(report.InitialQueryTruthDiffs) > 0 {
		return report, model.Err(model.EValidationFailed, "initial truth validation failed", map[string]any{
			"shard_diffs": report.InitialShardTruthDiffs, "query_diffs": report.InitialQueryTruthDiffs,
		})
	}
	for _, q := range report.InitialQueries {
		logf("query %s cache=%s status=%s visitedV=%v visitedE=%v result=%v",
			q.QueryID, q.CacheID, q.Status, q.VisitedVertices, q.VisitedEdges, q.ResultVertices)
	}
	for _, version := range []string{model.SchemaV2, model.SchemaV3} {
		schemaRuntime, err := engine.NewRuntime(bundle.Projects[version], opts.BuildOptions())
		if err != nil {
			return nil, err
		}
		schemaResults, err := schemaRuntime.EvaluateAll(engine.LayoutIndex)
		if err != nil {
			return nil, err
		}
		report.SchemaQueryDiffs[version] = engine.CompareQueryResults(initialResults, schemaResults)
		logf("schema %s loaded and evaluated: query-diffs-vs-v1=%d hierarchy-enabled=%v",
			version, len(report.SchemaQueryDiffs[version]), bundle.Projects[version].HierarchyEnabled)
	}

	store, err := checkpoint.Open(opts.CheckpointPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()
	initialCommit, err := store.Commit(rt, checkpoint.CommitOptions{})
	if err != nil {
		return nil, err
	}
	report.InitialCheckpoint = initialCommit
	logf("checkpoint committed seq=%d version=%d shards=%d keys=%d caches=%d",
		initialCommit.CommitSeq, initialCommit.ProjectVersion, initialCommit.ShardRows,
		initialCommit.IndexRows, initialCommit.CacheRows)

	for i, op := range rt.Project.Operations {
		select {
		case <-ctx.Done():
			report.StoppedOnSignal = true
			logf("signal received; stopping at last complete commit seq=%d", rt.CommitSeq)
			break
		default:
		}
		if report.StoppedOnSignal {
			break
		}
		opReport, err := rt.ApplyOperation(op, engine.LayoutIndex)
		if err != nil {
			return report, err
		}
		fresh, fullResults, err := rt.FullRebuild(engine.LayoutIndex)
		if err != nil {
			return report, err
		}
		incrementalDiffs := engine.CompareQueryResults(opReport.QueryResults, fullResults)
		keyDiffs := compareKeySetsNoVersion(rt, fresh)
		truthDiffs := CompareTruthQueries(opReport.QueryResults, truth.Operations[i].Queries)
		if !equalStringSets(opReport.AffectedShards, truth.Operations[i].AffectedShards) {
			truthDiffs = append(truthDiffs, "affected shards differ from generator truth")
		}
		if !equalStringSets(opReport.ChangedEntities, truth.Operations[i].ChangedEntities) {
			truthDiffs = append(truthDiffs, "changed entities differ from generator truth")
		}
		if !equalStringSets(opReport.AffectedIndexKeys, truth.Operations[i].AffectedIndexKeys) {
			truthDiffs = append(truthDiffs, "affected index keys differ from generator truth")
		}
		if !equalStringSets(cacheInvalidationIDs(opReport.InvalidatedCaches), truth.Operations[i].InvalidatedCacheIDs) {
			truthDiffs = append(truthDiffs, "invalidated cache IDs differ from generator truth")
		}
		if !equalStringSets(opReport.ReusedCacheIDs, truth.Operations[i].ReusedCacheIDs) {
			truthDiffs = append(truthDiffs, "reused cache IDs differ from generator truth")
		}
		display := OperationDisplay{
			Report: opReport, IncrementalVsFullDiffs: incrementalDiffs, KeySetDiffs: keyDiffs,
			TruthDiffs: truthDiffs,
		}
		report.Operations = append(report.Operations, display)
		if len(truthDiffs) > 0 {
			report.OperationTruthMatched = false
		}
		if len(incrementalDiffs) > 0 || len(keyDiffs) > 0 {
			report.IncrementalFullEqual = false
		}
		logf("op=%d/%d kind=%s shards=%v entities=%d keys=%d invalidated=%d reused=%d incremental/full=%s truth=%s",
			op.Seq, len(rt.Project.Operations), op.Kind, opReport.AffectedShards,
			len(opReport.ChangedEntities), len(opReport.AffectedIndexKeys),
			len(opReport.InvalidatedCaches), len(opReport.ReusedCacheIDs),
			passFail(len(incrementalDiffs) == 0 && len(keyDiffs) == 0),
			passFail(len(truthDiffs) == 0))
		for _, inv := range opReport.InvalidatedCaches {
			logf("  invalidate cache=%s query=%s sources=%v", inv.CacheID, inv.QueryID, inv.Reasons)
		}
		for _, result := range opReport.QueryResults {
			logf("  query %s cache=%s status=%s visitedV=%v result=%v",
				result.QueryID, result.CacheID, result.Status, result.VisitedVertices,
				result.ResultVertices)
		}
		commit, err := store.Commit(rt, checkpoint.CommitOptions{})
		if err != nil {
			return report, err
		}
		logf("  checkpoint seq=%d version=%d committed", commit.CommitSeq, commit.ProjectVersion)
	}
	if !report.IncrementalFullEqual {
		return report, model.Err(model.EValidationFailed,
			"incremental rebuild differs from full rebuild", map[string]any{
				"operations": report.Operations,
			})
	}
	finalResults, err := rt.EvaluateAll(engine.LayoutIndex)
	if err != nil {
		return report, err
	}
	report.FinalQueryDigest = queryResultDigest(finalResults)
	if err := store.Close(); err != nil {
		return report, err
	}
	store, err = checkpoint.Open(opts.CheckpointPath)
	if err != nil {
		return report, err
	}
	recovered, header, err := store.Recover(rt.Project.ID, rt.Project.Version, opts.BuildOptions())
	if err != nil {
		return report, err
	}
	report.FinalHeader = header
	recoveredResults, err := recovered.EvaluateAll(engine.LayoutIndex)
	if err != nil {
		return report, err
	}
	report.RecoveryQueryDiffs = engine.CompareQueryResults(finalResults, recoveredResults)
	logf("checkpoint recovered seq=%d version=%d query-diffs=%d",
		header.CommitSeq, header.ProjectVersion, len(report.RecoveryQueryDiffs))

	_, _, mismatchErr := store.Recover(rt.Project.ID, rt.Project.Version+1, opts.BuildOptions())
	report.ForcedMismatchCode = errorCode(mismatchErr)
	logf("forced version mismatch -> %s (checkpoint untouched)", report.ForcedMismatchCode)
	fallback, err := engine.NewRuntime(rt.Project, opts.BuildOptions())
	if err != nil {
		return report, err
	}
	fallbackResults, err := fallback.EvaluateAll(engine.LayoutIndex)
	if err != nil {
		return report, err
	}
	report.RebuildFallbackDiffs = engine.CompareQueryResults(finalResults, fallbackResults)
	logf("version-mismatch fallback full rebuild query-diffs=%d", len(report.RebuildFallbackDiffs))

	schemaDiffsPresent := false
	for _, diffs := range report.SchemaQueryDiffs {
		if len(diffs) > 0 {
			schemaDiffsPresent = true
		}
	}
	if schemaDiffsPresent || !report.OperationTruthMatched ||
		len(report.RecoveryQueryDiffs) > 0 || len(report.RebuildFallbackDiffs) > 0 ||
		report.ForcedMismatchCode != model.ECheckpointVersionMismatch {
		return report, model.Err(model.EValidationFailed, "checkpoint recovery validation failed",
			map[string]any{
				"recovery_diffs": report.RecoveryQueryDiffs,
				"fallback_diffs": report.RebuildFallbackDiffs,
				"mismatch_code":  report.ForcedMismatchCode,
			})
	}
	reportPath := filepath.Join(opts.OutputDir, "demo-report.json")
	report.ReportPath = reportPath
	if err := WriteJSON(reportPath, report); err != nil {
		return report, err
	}
	logf("wrote %s", reportPath)
	return report, nil
}

func passFail(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

func cacheInvalidationIDs(items []engine.CacheInvalidation) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.CacheID)
	}
	return out
}

func errorCode(err error) string {
	if err == nil {
		return ""
	}
	for err != nil {
		if be, ok := err.(*model.BizError); ok {
			return be.Code
		}
		type unwrapper interface{ Unwrap() error }
		if u, ok := err.(unwrapper); ok {
			err = u.Unwrap()
			continue
		}
		break
	}
	return fmt.Sprintf("%T", err)
}

func compareKeySetsNoVersion(a, b *engine.Runtime) []string {
	var diffs []string
	if len(a.Index.Keys) != len(b.Index.Keys) {
		diffs = append(diffs, fmt.Sprintf("key count %d != %d", len(a.Index.Keys), len(b.Index.Keys)))
	}
	for key, ik := range b.Index.Keys {
		got := a.Index.Keys[key]
		if got == nil || !equalSlices(got.RefShards, ik.RefShards) ||
			!equalSlices(got.Entities, ik.Entities) {
			diffs = append(diffs, "key differs: "+key)
		}
	}
	return diffs
}
