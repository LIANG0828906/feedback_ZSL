package downsample

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
)

const (
	InjectWindowWidthZero     = "window_width_zero"
	InjectRelativeOffset      = "relative_offset_out_of_range"
	InjectWeightScale         = "weight_scale_out_of_range"
	InjectFunctionCycle       = "function_cycle"
	InjectWorkerBudgetRange   = "worker_budget_out_of_range"
	InjectBufferBudgetRange   = "buffer_budget_out_of_range"
	InjectWorkerBackpressure  = "worker_backpressure"
	InjectBufferBackpressure  = "buffer_backpressure"
	InjectCheckpointTruncated = "checkpoint_truncated"
	InjectVersionMismatch     = "checkpoint_version_mismatch"
	InjectUnknownField        = "unknown_field"
	InjectStreamInterrupt     = "stream_interrupt"
)

func InjectionCases() []string {
	return []string{
		InjectWindowWidthZero,
		InjectRelativeOffset,
		InjectWeightScale,
		InjectFunctionCycle,
		InjectWorkerBudgetRange,
		InjectBufferBudgetRange,
		InjectWorkerBackpressure,
		InjectBufferBackpressure,
		InjectCheckpointTruncated,
		InjectVersionMismatch,
		InjectUnknownField,
		InjectStreamInterrupt,
	}
}

type InjectionResult struct {
	Case    string         `json:"case"`
	Code    string         `json:"code"`
	Version string         `json:"schema_version,omitempty"`
	Field   string         `json:"field_path,omitempty"`
	Message string         `json:"message"`
	Context map[string]any `json:"context,omitempty"`
}

func (e *Error) InjectionResult(caseName string) InjectionResult {
	if e == nil {
		return InjectionResult{Case: caseName, Code: CodeOK}
	}
	return InjectionResult{Case: caseName, Code: e.Code, Version: e.Version, Field: e.FieldPath, Message: e.Message, Context: e.Context}
}

// RunInjection executes a deterministic failure case. It only mutates parsed
// in-memory documents or files inside workDir; Bundle.Documents bytes are never
// written to by this function.
func RunInjection(bundle *SampleBundle, caseName, workDir string) *Error {
	switch caseName {
	case InjectWindowWidthZero:
		project, err := LoadProject(bundle.Documents[SchemaV1])
		if err != nil {
			return err
		}
		engine := NewEngine(project, DefaultBudget())
		_, err = engine.ApplyConfigEdit(ConfigEdit{MetricID: "cpu.usage", Field: "window_width", WidthMS: 0})
		return err

	case InjectRelativeOffset:
		doc := cloneDocumentMap(bundle.Documents[SchemaV2])
		metrics := doc["metrics"].([]any)
		metric := metrics[0].(map[string]any)
		points := metric["points"].([]any)
		point := points[0].(map[string]any)
		point["offsetWithinWindowMs"] = 60_001
		data, _ := json.Marshal(doc)
		_, err := LoadProject(data)
		return err

	case InjectWeightScale:
		doc := cloneDocumentMap(bundle.Documents[SchemaV1])
		metrics := doc["metrics"].([]any)
		metric := metrics[0].(map[string]any)
		metric["weight_scale"] = "permille_999"
		data, _ := json.Marshal(doc)
		_, err := LoadProject(data)
		return err

	case InjectFunctionCycle:
		doc := cloneDocumentMap(bundle.Documents[SchemaV1])
		table := doc["function_table"].(map[string]any)
		constants := table["constants"].([]any)
		constants = append(constants,
			map[string]any{"name": "cycle_a", "ref": "cycle_b"},
			map[string]any{"name": "cycle_b", "ref": "cycle_a"},
		)
		table["constants"] = constants
		data, _ := json.Marshal(doc)
		_, err := LoadProject(data)
		return err

	case InjectUnknownField:
		doc := cloneDocumentMap(bundle.Documents[SchemaV1])
		doc["unexpected_demo_field"] = true
		data, _ := json.Marshal(doc)
		_, err := LoadProject(data)
		return err

	case InjectWorkerBudgetRange:
		return Budget{MaxWorkers: 0, MaxWindowsPerMetric: 16}.Validate()

	case InjectBufferBudgetRange:
		return Budget{MaxWorkers: 2, MaxWindowsPerMetric: -4}.Validate()

	case InjectWorkerBackpressure:
		gate, err := NewGate(Budget{MaxWorkers: 1, MaxWindowsPerMetric: 4})
		if err != nil {
			return err
		}
		release, err := gate.TryAcquire("cpu.usage")
		if err != nil {
			return err
		}
		defer release()
		_, err = gate.TryAcquire("mem.usage")
		return err

	case InjectBufferBackpressure:
		gate, err := NewGate(Budget{MaxWorkers: 2, MaxWindowsPerMetric: 1})
		if err != nil {
			return err
		}
		release, err := gate.TryAcquire("cpu.usage")
		if err != nil {
			return err
		}
		defer release()
		_, err = gate.TryAcquire("cpu.usage")
		return err

	case InjectCheckpointTruncated:
		if err := os.MkdirAll(workDir, 0o755); err != nil {
			return WrapError(CodeCheckpointOpen, "cannot create injection directory", err).WithContext("dir", workDir)
		}
		path := filepath.Join(workDir, "truncated-checkpoint.sqlite")
		_ = os.Remove(path)
		cp, err := OpenCheckpoint(path)
		if err != nil {
			return err
		}
		if serr := cp.SaveCommit("cfg-v1", 3, nil); serr != nil {
			_ = cp.Close()
			return serr
		}
		_ = cp.Close()
		info, oerr := os.Stat(path)
		if oerr != nil {
			return WrapError(CodeCheckpointTruncated, "cannot stat checkpoint", oerr).WithContext("path", path)
		}
		if terr := os.Truncate(path, info.Size()/2); terr != nil {
			return WrapError(CodeCheckpointTruncated, "cannot truncate checkpoint", terr).WithContext("path", path)
		}
		_, err = OpenCheckpoint(path)
		if err == nil {
			return NewError(CodeCheckpointTruncated, "truncated checkpoint unexpectedly opened").WithContext("path", path)
		}
		return err

	case InjectVersionMismatch:
		if err := os.MkdirAll(workDir, 0o755); err != nil {
			return WrapError(CodeCheckpointOpen, "cannot create injection directory", err)
		}
		path := filepath.Join(workDir, "version-mismatch.sqlite")
		_ = os.Remove(path)
		cp, err := OpenCheckpoint(path)
		if err != nil {
			return err
		}
		if serr := cp.SaveCommit("cfg-old", 7, nil); serr != nil {
			_ = cp.Close()
			return serr
		}
		_ = cp.Close()
		cp2, oerr := OpenCheckpoint(path)
		if oerr != nil {
			return oerr
		}
		defer cp2.Close()
		_, rerr := cp2.Recover("cfg-v9")
		return rerr

	case InjectStreamInterrupt:
		if err := os.MkdirAll(workDir, 0o755); err != nil {
			return WrapError(CodeCheckpointOpen, "cannot create injection directory", err)
		}
		path := filepath.Join(workDir, "signal.sqlite")
		session, err := NewSession(bundle.Project, DefaultBudget(), path)
		if err != nil {
			return err
		}
		defer session.Close()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, ierr := session.AppendAndCheckpoint(ctx, Point{MetricID: "cpu.usage", TimestampMS: bundle.BaseTimeMS + 8*60_000, Value: 99, Weight: 10000})
		return ierr

	default:
		return NewError(CodeInjectionUnknown, "unknown injection case").
			WithContext("case", caseName).
			WithContext("allowed", InjectionCases())
	}
}

func cloneDocumentMap(data []byte) map[string]any {
	var doc map[string]any
	_ = json.Unmarshal(data, &doc)
	cloned, _ := json.Marshal(doc)
	var out map[string]any
	_ = json.Unmarshal(cloned, &out)
	return out
}
