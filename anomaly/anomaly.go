package anomaly

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"serviceregistry/checkpoint"
	"serviceregistry/registry"
)

type Kind string

const (
	NegativeLease             Kind = "negative_lease"
	OutOfRangeLease           Kind = "out_of_range_lease"
	OutOfRangeHeartbeat       Kind = "out_of_range_heartbeat"
	NegativeHeartbeat         Kind = "negative_heartbeat"
	AmbiguousLeaseUnit        Kind = "ambiguous_lease_unit"
	AmbiguousThreshold        Kind = "ambiguous_threshold"
	DuplicateInstance         Kind = "duplicate_instance"
	InvalidConcurrencyBudget  Kind = "invalid_concurrency_budget"
	InvalidBufferBudget       Kind = "invalid_buffer_budget"
	TruncatedCheckpoint       Kind = "truncated_checkpoint"
	CheckpointVersionMismatch Kind = "checkpoint_version_mismatch"
)

type Input struct {
	Project        *registry.Project
	Documents      map[registry.SchemaVersion][]byte
	CheckpointPath string
}

type CaseResult struct {
	Kind            Kind                  `json:"kind"`
	ExpectedCode    string                `json:"expected_code"`
	ActualCode      string                `json:"actual_code"`
	Passed          bool                  `json:"passed"`
	Context         registry.ErrorContext `json:"context,omitempty"`
	SourceUnchanged bool                  `json:"source_unchanged"`
	CatalogUsable   bool                  `json:"catalog_usable"`
	Detail          string                `json:"detail,omitempty"`
}

func RunSuite(input Input) ([]CaseResult, error) {
	if input.Project == nil || len(input.Documents) == 0 {
		return nil, registry.NewError(registry.CodeEditInvalid, "anomaly suite requires a project and schema documents", registry.ErrorContext{})
	}
	docHashes := make(map[registry.SchemaVersion]string, len(input.Documents))
	for schema, data := range input.Documents {
		docHashes[schema] = hashBytes(data)
	}
	projectHash, err := input.Project.ModelHash()
	if err != nil {
		return nil, err
	}
	type loaderCase struct {
		kind     Kind
		schema   registry.SchemaVersion
		mutate   func([]byte) ([]byte, error)
		expected string
	}
	loaderCases := []loaderCase{
		{
			kind: NegativeLease, schema: registry.SchemaV1, expected: registry.CodeInvalidDuration,
			mutate: func(data []byte) ([]byte, error) {
				return mutateInstance(data, registry.SchemaV1, func(instance map[string]any) {
					instance["lease_ttl_ticks"] = int64(-1)
				})
			},
		},
		{
			kind: OutOfRangeHeartbeat, schema: registry.SchemaV2, expected: registry.CodeInvalidHeartbeat,
			mutate: func(data []byte) ([]byte, error) {
				return mutateInstance(data, registry.SchemaV2, func(instance map[string]any) {
					instance["heartbeatEveryMs"] = registry.MaxDurationMS + 1000
				})
			},
		},
		{
			kind: OutOfRangeLease, schema: registry.SchemaV1, expected: registry.CodeInvalidDuration,
			mutate: func(data []byte) ([]byte, error) {
				return mutateInstance(data, registry.SchemaV1, func(instance map[string]any) {
					instance["lease_ttl_ticks"] = registry.MaxDurationMS/100 + 1
				})
			},
		},
		{
			kind: NegativeHeartbeat, schema: registry.SchemaV2, expected: registry.CodeInvalidHeartbeat,
			mutate: func(data []byte) ([]byte, error) {
				return mutateInstance(data, registry.SchemaV2, func(instance map[string]any) {
					instance["heartbeatEveryMs"] = int64(-1)
				})
			},
		},
		{
			kind: AmbiguousLeaseUnit, schema: registry.SchemaV3, expected: registry.CodeLeaseUnitAmbiguous,
			mutate: func(data []byte) ([]byte, error) {
				return mutateInstance(data, registry.SchemaV3, func(instance map[string]any) {
					for _, key := range []string{
						"lease-ttl-seconds", "lease-ttl-millis", "lease-watermark-ms",
					} {
						delete(instance, key)
					}
					instance["lease-ttl"] = int64(5)
				})
			},
		},
		{
			kind: AmbiguousThreshold, schema: registry.SchemaV3, expected: registry.CodeThresholdAmbiguous,
			mutate: func(data []byte) ([]byte, error) {
				return mutateInstance(data, registry.SchemaV3, func(instance map[string]any) {
					delete(instance, "failure-threshold-remaining")
					delete(instance, "threshold-mode")
					instance["failure-threshold"] = int64(2)
				})
			},
		},
		{
			kind: DuplicateInstance, schema: registry.SchemaV2, expected: registry.CodeDuplicateInstance,
			mutate: func(data []byte) ([]byte, error) {
				root, err := decodeObject(data)
				if err != nil {
					return nil, err
				}
				service, err := firstObject(root["services"])
				if err != nil {
					return nil, err
				}
				instances, ok := service["instances"].([]any)
				if !ok || len(instances) == 0 {
					return nil, fmt.Errorf("v2 service has no instances")
				}
				copied, ok := cloneJSON(instances[0])
				if !ok {
					return nil, fmt.Errorf("cannot clone instance")
				}
				service["instances"] = append(instances, copied)
				return json.Marshal(root)
			},
		},
		{
			kind: InvalidConcurrencyBudget, schema: registry.SchemaV1, expected: registry.CodeInvalidBudget,
			mutate: func(data []byte) ([]byte, error) {
				root, err := decodeObject(data)
				if err != nil {
					return nil, err
				}
				budgets, ok := root["budgets"].(map[string]any)
				if !ok {
					return nil, fmt.Errorf("v1 budgets are missing")
				}
				budgets["global_concurrency"] = int64(0)
				return json.Marshal(root)
			},
		},
		{
			kind: InvalidBufferBudget, schema: registry.SchemaV1, expected: registry.CodeInvalidBudget,
			mutate: func(data []byte) ([]byte, error) {
				root, err := decodeObject(data)
				if err != nil {
					return nil, err
				}
				budgets, ok := root["budgets"].(map[string]any)
				if !ok {
					return nil, fmt.Errorf("v1 budgets are missing")
				}
				budgets["instance_buffer"] = int64(5000)
				return json.Marshal(root)
			},
		},
	}
	results := make([]CaseResult, 0, len(loaderCases)+2)
	for _, anomalyCase := range loaderCases {
		data, ok := input.Documents[anomalyCase.schema]
		if !ok {
			return nil, fmt.Errorf("missing %s document", anomalyCase.schema)
		}
		mutated, err := anomalyCase.mutate(data)
		if err != nil {
			return nil, err
		}
		_, loadErr := registry.LoadDocument(mutated)
		actual := registry.ErrorCode(loadErr)
		var context registry.ErrorContext
		if biz, ok := registry.AsError(loadErr); ok {
			context = biz.Context
		}
		catalogUsable := baselineUsable(input.Project)
		result := CaseResult{
			Kind:            anomalyCase.kind,
			ExpectedCode:    anomalyCase.expected,
			ActualCode:      actual,
			Passed:          actual == anomalyCase.expected && context.Path != "" && catalogUsable,
			Context:         context,
			SourceUnchanged: hashBytes(data) == docHashes[anomalyCase.schema] && hashProject(input.Project) == projectHash,
			CatalogUsable:   catalogUsable,
		}
		if loadErr != nil {
			result.Detail = loadErr.Error()
		}
		result.Passed = result.Passed && result.SourceUnchanged
		results = append(results, result)
	}
	truncatedResult := checkTruncatedCheckpoint(input.CheckpointPath)
	truncatedResult.SourceUnchanged = hashProject(input.Project) == projectHash
	truncatedResult.CatalogUsable = baselineUsable(input.Project)
	truncatedResult.Passed = truncatedResult.Passed && truncatedResult.SourceUnchanged && truncatedResult.CatalogUsable
	results = append(results, truncatedResult)
	mismatchResult := checkVersionMismatch(input.CheckpointPath, input.Project)
	mismatchResult.SourceUnchanged = hashProject(input.Project) == projectHash
	mismatchResult.CatalogUsable = baselineUsable(input.Project)
	mismatchResult.Passed = mismatchResult.Passed && mismatchResult.SourceUnchanged && mismatchResult.CatalogUsable
	results = append(results, mismatchResult)
	return results, nil
}

func checkTruncatedCheckpoint(path string) CaseResult {
	result := CaseResult{
		Kind:         TruncatedCheckpoint,
		ExpectedCode: registry.CodeCheckpointTruncated,
	}
	if path == "" {
		result.Detail = "checkpoint path is empty"
		return result
	}
	data, err := os.ReadFile(path)
	if err != nil {
		result.Detail = err.Error()
		return result
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".anomaly-truncated-*.db")
	if err != nil {
		result.Detail = err.Error()
		return result
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	cut := len(data) / 2
	if cut < 100 {
		cut = len(data) / 3
	}
	if _, err := temp.Write(data[:cut]); err != nil {
		_ = temp.Close()
		result.Detail = err.Error()
		return result
	}
	if err := temp.Close(); err != nil {
		result.Detail = err.Error()
		return result
	}
	_, err = (&checkpoint.Store{Path: tempPath}).Load()
	result.ActualCode = registry.ErrorCode(err)
	if biz, ok := registry.AsError(err); ok {
		result.Context = biz.Context
	}
	result.Passed = result.ActualCode == result.ExpectedCode && result.Context.Checkpoint != ""
	if err != nil {
		result.Detail = err.Error()
	}
	return result
}

func checkVersionMismatch(path string, project *registry.Project) CaseResult {
	result := CaseResult{
		Kind:         CheckpointVersionMismatch,
		ExpectedCode: registry.CodeCheckpointVersion,
	}
	if path == "" {
		result.Detail = "checkpoint path is empty"
		return result
	}
	_, status, err := (&checkpoint.Store{Path: path}).LoadCompatible(checkpoint.Identity{
		EngineVersion:  registry.EngineVersion,
		ProjectVersion: project.ProjectVersion,
		ModelHash:      "injected-version-mismatch",
	})
	if err != nil {
		result.ActualCode = registry.ErrorCode(err)
		if biz, ok := registry.AsError(err); ok {
			result.Context = biz.Context
		}
		result.Detail = err.Error()
		return result
	}
	result.ActualCode = status.Code
	result.Context = registry.ErrorContext{Checkpoint: path}
	result.Passed = status.Mode == checkpoint.ResumeModeFullRecompute && status.Code == result.ExpectedCode
	result.Detail = status.Reason
	return result
}

func baselineUsable(project *registry.Project) bool {
	zero := int64(0)
	run, err := registry.RunProject(project, registry.RunOptions{UntilTick: &zero})
	return err == nil && len(run.Steps) == 1 && len(run.Steps[0].Catalogs) > 0
}

func mutateInstance(data []byte, schema registry.SchemaVersion, mutate func(map[string]any)) ([]byte, error) {
	root, err := decodeObject(data)
	if err != nil {
		return nil, err
	}
	instance, err := instanceObject(root, schema)
	if err != nil {
		return nil, err
	}
	mutate(instance)
	return json.Marshal(root)
}

func instanceObject(root map[string]any, schema registry.SchemaVersion) (map[string]any, error) {
	switch schema {
	case registry.SchemaV1:
		rows, ok := root["instance_rows"].([]any)
		if !ok || len(rows) == 0 {
			return nil, fmt.Errorf("v1 instance_rows missing")
		}
		instance, ok := rows[0].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("v1 instance is not an object")
		}
		return instance, nil
	default:
		service, err := firstObject(root["services"])
		if err != nil {
			return nil, err
		}
		instances, ok := service["instances"].([]any)
		if !ok || len(instances) == 0 {
			return nil, fmt.Errorf("%s instances missing", schema)
		}
		instance, ok := instances[0].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s instance is not an object", schema)
		}
		return instance, nil
	}
}

func decodeObject(data []byte) (map[string]any, error) {
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	return root, nil
}

func firstObject(raw any) (map[string]any, error) {
	values, ok := raw.([]any)
	if !ok || len(values) == 0 {
		return nil, fmt.Errorf("array is missing or empty")
	}
	value, ok := values[0].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("first item is not an object")
	}
	return value, nil
}

func cloneJSON(value any) (any, bool) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	var cloned any
	if err := json.Unmarshal(data, &cloned); err != nil {
		return nil, false
	}
	return cloned, true
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hashProject(project *registry.Project) string {
	if project == nil {
		return ""
	}
	hash, err := project.ModelHash()
	if err != nil {
		return ""
	}
	return hash
}
