package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"graphlab/internal/checkpoint"
	"graphlab/internal/engine"
	"graphlab/internal/loader"
	"graphlab/internal/model"
)

type FaultCase struct {
	Name         string         `json:"name"`
	ExpectedCode string         `json:"expected_code"`
	ActualCode   string         `json:"actual_code"`
	Passed       bool           `json:"passed"`
	Context      map[string]any `json:"context,omitempty"`
	Details      string         `json:"details,omitempty"`
}

type FaultReport struct {
	SampleDir                   string            `json:"sample_dir"`
	GeneratedSample             bool              `json:"generated_sample"`
	Cases                       []FaultCase       `json:"cases"`
	AllExpected                 bool              `json:"all_expected"`
	SourceHashesBefore          map[string]string `json:"source_hashes_before"`
	SourceHashesAfter           map[string]string `json:"source_hashes_after"`
	SourceUnchanged             bool              `json:"source_unchanged"`
	CommittedRuntimeStillUsable bool              `json:"committed_runtime_still_usable"`
	OriginalCheckpointUsable    bool              `json:"original_checkpoint_usable"`
	ReportPath                  string            `json:"report_path,omitempty"`
}

func RunFaults(opts Options) (*FaultReport, error) {
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	docs, _, generated, err := LoadOrGenerate(opts)
	if err != nil {
		return nil, err
	}
	bundle, err := loadBundle(docs)
	if err != nil {
		return nil, err
	}
	report := &FaultReport{
		SampleDir: opts.SampleDir, GeneratedSample: generated,
		SourceHashesBefore: DocHashes(docs),
		AllExpected:        true,
	}
	v1Bytes := docs[model.SchemaV1]

	badScale, err := mutateV1(v1Bytes, func(m map[string]any) {
		vertex := firstArrayMap(m, "vertices")
		vertex["attrs"].(map[string]any)["score"].(map[string]any)["scale"] = 999
	})
	if err != nil {
		return nil, err
	}
	_, caseErr := loader.Load(badScale)
	report.Cases = append(report.Cases, makeCase("out_of_range_attribute_scale", model.EBadAttributeScale, caseErr))

	missingVertex, err := mutateV1(v1Bytes, func(m map[string]any) {
		firstArrayMap(m, "edges")["target_vertex"] = "v9998"
	})
	if err != nil {
		return nil, err
	}
	_, caseErr = loader.Load(missingVertex)
	report.Cases = append(report.Cases, makeCase("edge_points_to_missing_vertex", model.EMissingVertex, caseErr))

	overlap, err := mutateV1(v1Bytes, func(m map[string]any) {
		m["vertex_ranges"].([]any)[1].(map[string]any)["start_key"] = "v0005"
	})
	if err != nil {
		return nil, err
	}
	_, caseErr = loader.Load(overlap)
	report.Cases = append(report.Cases, makeCase("overlapping_shard_ranges", model.EOverlapShard, caseErr))

	v1 := bundle.Projects[model.SchemaV1]
	_, caseErr = engine.NewRuntime(v1, engine.BuildOptions{Workers: 0, BufferPerWorker: opts.BufferPerWorker})
	report.Cases = append(report.Cases, makeCase("worker_budget_exceeded", model.EThreadBudget, caseErr))
	_, caseErr = engine.NewRuntime(v1, engine.BuildOptions{Workers: opts.Workers, BufferPerWorker: 1})
	report.Cases = append(report.Cases, makeCase("index_buffer_exhausted", model.EBufferBudget, caseErr))

	baseline, err := engine.NewRuntime(v1, opts.BuildOptions())
	if err != nil {
		return nil, err
	}
	beforeResults, err := baseline.EvaluateAll(engine.LayoutIndex)
	if err != nil {
		return nil, err
	}
	beforeDigest := model.CoreDigest(baseline.Project)
	faultyEdge := model.Edge{
		ID: "e9999", From: "v0000", To: "v9998", Directed: true,
		Weight: 1, DeclOrder: 1000,
	}
	_, caseErr = baseline.ApplyOperation(model.Operation{
		Seq: 1, Kind: model.OpEdgeAdd, EdgeID: faultyEdge.ID, Edge: &faultyEdge,
	}, engine.LayoutIndex)
	report.Cases = append(report.Cases, makeCase("invalid_operation_rollback", model.EMissingVertex, caseErr))
	afterResults, err := baseline.EvaluateAll(engine.LayoutIndex)
	if err != nil {
		return nil, err
	}
	report.CommittedRuntimeStillUsable = model.CoreDigest(baseline.Project) == beforeDigest &&
		len(engine.CompareQueryResults(beforeResults, afterResults)) == 0

	dir := filepath.Join(opts.OutputDir, "faults")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	goodPath := filepath.Join(dir, "good-checkpoint.db")
	store, err := checkpoint.Open(goodPath)
	if err != nil {
		return nil, err
	}
	if _, err := store.Commit(baseline, checkpoint.CommitOptions{}); err != nil {
		return nil, err
	}
	if err := store.Close(); err != nil {
		return nil, err
	}

	truncatedPath := filepath.Join(dir, "truncated-checkpoint.db")
	if err := copyFile(goodPath, truncatedPath); err != nil {
		return nil, err
	}
	info, err := os.Stat(truncatedPath)
	if err != nil {
		return nil, err
	}
	if err := os.Truncate(truncatedPath, info.Size()/3); err != nil {
		return nil, err
	}
	_, caseErr = checkpoint.Open(truncatedPath)
	report.Cases = append(report.Cases, makeCase("truncated_checkpoint", model.ECheckpointTruncated, caseErr))

	store, err = checkpoint.Open(goodPath)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	_, _, caseErr = store.Recover(baseline.Project.ID, baseline.Project.Version+1, opts.BuildOptions())
	report.Cases = append(report.Cases, makeCase("version_mismatch_snapshot", model.ECheckpointVersionMismatch, caseErr))
	recovered, _, recoverErr := store.Recover(baseline.Project.ID, baseline.Project.Version, opts.BuildOptions())
	if recoverErr != nil {
		report.OriginalCheckpointUsable = false
	} else {
		recoveredResults, evalErr := recovered.EvaluateAll(engine.LayoutIndex)
		report.OriginalCheckpointUsable = evalErr == nil &&
			len(engine.CompareQueryResults(beforeResults, recoveredResults)) == 0
	}

	for _, item := range report.Cases {
		if !item.Passed {
			report.AllExpected = false
		}
	}
	report.SourceHashesAfter = DocHashes(docs)
	report.SourceUnchanged = sameStringMap(report.SourceHashesBefore, report.SourceHashesAfter)
	report.AllExpected = report.AllExpected && report.SourceUnchanged &&
		report.CommittedRuntimeStillUsable && report.OriginalCheckpointUsable
	reportPath := filepath.Join(opts.OutputDir, "faults-report.json")
	report.ReportPath = reportPath
	if err := WriteJSON(reportPath, report); err != nil {
		return report, err
	}
	for _, item := range report.Cases {
		logf("%s expected=%s actual=%s", passFail(item.Passed), item.ExpectedCode, item.ActualCode)
	}
	logf("source-unchanged=%v runtime-still-usable=%v checkpoint-still-usable=%v",
		report.SourceUnchanged, report.CommittedRuntimeStillUsable, report.OriginalCheckpointUsable)
	if !report.AllExpected {
		return report, model.Err(model.EValidationFailed, "one or more fault injections did not produce the expected code",
			map[string]any{"cases": report.Cases})
	}
	return report, nil
}

func makeCase(name, expected string, err error) FaultCase {
	item := FaultCase{Name: name, ExpectedCode: expected, ActualCode: errorCode(err)}
	if err != nil {
		item.Details = err.Error()
		var target *model.BizError
		if asBizError(err, &target) {
			item.Context = target.Context
		}
	}
	item.Passed = item.ActualCode == expected
	return item
}

func asBizError(err error, target **model.BizError) bool {
	for err != nil {
		if be, ok := err.(*model.BizError); ok {
			*target = be
			return true
		}
		type unwrapper interface{ Unwrap() error }
		if u, ok := err.(unwrapper); ok {
			err = u.Unwrap()
			continue
		}
		break
	}
	return false
}

func mutateV1(original []byte, mutate func(map[string]any)) ([]byte, error) {
	var m map[string]any
	copied := append([]byte(nil), original...)
	if err := json.Unmarshal(copied, &m); err != nil {
		return nil, err
	}
	mutate(m)
	return json.Marshal(m)
}

func firstArrayMap(m map[string]any, key string) map[string]any {
	items, ok := m[key].([]any)
	if !ok || len(items) == 0 {
		panic(fmt.Sprintf("missing array %s", key))
	}
	value, ok := items[0].(map[string]any)
	if !ok {
		panic(fmt.Sprintf("array %s first item is not an object", key))
	}
	return value
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

func sameStringMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, want := range a {
		if b[key] != want {
			return false
		}
	}
	return true
}
