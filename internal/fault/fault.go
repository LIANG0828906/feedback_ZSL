package fault

import (
	"encoding/json"
	"os"
	"path/filepath"

	"grayrelease/internal/checkpoint"
	"grayrelease/internal/loader"
	"grayrelease/internal/model"
	"grayrelease/internal/sample"
	"grayrelease/internal/scheduler"
)

func Inject(kind, workdir string) *model.BizError {
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return model.NewError("E_INJECTION_FAILED", "cannot create injection directory").With("path", workdir).With("cause", err.Error())
	}
	switch kind {
	case "overlap", "overlap-rule":
		return injectOverlap(workdir)
	case "bad-scale", "out-of-range-scale":
		return injectBadScale(workdir)
	case "missing-service", "rule-missing-service":
		return injectMissingService(workdir)
	case "ambiguous-scale", "ambiguous-fields":
		return injectAmbiguousScale(workdir)
	case "truncated-checkpoint", "truncate-checkpoint":
		return injectTruncatedCheckpoint(workdir)
	case "version-mismatch", "mismatch-snapshot":
		return injectVersionMismatch(workdir)
	case "worker-budget":
		return model.NewError("E_WORKER_BUDGET", "declared worker budget is exhausted or invalid").
			With("worker_budget", 0).With("injection", kind)
	case "worker-budget-exhausted", "workers-busy":
		gate, err := scheduler.NewGate(1)
		if err != nil {
			return toInjectionError(err)
		}
		if !gate.TryAcquire() {
			return model.NewError(scheduler.CodeWorkerBudgetExhausted, "worker budget was already exhausted")
		}
		if gate.TryAcquire() {
			return model.NewError("E_EXPECTED_BUSINESS_ERROR", "second worker permit should not be available")
		}
		return model.NewError(scheduler.CodeWorkerBudgetExhausted, "all routing workers are busy").
			With("worker_budget", 1).With("active_workers", 1).With("injection", kind)
	default:
		return model.NewError("E_INJECTION_KIND_UNKNOWN", "unknown fault injection kind").With("kind", kind)
	}
}

func writeInjectedJSON(workdir, name string, value map[string]any) (string, *model.BizError) {
	path := filepath.Join(workdir, name)
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", model.NewError("E_INJECTION_FAILED", "marshal injected document failed").With("cause", err.Error())
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", model.NewError("E_INJECTION_FAILED", "write injected document failed").With("path", path).With("cause", err.Error())
	}
	return path, nil
}

func sampleV2Map() (map[string]any, error) {
	project := sample.CanonicalProject()
	data, err := loader.MarshalProject(project, model.SchemaV2)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func injectOverlap(workdir string) *model.BizError {
	value, err := sampleV2Map()
	if err != nil {
		return model.NewError("E_INJECTION_FAILED", err.Error())
	}
	tree, _ := value["ruleTree"].([]any)
	for _, groupRaw := range tree {
		group := groupRaw.(map[string]any)
		if group["service"] != "checkout" {
			continue
		}
		rules := group["rules"].([]any)
		rules = append(rules, map[string]any{
			"id": "R-INJECT-OVERLAP", "ruleNo": 999, "priority": 10, "declarationOrder": 1,
			"match":     map[string]any{"labels": map[string]any{}},
			"weightBps": 1000,
		})
		group["rules"] = rules
	}
	path, bizErr := writeInjectedJSON(workdir, "overlap-rule.json", value)
	if bizErr != nil {
		return bizErr
	}
	_, loadErr := loader.LoadProject(readFileOrEmpty(path), loader.LoadOptions{StrictOverlap: true})
	if be, ok := model.AsBizError(loadErr); ok {
		return be.With("injected_artifact", path).With("source_project_unchanged", true)
	}
	return model.NewError("E_EXPECTED_BUSINESS_ERROR", "overlap injection was accepted").With("path", path)
}

func readFileOrEmpty(path string) []byte {
	data, _ := os.ReadFile(path)
	return data
}

func injectBadScale(workdir string) *model.BizError {
	project := sample.CanonicalProject()
	data, err := loader.MarshalProject(project, model.SchemaV1)
	if err != nil {
		return model.NewError("E_INJECTION_FAILED", err.Error())
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		return model.NewError("E_INJECTION_FAILED", err.Error())
	}
	rules := value["gray_rules"].([]any)
	rule := rules[0].(map[string]any)
	rule["weight_permille"] = 1001
	path, bizErr := writeInjectedJSON(workdir, "bad-scale.json", value)
	if bizErr != nil {
		return bizErr
	}
	_, loadErr := loader.LoadProjectFile(path)
	if be, ok := model.AsBizError(loadErr); ok {
		return be.With("injected_artifact", path).With("source_project_unchanged", true)
	}
	return model.NewError("E_EXPECTED_BUSINESS_ERROR", "bad scale was accepted").With("path", path)
}

func injectMissingService(workdir string) *model.BizError {
	value, err := sampleV2Map()
	if err != nil {
		return model.NewError("E_INJECTION_FAILED", err.Error())
	}
	value["ruleTree"] = append(value["ruleTree"].([]any), map[string]any{
		"service": "ghost-service",
		"rules": []any{map[string]any{
			"id": "R-INJECT-MISSING", "ruleNo": 998, "priority": 1, "declarationOrder": 0,
			"match": map[string]any{"labels": map[string]any{}}, "weightBps": 100,
		}},
	})
	path, bizErr := writeInjectedJSON(workdir, "missing-service.json", value)
	if bizErr != nil {
		return bizErr
	}
	_, loadErr := loader.LoadProjectFile(path)
	if be, ok := model.AsBizError(loadErr); ok {
		return be.With("injected_artifact", path).With("source_project_unchanged", true)
	}
	return model.NewError("E_EXPECTED_BUSINESS_ERROR", "missing service was accepted").With("path", path)
}

func injectAmbiguousScale(workdir string) *model.BizError {
	project := sample.CanonicalProject()
	data, err := loader.MarshalProject(project, model.SchemaV1)
	if err != nil {
		return model.NewError("E_INJECTION_FAILED", err.Error())
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		return model.NewError("E_INJECTION_FAILED", err.Error())
	}
	rules := value["gray_rules"].([]any)
	rule := rules[0].(map[string]any)
	rule["weight_bps"] = 9999
	path, bizErr := writeInjectedJSON(workdir, "ambiguous-scale.json", value)
	if bizErr != nil {
		return bizErr
	}
	_, loadErr := loader.LoadProjectFile(path)
	if be, ok := model.AsBizError(loadErr); ok {
		return be.With("injected_artifact", path).With("source_project_unchanged", true)
	}
	return model.NewError("E_EXPECTED_BUSINESS_ERROR", "ambiguous scale was accepted").With("path", path)
}

func injectTruncatedCheckpoint(workdir string) *model.BizError {
	path := filepath.Join(workdir, "truncated-checkpoint.db")
	_ = os.Remove(path)
	store, err := checkpoint.OpenStore(path, sample.EngineeringVersion)
	if err != nil {
		return toInjectionError(err)
	}
	if err := store.CommitTick(2); err != nil {
		_ = store.Close()
		return toInjectionError(err)
	}
	if err := store.Close(); err != nil {
		return toInjectionError(err)
	}
	if err := os.Truncate(path, 64); err != nil {
		return model.NewError("E_INJECTION_FAILED", "truncate checkpoint failed").With("cause", err.Error())
	}
	_, openErr := checkpoint.OpenStore(path, sample.EngineeringVersion)
	if be, ok := model.AsBizError(openErr); ok {
		return be.With("injected_artifact", path).With("truncated_bytes", 64)
	}
	return model.NewError("E_EXPECTED_BUSINESS_ERROR", "truncated checkpoint was accepted").With("path", path)
}

func injectVersionMismatch(workdir string) *model.BizError {
	path := filepath.Join(workdir, "version-mismatch.db")
	_ = os.Remove(path)
	store, err := checkpoint.OpenStore(path, "eng-old-snapshot")
	if err != nil {
		return toInjectionError(err)
	}
	if err := store.CommitTick(1); err != nil {
		_ = store.Close()
		return toInjectionError(err)
	}
	_ = store.Close()
	_, openErr := checkpoint.OpenStore(path, sample.EngineeringVersion)
	if be, ok := model.AsBizError(openErr); ok {
		return be.With("injected_artifact", path)
	}
	return model.NewError("E_EXPECTED_BUSINESS_ERROR", "version mismatch was accepted").With("path", path)
}

func toInjectionError(err error) *model.BizError {
	if be, ok := model.AsBizError(err); ok {
		return be
	}
	return model.NewError("E_INJECTION_FAILED", err.Error())
}
