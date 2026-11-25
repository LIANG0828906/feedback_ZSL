package fogkernel

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
)

type InjectionRequest struct {
	Kind    string `json:"kind"`
	Workers int    `json:"workers,omitempty"`
	Buffer  int    `json:"buffer,omitempty"`
	Blocks  int    `json:"blocks,omitempty"`
}

type InjectionResult struct {
	Kind               string           `json:"kind"`
	ExpectedErrorCode  string           `json:"expected_error_code,omitempty"`
	Error              *BizError        `json:"error,omitempty"`
	Audit              *ConversionAudit `json:"audit,omitempty"`
	Detail             any              `json:"detail,omitempty"`
	SourceHash         string           `json:"source_hash,omitempty"`
	ExistingRayResults int              `json:"existing_ray_results,omitempty"`
}

func mutateV1Doc(bundle *GeneratedBundle, mutate func(*dtoV1)) ([]byte, error) {
	var doc dtoV1
	if err := json.Unmarshal(bundle.Docs[SchemaV1], &doc); err != nil {
		return nil, err
	}
	mutate(&doc)
	return json.Marshal(doc)
}

// RunInjection intentionally operates on a copied/mutated DTO or a temporary
// checkpoint. It never mutates bundle.Docs or the active incremental scene.
func RunInjection(bundle *GeneratedBundle, req InjectionRequest) (*InjectionResult, error) {
	result := &InjectionResult{Kind: req.Kind}
	switch req.Kind {
	case "density_out_of_range":
		data, err := mutateV1Doc(bundle, func(d *dtoV1) { d.Volumes[0].DensityValues[0] = 10_000 })
		if err != nil {
			return nil, err
		}
		_, loadErr := LoadProject(data)
		return injectionBiz(result, CodeDensityRange, loadErr)

	case "density_shape_mismatch":
		data, err := mutateV1Doc(bundle, func(d *dtoV1) { d.Volumes[0].Size[0] = 99 })
		if err != nil {
			return nil, err
		}
		_, loadErr := LoadProject(data)
		return injectionBiz(result, CodeDensityShape, loadErr)

	case "negative_coefficient":
		data, err := mutateV1Doc(bundle, func(d *dtoV1) { d.Volumes[0].SigmaAPermille = -42 })
		if err != nil {
			return nil, err
		}
		_, loadErr := LoadProject(data)
		return injectionBiz(result, CodeCoeffNegative, loadErr)

	case "distance_overflow":
		data, err := mutateV1Doc(bundle, func(d *dtoV1) { d.Volumes[0].BoundsMinMM[0] = math.MaxInt64 })
		if err != nil {
			return nil, err
		}
		_, loadErr := LoadProject(data)
		return injectionBiz(result, CodeFixedOverflow, loadErr)

	case "unknown_schema":
		data, err := mutateV1Doc(bundle, func(d *dtoV1) { d.Schema = "fog.project/v9+unknown" })
		if err != nil {
			return nil, err
		}
		_, loadErr := LoadProject(data)
		return injectionBiz(result, CodeSchemaVersion, loadErr)

	case "mixed_schema_combination":
		var raw map[string]any
		if err := json.Unmarshal(bundle.Docs[SchemaV1], &raw); err != nil {
			return nil, err
		}
		raw["schema_version"] = SchemaV2
		data, _ := json.Marshal(raw)
		_, loadErr := LoadProject(data)
		return injectionBiz(result, CodeSchemaCombination, loadErr)

	case "budget_workers":
		workers := req.Workers
		if workers == 0 {
			workers = MaxDeclaredWorkers + 1
		}
		_, err := Budget{Workers: workers, PerWorkerBuffer: 16}.Validate()
		return injectionBiz(result, CodeBudgetWorkers, err)

	case "budget_buffer":
		buffer := req.Buffer
		if buffer == 0 {
			buffer = MaxDeclaredBuffer + 1
		}
		_, err := Budget{Workers: 2, PerWorkerBuffer: buffer}.Validate()
		return injectionBiz(result, CodeBudgetBuffer, err)

	case "backpressure":
		workers := req.Workers
		if workers == 0 {
			workers = 1
		}
		buffer := req.Buffer
		if buffer == 0 {
			buffer = 1
		}
		blocks := req.Blocks
		if blocks == 0 {
			blocks = workers*buffer + 7
		}
		err := AdmitBlockBatch(Budget{Workers: workers, PerWorkerBuffer: buffer}, blocks)
		return injectionBiz(result, CodeBackpressure, err)

	case "audit_tolerance_breach":
		audit := ConversionAudit{
			ID: "injected-tau-audit", Kind: "optical_thickness", SourceVersion: "injected",
			Tau: 0.500001, CanonicalFloat: 0.5, DifferenceQ: 50_000, ToleranceQ: 1_000,
			WithinTolerance: false, StatusCode: CodeAuditTolerance,
			Message: "declared fixed-point tolerance exceeded; refusing silent rounding",
		}
		result.Audit = &audit
		biz := NewBizError(CodeAuditTolerance, "$.audit.injected-tau-audit", audit.Message).
			WithContext("difference_q", audit.DifferenceQ, "tolerance_q", audit.ToleranceQ)
		result.Error = biz
		result.ExpectedErrorCode = CodeAuditTolerance
		return result, biz

	case "truncated_checkpoint":
		path := filepath.Join(os.TempDir(), "fog-inject-truncated.sqlite")
		store, err := CreateFreshCheckpoint(path, bundle.Canonical.Version())
		if err != nil {
			return nil, err
		}
		if err := store.BeginRay(0, "r-inject"); err != nil {
			_ = store.Close()
			return nil, err
		}
		if err := store.Close(); err != nil {
			return nil, err
		}
		if err := os.Truncate(path, 48); err != nil {
			return nil, err
		}
		_, inspectErr := InspectCheckpoint(path, bundle.Canonical.Version())
		result.Detail = map[string]string{"path": path}
		return injectionBiz(result, CodeCheckpointTrunc, inspectErr)

	case "version_mismatch_checkpoint":
		path := filepath.Join(os.TempDir(), "fog-inject-version.sqlite")
		store, err := CreateFreshCheckpoint(path, "scene-injected-old-version")
		if err != nil {
			return nil, err
		}
		if err := store.Close(); err != nil {
			return nil, err
		}
		state, inspectErr := InspectCheckpoint(path, bundle.Canonical.Version())
		result.Detail = state
		return injectionBiz(result, CodeCheckpointVersion, inspectErr)

	default:
		biz := NewBizError(CodeInjectionUnknown, "$.kind", "unknown injection %q", req.Kind).
			WithContext("known", []string{
				"density_out_of_range", "density_shape_mismatch", "negative_coefficient",
				"distance_overflow", "unknown_schema", "mixed_schema_combination",
				"budget_workers", "budget_buffer", "backpressure", "audit_tolerance_breach",
				"truncated_checkpoint", "version_mismatch_checkpoint",
			})
		result.Error = biz
		result.ExpectedErrorCode = CodeInjectionUnknown
		return result, biz
	}
}

func injectionBiz(result *InjectionResult, expected string, err error) (*InjectionResult, error) {
	biz, ok := AsBizError(err)
	if !ok {
		biz = NewBizError(expected, "$.injection", "%v", err)
	}
	result.Error = biz
	result.ExpectedErrorCode = expected
	return result, biz
}
