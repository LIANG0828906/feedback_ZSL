package schema

import (
	"encoding/json"

	"featurekernel/internal/kernel"
)

func Encode(project *kernel.Project, version string) ([]byte, error) {
	var dto any
	var err error
	switch version {
	case "v1":
		dto, err = toV1(project)
	case "v2":
		dto, err = toV2(project)
	case "v3":
		dto, err = toV3(project)
	default:
		return nil, kernel.NewError(kernel.CodeSchemaUnsupportedVersion, "cannot encode unsupported version", map[string]any{"version": version})
	}
	if err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(dto, "", "  ")
	if err != nil {
		return nil, kernel.WrapError(kernel.CodeInvalidArgument, err, map[string]any{"stage": "encode_json"})
	}
	data = append(data, '\n')
	return data, nil
}

func rawWeights(ppm map[string]int64, scale int64) (map[string]int64, error) {
	factor := 1_000_000 / scale
	out := make(map[string]int64, len(ppm))
	for channel, value := range ppm {
		if value%factor != 0 {
			return nil, kernel.NewError(kernel.CodeAggWeightOutOfRange, "canonical weight cannot be represented exactly in target scale", map[string]any{
				"channel": channel, "weight_ppm": value, "target_scale": scale,
			})
		}
		out[channel] = value / factor
	}
	return out, nil
}

func temporalSequence(project *kernel.Project, timeMs int64) (int64, error) {
	delta := timeMs - project.TimeBaseMs
	if project.TickMs <= 0 || delta%project.TickMs != 0 {
		return 0, kernel.NewError(kernel.CodeSchemaInvalidCombination, "time is not representable as relative event sequence", map[string]any{
			"time_ms": timeMs, "base_ms": project.TimeBaseMs, "tick_ms": project.TickMs,
		})
	}
	return delta / project.TickMs, nil
}

func toV1(project *kernel.Project) (*V1Project, error) {
	dto := &V1Project{
		SchemaVersion: "v1", ProjectID: project.ProjectID, BatchID: project.BatchID,
		Seed: project.Seed, TimeBaseMs: project.TimeBaseMs, TickMs: project.TickMs,
		FixedTolerance: project.FixedTolerance, Channels: project.Channels,
		WatermarkLagMs: project.WatermarkPolicy.LagMs, PathThreshold: project.PathPolicy.IncrementalRatioThreshold,
		WorkerBudget: project.Budget.Workers, WindowBuffer: project.Budget.MaxWindowsPerFeature,
	}
	for _, feature := range project.Features {
		weights, err := rawWeights(feature.ChannelWeightsPPM, 1000)
		if err != nil {
			return nil, err
		}
		dto.Features = append(dto.Features, V1Feature{
			FeatureID: feature.ID, WindowWidthMs: feature.WidthMs, SlideMs: feature.SlideMs,
			AggFuncs: aggStrings(feature.Aggregations), WindowBase: kernel.AlignmentEpochHour,
			AllowedLatenessMs: feature.AllowedLatenessMs, WeightScale: 1000, ChannelWeights: weights,
		})
	}
	for _, derived := range project.Derived {
		dto.Derived = append(dto.Derived, V1Derived{
			DerivedID: derived.ID, BaseFeatureID: derived.BaseFeatureID,
			NumeratorFeatureID: derived.NumeratorFeature, DenominatorFeatureID: derived.DenominatorFeature,
			Expression: derived.Expression,
		})
	}
	for _, record := range project.Records {
		raw := V1Record{Seq: record.Seq, Kind: string(record.Kind)}
		if record.Event != nil {
			raw.FeatureID = record.Event.FeatureID
			raw.EventTimeMs = &record.Event.TimeMs
			raw.Channel = record.Event.Channel
			value := record.Event.Value
			raw.SampleValue = &value
		}
		if record.Watermark != nil {
			raw.WatermarkMs = &record.Watermark.TimeMs
		}
		dto.Records = append(dto.Records, raw)
	}
	return dto, nil
}

func toV2(project *kernel.Project) (*V2Project, error) {
	dto := &V2Project{
		SchemaVersion: "v2", ProjectID: project.ProjectID, BatchID: project.BatchID,
		Seed: project.Seed, TimeBaseMs: project.TimeBaseMs, TickMs: project.TickMs,
		Tolerance: project.FixedTolerance, Channels: project.Channels,
		WatermarkLagMs: project.WatermarkPolicy.LagMs, PathThreshold: project.PathPolicy.IncrementalRatioThreshold,
		WorkerBudget: project.Budget.Workers, WindowBuffer: project.Budget.MaxWindowsPerFeature,
	}
	for _, feature := range project.Features {
		weights, err := rawWeights(feature.ChannelWeightsPPM, 10000)
		if err != nil {
			return nil, err
		}
		dto.Features = append(dto.Features, V2Feature{
			FeatureID: feature.ID, WindowWidthMs: feature.WidthMs, SlideIntervalMs: feature.SlideMs,
			Aggregations: aggStrings(feature.Aggregations), WindowBase: kernel.AlignmentFirstPoint,
			AllowedLatenessMs: feature.AllowedLatenessMs, WeightScalePer10K: 10000, ChannelWeights: weights,
		})
	}
	for _, derived := range project.Derived {
		dto.DerivedMetrics = append(dto.DerivedMetrics, V2Derived{
			DerivedID: derived.ID, BaseFeatureID: derived.BaseFeatureID,
			NumeratorFeatureID: derived.NumeratorFeature, DenominatorFeatureID: derived.DenominatorFeature,
			Expression: derived.Expression,
		})
	}
	for _, record := range project.Records {
		raw := V2Record{Sequence: record.Seq, RecordType: string(record.Kind)}
		if record.Event != nil {
			seq, err := temporalSequence(project, record.Event.TimeMs)
			if err != nil {
				return nil, err
			}
			raw.FeatureID = record.Event.FeatureID
			raw.ObservedSequence = &seq
			raw.ChannelName = record.Event.Channel
			value := record.Event.Value
			raw.SampleValue = &value
		}
		if record.Watermark != nil {
			seq, err := temporalSequence(project, record.Watermark.TimeMs)
			if err != nil {
				return nil, err
			}
			raw.WatermarkSequence = &seq
		}
		dto.EventStream = append(dto.EventStream, raw)
	}
	return dto, nil
}

func toV3(project *kernel.Project) (*V3Project, error) {
	dto := &V3Project{
		SchemaVersion: "v3", ProjectID: project.ProjectID, BatchID: project.BatchID,
		Seed: project.Seed, BaseEpochMs: project.TimeBaseMs, TickMs: project.TickMs,
		Tolerance: project.FixedTolerance, Channels: project.Channels,
		WatermarkLagMs: project.WatermarkPolicy.LagMs, PathThreshold: project.PathPolicy.IncrementalRatioThreshold,
		WorkerBudget: project.Budget.Workers, WindowBuffer: project.Budget.MaxWindowsPerFeature,
	}
	for _, feature := range project.Features {
		weights, err := rawWeights(feature.ChannelWeightsPPM, 10000)
		if err != nil {
			return nil, err
		}
		dto.Features = append(dto.Features, V3Feature{
			ID: feature.ID, WidthMs: feature.WidthMs, StepMs: feature.SlideMs,
			Aggregates: aggStrings(feature.Aggregations), WindowBase: kernel.AlignmentFirstPoint,
			LatenessMs: feature.AllowedLatenessMs, WeightScalePer10K: 10000, ChannelWeights: weights,
		})
	}
	for _, derived := range project.Derived {
		dto.DerivedMetrics = append(dto.DerivedMetrics, V3Derived{
			ID: derived.ID, BaseFeatureID: derived.BaseFeatureID,
			NumeratorFeatureID: derived.NumeratorFeature, DenominatorFeatureID: derived.DenominatorFeature,
			Expression: derived.Expression,
		})
	}
	for _, record := range project.Records {
		raw := V3Record{Seq: record.Seq}
		switch record.Kind {
		case kernel.RecordEvent:
			if record.Event == nil {
				return nil, kernel.NewError(kernel.CodeSchemaInvalidCombination, "event record missing payload", map[string]any{"seq": record.Seq})
			}
			raw.Type = string(kernel.RecordEvent)
			raw.FID = record.Event.FeatureID
			raw.EpochMs = &record.Event.TimeMs
			raw.Channel = record.Event.Channel
			value := record.Event.Value
			raw.Value = &value
		case kernel.RecordWatermark:
			raw.Type = "barrier"
		default:
			return nil, kernel.NewError(kernel.CodeSchemaInvalidCombination, "cannot encode unknown record kind", map[string]any{"seq": record.Seq, "kind": record.Kind})
		}
		dto.Stream = append(dto.Stream, raw)
	}
	return dto, nil
}

func aggStrings(funcs []kernel.AggFunc) []string {
	out := make([]string, 0, len(funcs))
	for _, fn := range funcs {
		out = append(out, string(fn))
	}
	return out
}
