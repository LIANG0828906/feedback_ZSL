package engine

import (
	"fmt"

	"maskpipeline/internal/apperr"
	"maskpipeline/internal/model"
)

func paramError(field, format string, args ...any) error {
	return apperr.New(apperr.CodePara, "engine_options", format, args...).WithField(field)
}

func validateOptions(o ExecutorOptions) error {
	if o.MaxParallel < 1 || o.MaxParallel > 64 {
		return paramError("max_parallel", "max parallel must be in [1,64]")
	}
	if o.PerBlockBuffer < 1 || o.PerBlockBuffer > 1024 {
		return paramError("per_block_buffer", "per block buffer must be in [1,1024]")
	}
	if o.MaxStageBytes < 1 {
		return paramError("max_stage_bytes", "max stage bytes must be positive")
	}
	if o.PreviewIterationBudget < 0 {
		return paramError("preview_iteration_budget", "preview iteration budget cannot be negative")
	}
	if o.MaxAreaRatio <= 0 {
		return paramError("max_area_ratio", "max area ratio must be positive")
	}
	if o.OutsideTruthRatio < 0 || o.OutsideTruthRatio > 1 {
		return paramError("outside_truth_ratio", "outside truth ratio must be in [0,1]")
	}
	if o.InjectBadStage != "" && !o.InjectBadStage.Valid() {
		return paramError("inject_bad_stage", "unknown injected stage")
	}
	return nil
}

func blockError(tile int, stage model.Stage, message string) *apperr.Error {
	return apperr.New(apperr.CodeBlock, "process_block", message).
		WithDetail("tile", tile).
		WithDetail("stage", stage)
}

func stageName(stage model.Stage) string { return string(stage) }

func joinErrors(errors []error) string {
	if len(errors) == 0 {
		return ""
	}
	if len(errors) == 1 {
		return errors[0].Error()
	}
	return fmt.Sprintf("%s (and %d more)", errors[0], len(errors)-1)
}
