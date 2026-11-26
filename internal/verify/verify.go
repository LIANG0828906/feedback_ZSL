package verify

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"

	"enginelab.local/internal/gen"
	"enginelab.local/internal/loader"
	"enginelab.local/internal/model"
	"enginelab.local/internal/recovery"
	"enginelab.local/internal/workbench"
)

type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Code   string `json:"code,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type Report struct {
	OK             bool     `json:"ok"`
	Seed           int64    `json:"seed"`
	GoVersion      string   `json:"go_version"`
	Checks         []Check  `json:"checks"`
	MomentParity   []bool   `json:"moment_parity"`
	InjectionCodes []string `json:"injection_codes"`
	ArtifactFiles  []string `json:"artifact_files"`
}

func Run(ctx context.Context, outDir string) (*Report, error) {
	if outDir == "" {
		outDir = "artifacts"
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	report := &Report{OK: true, Seed: gen.DefaultSeed, GoVersion: runtime.Version()}
	add := func(name string, ok bool, detail string) {
		report.Checks = append(report.Checks, Check{Name: name, OK: ok, Detail: detail})
		if !ok {
			report.OK = false
		}
	}

	bundle := gen.NewBundle(gen.DefaultSeed)
	loaded := map[model.SchemaVersion]*model.Project{}
	for version, doc := range bundle.Docs {
		p, err := loader.LoadDocument(doc)
		if err != nil {
			add("load_"+string(version), false, err.Error())
			continue
		}
		loaded[version] = p
	}
	if err := recovery.CompareVersionProjects(loaded); err != nil {
		add("three_schema_equivalence", false, err.Error())
	} else {
		add("three_schema_equivalence", true, "v1/v2/v3 normalized projects are equivalent")
	}

	result, err := recovery.RecoverOneShot(bundle.Project, recovery.Options{Budget: workbench.DefaultBudget(), BaselineItems: bundle.Truth.CheckpointItems})
	if err != nil {
		add("truth_recovery", false, err.Error())
	} else {
		equal := len(result.Items) == len(bundle.Truth.Items)
		for i := range result.Items {
			if i >= len(bundle.Truth.Items) || result.Items[i] != bundle.Truth.Items[i] {
				equal = false
			}
		}
		add("truth_recovery", equal, "items and ranges compared to generator truth")
	}

	dbPath := filepath.Join(outDir, "verify.sqlite")
	svc, err := workbench.NewService(workbench.Config{Seed: gen.DefaultSeed, DBPath: dbPath, ResetDB: true, Budget: workbench.DefaultBudget()})
	if err != nil {
		add("workbench_sqlite", false, err.Error())
		return report, writeReport(outDir, report, bundle)
	}
	defer svc.Close()

	for _, moment := range svc.Timeline() {
		report.MomentParity = append(report.MomentParity, moment.Comparison.Equal)
		if !moment.Comparison.Equal {
			add("moment_"+moment.Key, false, joinDifferences(moment.Comparison.Differences))
		}
	}
	add("incremental_moments", allTrue(report.MomentParity), "all five logical moments match one-shot recompute")

	restore, err := svc.RestoreCheckpoint(ctx)
	if err != nil {
		add("sqlite_restore_parity", false, err.Error())
	} else {
		add("sqlite_restore_parity", restore.Parity, joinDifferences(restore.Differences))
	}

	injections := []workbench.InjectRequest{
		{Kind: "duplicate_segment_ref"}, {Kind: "cross_version_base_conflict"},
		{Kind: "duplicate_record"}, {Kind: "sequence_order"}, {Kind: "dependency_cycle"},
		{Kind: "dependency_missing"}, {Kind: "truncated_segment_file"},
		{Kind: "incomplete_record"}, {Kind: "truncated_checkpoint"},
		{Kind: "budget_threads", Workers: 999}, {Kind: "budget_memory", MemoryBytes: 999999999},
		{Kind: "backpressure"}, {Kind: "schema_version_mismatch"}, {Kind: "version_mismatch"},
	}
	for _, req := range injections {
		resp := svc.Inject(ctx, req)
		report.InjectionCodes = append(report.InjectionCodes, string(resp.Code))
		expectedOK := req.Kind == "truncated_segment_file" || req.Kind == "incomplete_record" || req.Kind == "truncated_checkpoint"
		ok := resp.Code != "" && resp.OK == expectedOK
		add("inject_"+req.Kind, ok, string(resp.Code))
	}

	parallelOK := true
	base := result
	for _, workers := range []int{1, 2, 4} {
		got, err := recovery.RecoverOneShot(bundle.Project, recovery.Options{
			Budget:          model.Budget{Workers: workers, QueueDepth: 16, MaxSegmentMemoryBytes: recovery.DefaultMaxSegmentMemoryBytes},
			BaselineItems:   bundle.Truth.CheckpointItems,
			ReverseSchedule: workers == 2,
			CommitBatchSize: workers,
		})
		if err != nil || !recovery.CompareResults(base, got).Equal {
			parallelOK = false
		}
	}
	add("parallel_determinism", parallelOK, "worker counts, reverse schedule and batches preserve correctness")

	report.ArtifactFiles = []string{"verification-report.json", "sample_truth.json", "demo_summary.json"}
	if err := writeReport(outDir, report, bundle); err != nil {
		return report, err
	}
	return report, nil
}

func allTrue(in []bool) bool {
	for _, v := range in {
		if !v {
			return false
		}
	}
	return len(in) > 0
}

func joinDifferences(in []string) string {
	if len(in) == 0 {
		return ""
	}
	out := in[0]
	for _, s := range in[1:] {
		out += "; " + s
	}
	return out
}

func writeReport(outDir string, report *Report, bundle *gen.Bundle) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "verification-report.json"), data, 0o644); err != nil {
		return err
	}
	truth, err := json.MarshalIndent(bundle.Truth, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "sample_truth.json"), truth, 0o644); err != nil {
		return err
	}
	summary := map[string]any{
		"seed":            bundle.Seed,
		"project_id":      bundle.Project.ID,
		"segments":        len(bundle.Project.Segments),
		"truth_items":     len(bundle.Truth.Items),
		"committed_txs":   len(bundle.Truth.CommittedTxIDs),
		"aborted_txs":     len(bundle.Truth.AbortedTxIDs),
		"uncommitted_txs": len(bundle.Truth.UncommittedTxIDs),
		"schema_versions": []string{model.SchemaV1, model.SchemaV2, model.SchemaV3},
		"verification_ok": report.OK,
		"injection_codes": report.InjectionCodes,
	}
	sdata, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outDir, "demo_summary.json"), sdata, 0o644)
}
