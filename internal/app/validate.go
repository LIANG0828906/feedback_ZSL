package app

import (
	"encoding/json"

	"graphlab/internal/engine"
	"graphlab/internal/model"
)

type CheckResult struct {
	Name   string   `json:"name"`
	Passed bool     `json:"passed"`
	Diffs  []string `json:"diffs,omitempty"`
}

type ValidationReport struct {
	SampleDir       string               `json:"sample_dir"`
	GeneratedSample bool                 `json:"generated_sample"`
	Seed            int64                `json:"seed"`
	CoreDigest      string               `json:"core_digest"`
	Schemas         []SchemaSummary      `json:"schemas"`
	Checks          []CheckResult        `json:"checks"`
	ShardMetrics    []engine.ShardMetric `json:"shard_metrics"`
	AllPassed       bool                 `json:"all_passed"`
	Workers         int                  `json:"workers"`
	BufferPerWorker int                  `json:"buffer_per_worker"`
}

func Validate(opts Options) (*ValidationReport, error) {
	docs, truth, generated, err := LoadOrGenerate(opts)
	if err != nil {
		return nil, err
	}
	bundle, err := loadBundle(docs)
	if err != nil {
		return nil, err
	}
	report := &ValidationReport{
		SampleDir: opts.SampleDir, GeneratedSample: generated, Seed: truth.Seed,
		CoreDigest: bundle.CoreDigest, Workers: opts.Workers,
		BufferPerWorker: opts.BufferPerWorker, AllPassed: true,
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
	addCheck(report, "schema_core_equivalence", len(bundle.Consistency) == 0, bundle.Consistency)
	addCheck(report, "hierarchy_optional_degradation",
		!bundle.Projects[model.SchemaV3].HierarchyEnabled &&
			bundle.Projects[model.SchemaV3].HierarchyDegraded &&
			bundle.Projects[model.SchemaV1].HierarchyEnabled &&
			bundle.Projects[model.SchemaV2].HierarchyEnabled, nil)

	rtV1, err := engine.NewRuntime(v1, opts.BuildOptions())
	if err != nil {
		return nil, err
	}
	report.ShardMetrics = append([]engine.ShardMetric(nil), rtV1.Metrics...)
	diffs := CompareShardTruth(rtV1, truth)
	addCheck(report, "v1_shard_order_and_keys", len(diffs) == 0, diffs)
	v1Results, err := rtV1.EvaluateAll(engine.LayoutIndex)
	if err != nil {
		return nil, err
	}
	queryDiffs := CompareTruthQueries(v1Results, truth.Queries)
	addCheck(report, "v1_query_truth", len(queryDiffs) == 0, queryDiffs)

	for _, version := range []string{model.SchemaV2, model.SchemaV3} {
		rt, err := engine.NewRuntime(bundle.Projects[version], opts.BuildOptions())
		if err != nil {
			return nil, err
		}
		diffs := CompareShardTruth(rt, truth)
		if version == model.SchemaV3 {
			diffs = compareShardOrdersOnly(rt, truth)
		}
		addCheck(report, version+"_shard_order", len(diffs) == 0, diffs)
		results, err := rt.EvaluateAll(engine.LayoutIndex)
		if err != nil {
			return nil, err
		}
		queryDiffs := CompareTruthQueries(results, truth.Queries)
		if version == model.SchemaV2 {
			addCheck(report, "v2_index_and_query_truth", len(queryDiffs) == 0, queryDiffs)
		} else {
			addCheck(report, "v3_query_truth_with_degraded_hierarchy", len(queryDiffs) == 0, queryDiffs)
		}
		equalDiffs := engine.CompareQueryResults(v1Results, results)
		addCheck(report, version+"_query_results_equal_v1", len(equalDiffs) == 0, equalDiffs)
	}

	rtIndex, err := engine.NewRuntime(v1, opts.BuildOptions())
	if err != nil {
		return nil, err
	}
	indexResults, err := rtIndex.EvaluateAll(engine.LayoutIndex)
	if err != nil {
		return nil, err
	}
	rtScan, err := engine.NewRuntime(v1, opts.BuildOptions())
	if err != nil {
		return nil, err
	}
	scanResults, err := rtScan.EvaluateAll(engine.LayoutScan)
	if err != nil {
		return nil, err
	}
	layoutDiffs := engine.CompareQueryResults(indexResults, scanResults)
	addCheck(report, "index_vs_scan_layout", len(layoutDiffs) == 0, layoutDiffs)

	rt1, err := engine.NewRuntime(v1, engine.BuildOptions{Workers: 1, BufferPerWorker: opts.BufferPerWorker})
	if err != nil {
		return nil, err
	}
	rtN, err := engine.NewRuntime(v1, opts.BuildOptions())
	if err != nil {
		return nil, err
	}
	snapshotDiffs := compareRuntimeSnapshots(rt1, rtN)
	res1, err := rt1.EvaluateAll(engine.LayoutIndex)
	if err != nil {
		return nil, err
	}
	resN, err := rtN.EvaluateAll(engine.LayoutIndex)
	if err != nil {
		return nil, err
	}
	snapshotDiffs = append(snapshotDiffs, engine.CompareQueryResults(res1, resN)...)
	if !equalSlices(res1[0].Dependencies, resN[0].Dependencies) {
		snapshotDiffs = append(snapshotDiffs, "query dependencies differ across worker counts")
	}
	addCheck(report, "concurrency_determinism", len(snapshotDiffs) == 0, snapshotDiffs)

	for _, check := range report.Checks {
		if !check.Passed {
			report.AllPassed = false
		}
	}
	if !report.AllPassed {
		return report, model.Err(model.EValidationFailed, "deterministic validation failed",
			map[string]any{"checks": report.Checks})
	}
	return report, nil
}

func addCheck(report *ValidationReport, name string, passed bool, diffs []string) {
	report.Checks = append(report.Checks, CheckResult{Name: name, Passed: passed, Diffs: diffs})
}

func compareShardOrdersOnly(rt *engine.Runtime, truth *model.Truth) []string {
	var diffs []string
	for shardID, want := range truth.Shards {
		si := rt.Index.Shards[shardID]
		if si == nil {
			diffs = append(diffs, "missing shard "+shardID)
			continue
		}
		if !equalSlices(si.VertexOrder, want.VertexOrder) {
			diffs = append(diffs, "vertex order differs in "+shardID)
		}
		if !equalSlices(si.EdgeOrder, want.EdgeOrder) {
			diffs = append(diffs, "edge order differs in "+shardID)
		}
	}
	return diffs
}

func compareRuntimeSnapshots(a, b *engine.Runtime) []string {
	var diffs []string
	if model.CoreDigest(a.Project) != model.CoreDigest(b.Project) {
		diffs = append(diffs, "core digest differs")
	}
	if len(a.Index.Shards) != len(b.Index.Shards) || len(a.Index.Keys) != len(b.Index.Keys) {
		diffs = append(diffs, "index size differs")
		return diffs
	}
	for id, si := range a.Index.Shards {
		other := b.Index.Shards[id]
		if other == nil || !equalSlices(si.VertexOrder, other.VertexOrder) ||
			!equalSlices(si.EdgeOrder, other.EdgeOrder) ||
			!equalSlices(a.Index.ShardKeys(id), b.Index.ShardKeys(id)) {
			diffs = append(diffs, "shard differs: "+id)
		}
	}
	for _, key := range a.Index.KeyViews() {
		other := b.Index.Keys[key.Key]
		if other == nil || key.Version != other.Version ||
			!equalSlices(key.RefShards, other.RefShards) ||
			!equalSlices(key.Entities, other.Entities) {
			diffs = append(diffs, "index key differs: "+key.Key)
		}
	}
	return diffs
}

func queryResultDigest(results []model.QueryResult) string {
	b, _ := json.Marshal(results)
	return digestBytes(b)
}
