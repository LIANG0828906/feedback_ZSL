package exceptions

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"samplingkernel/internal/errs"
	"samplingkernel/internal/loader"
	"samplingkernel/internal/model"
	"samplingkernel/internal/projectvalidate"
	"samplingkernel/internal/runtime"
	"samplingkernel/internal/store"
)

type CaseResult struct {
	Name    string `json:"name"`
	Code    string `json:"code"`
	Path    string `json:"path,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Message string `json:"message"`
}

func All(ctx context.Context, base *model.Project, v1Doc []byte, stateDir string) ([]CaseResult, error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, err
	}
	results := []CaseResult{
		injectRate(base),
		injectTailCycle(base),
		injectAllocationSum(base),
		injectMissingService(base),
		injectWorkerBudget(),
		injectBufferBudget(),
		injectUnknownField(v1Doc),
	}
	truncated, err := injectTruncatedCheckpoint(ctx, filepath.Join(stateDir, "truncated.db"))
	if err != nil {
		return nil, err
	}
	results = append(results, truncated)
	versionMismatch, err := injectVersionMismatch(ctx, filepath.Join(stateDir, "version-mismatch.db"))
	if err != nil {
		return nil, err
	}
	results = append(results, versionMismatch)
	return results, nil
}

func codedResult(name string, err error) CaseResult {
	coded, ok := errs.As(err)
	if !ok {
		return CaseResult{Name: name, Code: "E_UNEXPECTED", Message: err.Error()}
	}
	return CaseResult{Name: name, Code: coded.Code, Path: coded.Path, Detail: coded.Detail, Message: coded.Message}
}

func injectRate(base *model.Project) CaseResult {
	p, err := clone(base)
	if err != nil {
		return codedResult("rate_out_of_range", err)
	}
	p.Edges[0].BaseRatePermyriad = 10001
	return codedResult("rate_out_of_range", projectvalidate.Validate(p))
}

func injectTailCycle(base *model.Project) CaseResult {
	p, err := clone(base)
	if err != nil {
		return codedResult("tail_condition_cycle", err)
	}
	for i := range p.TailRules {
		switch p.TailRules[i].ID {
		case "tr-error":
			p.TailRules[i].DependsOn = []string{"tr-latency"}
		case "tr-latency":
			p.TailRules[i].DependsOn = []string{"tr-error"}
		}
	}
	return codedResult("tail_condition_cycle", projectvalidate.Validate(p))
}

func injectAllocationSum(base *model.Project) CaseResult {
	p, err := clone(base)
	if err != nil {
		return codedResult("allocation_sum_exceeded", err)
	}
	p.Allocations[0].WeightPermyriad += 1
	return codedResult("allocation_sum_exceeded", projectvalidate.Validate(p))
}

func injectMissingService(base *model.Project) CaseResult {
	p, err := clone(base)
	if err != nil {
		return codedResult("edge_service_missing", err)
	}
	p.Edges[0].ToServiceID = "missing-service"
	return codedResult("edge_service_missing", projectvalidate.Validate(p))
}

func injectWorkerBudget() CaseResult {
	_, err := runtime.NewScheduler(runtime.Options{Workers: 128, BufferCap: 16})
	return codedResult("worker_budget_out_of_bounds", err)
}

func injectBufferBudget() CaseResult {
	_, err := runtime.NewScheduler(runtime.Options{Workers: 2, BufferCap: 99999})
	return codedResult("buffer_budget_out_of_bounds", err)
}

func injectUnknownField(v1Doc []byte) CaseResult {
	var raw map[string]any
	if err := json.Unmarshal(v1Doc, &raw); err != nil {
		return codedResult("unknown_schema_field", err)
	}
	raw["unexpected_diagnostic_field"] = "boom"
	data, err := json.Marshal(raw)
	if err != nil {
		return codedResult("unknown_schema_field", err)
	}
	_, loadErr := loader.LoadVersion(model.SchemaV1, data)
	return codedResult("unknown_schema_field", loadErr)
}

func injectTruncatedCheckpoint(ctx context.Context, path string) (CaseResult, error) {
	_ = os.Remove(path)
	st, err := store.Open(path)
	if err != nil {
		return codedResult("truncated_checkpoint", err), nil
	}
	err = st.SaveProgress(ctx, store.Checkpoint{
		ProjectID: "exception-demo", ProjectVersion: model.SchemaV1, LastSeq: 3,
		Traces: []store.TraceRecord{{TraceID: "tr-committed", Status: "complete", LastEventSeq: 3, MergedCount: 1}},
	})
	_ = st.Close()
	if err != nil {
		return codedResult("truncated_checkpoint", err), nil
	}
	if err := os.Truncate(path, 128); err != nil {
		return CaseResult{}, err
	}
	st, err = store.Open(path)
	if err != nil {
		return codedResult("truncated_checkpoint", err), nil
	}
	defer st.Close()
	_, loadErr := st.Load(ctx, model.SchemaV1)
	result := codedResult("truncated_checkpoint", loadErr)
	if loadErr == nil {
		result = CaseResult{Name: "truncated_checkpoint", Code: "E_CHECKPOINT_NOT_DETECTED", Message: "truncation was not detected"}
	}
	return result, nil
}

func injectVersionMismatch(ctx context.Context, path string) (CaseResult, error) {
	_ = os.Remove(path)
	st, err := store.Open(path)
	if err != nil {
		return CaseResult{}, err
	}
	defer st.Close()
	if err := st.SaveProgress(ctx, store.Checkpoint{
		ProjectID: "exception-demo", ProjectVersion: model.SchemaV1, LastSeq: 7,
		Traces: []store.TraceRecord{{TraceID: "tr-stable", Status: "complete", LastEventSeq: 7, MergedCount: 2}},
	}); err != nil {
		return CaseResult{}, err
	}
	_, loadErr := st.Load(ctx, model.SchemaV2)
	result := codedResult("version_mismatch_snapshot", loadErr)
	committed, readErr := st.CompletedTraceJSON(ctx)
	if readErr == nil {
		if _, ok := committed["tr-stable"]; ok {
			result.Detail += "; committed_trace_still_readable=true"
		}
	}
	return result, nil
}

func clone(p *model.Project) (*model.Project, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	var out model.Project
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
