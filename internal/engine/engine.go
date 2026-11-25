package engine

import (
	"context"
	"sort"
	"strconv"

	"featurekernel/internal/kernel"
)

type Engine struct {
	project *kernel.Project
}

func New(project *kernel.Project) (*Engine, error) {
	if project == nil {
		return nil, kernel.NewError(kernel.CodeInvalidArgument, "project is required", nil)
	}
	if err := kernel.ValidateBudget(project.Budget); err != nil {
		return nil, err
	}
	for _, feature := range project.Features {
		if err := kernel.ValidateFeature(feature); err != nil {
			return nil, err
		}
	}
	return &Engine{project: project}, nil
}

func (e *Engine) Run(ctx context.Context) (*kernel.ResultSet, error) {
	return e.RunWithPath(ctx, "")
}

func (e *Engine) RunWithPath(ctx context.Context, forcePath string) (*kernel.ResultSet, error) {
	featureCount := len(e.project.Features)
	outputs := make([]*FeatureOutput, featureCount)
	tasks := make([]func(int) error, featureCount)
	for i := range e.project.Features {
		index := i
		tasks[index] = func(int) error {
			select {
			case <-ctx.Done():
				return kernel.WrapError(kernel.CodeInvalidArgument, ctx.Err(), map[string]any{"stage": "engine_cancelled"})
			default:
			}
			output, err := ProcessFeature(e.project, e.project.Features[index], forcePath)
			if err != nil {
				return err
			}
			outputs[index] = output
			return nil
		}
	}

	errs := RunParallel(tasks, e.project.Budget.Workers)
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}

	result := &kernel.ResultSet{
		ProjectID:        e.project.ProjectID,
		BatchID:          e.project.BatchID,
		CanonicalVersion: kernel.CanonicalVersion,
		Windows:          []kernel.WindowResult{},
		Derived:          []kernel.DerivedResult{},
		Recomputations:   []kernel.Recomputation{},
		Discarded:        []kernel.DiscardedRecord{},
		PathDecisions:    []kernel.PathDecision{},
		Diagnostics:      []kernel.FeatureDiagnostic{},
		WatermarkStatus:  e.project.WatermarkPolicy,
	}

	for _, output := range outputs {
		if output == nil {
			continue
		}
		result.Windows = append(result.Windows, output.Results...)
		result.Recomputations = append(result.Recomputations, output.Recomputations...)
		result.Discarded = append(result.Discarded, output.Discarded...)
		result.PathDecisions = append(result.PathDecisions, output.Decision)
		result.Diagnostics = append(result.Diagnostics, output.Diagnostic)
	}

	sort.Slice(result.Recomputations, func(i, j int) bool {
		a, b := result.Recomputations[i], result.Recomputations[j]
		if a.FeatureID != b.FeatureID {
			return a.FeatureID < b.FeatureID
		}
		if a.WindowIndex != b.WindowIndex {
			return a.WindowIndex < b.WindowIndex
		}
		return a.TriggerSeq < b.TriggerSeq
	})
	sort.Slice(result.Discarded, func(i, j int) bool {
		a, b := result.Discarded[i], result.Discarded[j]
		if a.FeatureID != b.FeatureID {
			return a.FeatureID < b.FeatureID
		}
		if a.ArrivalSeq != b.ArrivalSeq {
			return a.ArrivalSeq < b.ArrivalSeq
		}
		return a.WindowIndex < b.WindowIndex
	})

	windowIDs := make([]string, 0, len(result.Recomputations))
	for _, recomputation := range result.Recomputations {
		windowIDs = append(windowIDs, recomputation.WindowID)
	}
	result.RecomputedWindows = kernel.UniqueSortedStrings(windowIDs)

	derived, err := buildDerived(e.project, result.Windows)
	if err != nil {
		return nil, err
	}
	result.Derived = derived
	return result, nil
}

func buildDerived(project *kernel.Project, windows []kernel.WindowResult) ([]kernel.DerivedResult, error) {
	return BuildDerived(project, windows)
}

func BuildDerived(project *kernel.Project, windows []kernel.WindowResult) ([]kernel.DerivedResult, error) {
	byGeometry := map[string]kernel.WindowResult{}
	for _, window := range windows {
		key := geometryKey(window.FeatureID, window.StartMs, window.EndMs)
		byGeometry[key] = window
	}

	out := []kernel.DerivedResult{}
	for _, spec := range project.Derived {
		var base *kernel.WindowResult
		for i := range windows {
			if windows[i].FeatureID == spec.BaseFeatureID {
				base = &windows[i]
				numeratorKey := geometryKey(spec.NumeratorFeature, base.StartMs, base.EndMs)
				denominatorKey := geometryKey(spec.DenominatorFeature, base.StartMs, base.EndMs)
				numerator, okNum := byGeometry[numeratorKey]
				denominator, okDen := byGeometry[denominatorKey]
				numeratorValue := 0.0
				if okNum {
					numeratorValue = aggValue(numerator.Aggregations, kernel.AggSum)
				}
				denominatorValue := 0.0
				if okDen {
					denominatorValue = aggValue(denominator.Aggregations, kernel.AggSum)
				}
				value := 0.0
				if denominatorValue != 0 {
					switch spec.Expression {
					case "sum_ratio", "ratio":
						value = numeratorValue / denominatorValue
					default:
						return nil, kernel.NewError(kernel.CodeInvalidArgument, "unsupported derived expression", map[string]any{"expression": spec.Expression})
					}
				}
				out = append(out, kernel.DerivedResult{
					DerivedID: spec.ID, WindowID: spec.ID + ":" + base.WindowID, WindowIndex: base.WindowIndex,
					StartMs: base.StartMs, EndMs: base.EndMs, Value: value,
					Dependencies: []string{numeratorKey, denominatorKey},
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DerivedID != out[j].DerivedID {
			return out[i].DerivedID < out[j].DerivedID
		}
		return out[i].WindowIndex < out[j].WindowIndex
	})
	return out, nil
}

func geometryKey(featureID string, start, end int64) string {
	return featureID + "|" + strconv.FormatInt(start, 10) + "|" + strconv.FormatInt(end, 10)
}

func aggValue(values []kernel.AggregationValue, fn kernel.AggFunc) float64 {
	for _, value := range values {
		if value.Func == fn {
			return value.Value
		}
	}
	return 0
}
