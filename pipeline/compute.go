package pipeline

import (
	"fmt"

	"iconflow/errs"
	"iconflow/graph"
	"iconflow/model"
	"iconflow/segment"
	"iconflow/state"
)

func versionsFor(project *model.Project, params EffectiveParams) (thresholdVersion, quantVersion, orderHash string) {
	thresholdVersion = graph.SHA256Hex(
		"threshold_permyriad", fmt.Sprint(params.ThresholdPermyriad),
		"param_version", project.ParamVersion,
	)
	quantVersion = graph.SHA256Hex(
		"quant_step", fmt.Sprint(params.Quant.Step),
		"rounding", params.Quant.Rounding,
		"extent_divisible", fmt.Sprint(params.Quant.ExtentDivisible),
		"rules", graph.HashJSON(project.Rules),
	)
	orderHash = graph.OrderHash(params.ChannelOrder, project.ChannelLayout)
	return thresholdVersion, quantVersion, orderHash
}

func ComputeBlock(project *model.Project, coord model.BlockCoord, maxBufferPixels int) (*BlockResult, *JobError) {
	params := Effective(project, coord.Key())
	inputHash := graph.BlockInputHash(project.ImageByID(coord.ImageID), coord)
	keys := KeysFor(project, coord, inputHash)
	rules := project.Rules
	rules.QuantilePermyriad = params.ThresholdPermyriad
	rules.OperatorVersion = params.OperatorVersion

	gradient, gerr := segment.ComputeGradient(
		project.ImageByID(coord.ImageID), coord, params.ChannelOrder,
		params.Quant, params.OperatorVersion, maxBufferPixels)
	if gerr != nil {
		return nil, &JobError{Coord: coord, Stage: state.StageGradient, Err: gerr}
	}

	threshold, terr := segment.SelectThreshold(gradient, rules)
	if terr != nil {
		result := &BlockResult{
			Coord: coord, InputHash: inputHash,
			Keys:     StageKeys{Gradient: keys.Gradient, Threshold: keys.Threshold},
			Gradient: gradient, Threshold: threshold,
		}
		return result, &JobError{Coord: coord, Stage: state.StageThreshold, Err: terr}
	}

	markers := segment.MarkForeground(gradient, threshold)
	stats := segment.Analyze(markers, coord, rules)
	validationError := segment.ValidateBlock(stats, project.Truth[coord.Key()], rules.Tolerances)

	return &BlockResult{
		Coord: coord, InputHash: inputHash, Keys: keys,
		Gradient: gradient, Threshold: threshold, Markers: markers,
		Stats: stats, ValidationError: validationError,
		StageSeqs: map[string]int64{},
	}, nil
}

func KeysFor(project *model.Project, coord model.BlockCoord, inputHash string) StageKeys {
	params := Effective(project, coord.Key())
	thresholdVersion, quantVersion, orderHash := versionsFor(project, params)
	gradientKey := graph.StageCacheKey(graph.CacheKeyInput{
		Stage: "gradient", InputHash: inputHash,
		QuantVersion: quantVersion,
		OrderHash:    orderHash, AlgoVersion: params.OperatorVersion,
	})
	thresholdKey := graph.StageCacheKey(graph.CacheKeyInput{
		Stage: "threshold", InputHash: inputHash,
		ThresholdVersion: thresholdVersion, QuantVersion: quantVersion,
		OrderHash: orderHash, AlgoVersion: params.OperatorVersion,
		UpstreamKey: gradientKey,
	})
	markerKey := graph.StageCacheKey(graph.CacheKeyInput{
		Stage: "markers", InputHash: inputHash,
		ThresholdVersion: thresholdVersion, QuantVersion: quantVersion,
		OrderHash: orderHash, AlgoVersion: params.OperatorVersion,
		UpstreamKey: thresholdKey,
	})
	statsKey := graph.StageCacheKey(graph.CacheKeyInput{
		Stage: "stats", InputHash: inputHash,
		ThresholdVersion: thresholdVersion, QuantVersion: quantVersion,
		OrderHash: orderHash, AlgoVersion: params.OperatorVersion,
		UpstreamKey: markerKey,
	})
	return StageKeys{Gradient: gradientKey, Threshold: thresholdKey, Markers: markerKey, Stats: statsKey}
}

func ContentHash(stageName string, result *BlockResult) string {
	switch stageName {
	case string(state.StageLoaded):
		return graph.SHA256Hex("loaded", result.InputHash)
	case string(state.StageGradient):
		if result.Gradient == nil {
			return graph.SHA256Hex("nil-gradient")
		}
		return graph.HashJSON(result.Gradient.Magnitude)
	case string(state.StageThreshold):
		if result.Threshold == nil {
			return graph.SHA256Hex("nil-threshold")
		}
		return graph.HashJSON([]any{result.Threshold.Threshold, result.Threshold.QuantileValue, result.Threshold.Rank})
	case string(state.StageMarked):
		return graph.HashJSON(result.Markers)
	case string(state.StageCleaned):
		if result.Stats == nil {
			return graph.SHA256Hex("nil-stats")
		}
		return graph.HashJSON([]any{result.Stats.Foreground, result.Stats.Mask, result.Stats.Components})
	default:
		return graph.HashJSON(result)
	}
}

func ErrorCode(err error) string {
	if e, ok := errs.AsError(err); ok {
		return e.Code
	}
	return ""
}
