package schema

import (
	"featurekernel/internal/kernel"
)

type V2Project struct {
	SchemaVersion  string      `json:"schemaVersion"`
	ProjectID      string      `json:"projectId"`
	BatchID        string      `json:"batchId"`
	Seed           int64       `json:"seed"`
	TimeBaseMs     int64       `json:"timeBaseMs"`
	TickMs         int64       `json:"tickMs"`
	Tolerance      float64     `json:"fixedPointTolerance"`
	Channels       []string    `json:"channels"`
	Features       []V2Feature `json:"featureDefinitions"`
	DerivedMetrics []V2Derived `json:"derivedMetrics"`
	EventStream    []V2Record  `json:"eventStream"`
	WatermarkLagMs int64       `json:"watermarkLagMs"`
	PathThreshold  float64     `json:"incrementalRatioThreshold"`
	WorkerBudget   int         `json:"workerCount"`
	WindowBuffer   int         `json:"windowBufferPerFeature"`
}

type V2Feature struct {
	FeatureID         string           `json:"featureId"`
	WindowWidthMs     int64            `json:"windowWidthMs"`
	SlideIntervalMs   int64            `json:"slideIntervalMs"`
	Aggregations      []string         `json:"aggregations"`
	WindowBase        string           `json:"windowBase"`
	AllowedLatenessMs int64            `json:"allowedLatenessMs"`
	WeightScalePer10K int64            `json:"weightScalePer10K"`
	ChannelWeights    map[string]int64 `json:"channelWeights"`
}

type V2Derived struct {
	DerivedID            string `json:"derivedId"`
	BaseFeatureID        string `json:"baseFeatureId"`
	NumeratorFeatureID   string `json:"numeratorFeatureId"`
	DenominatorFeatureID string `json:"denominatorFeatureId"`
	Expression           string `json:"expression"`
}

type V2Record struct {
	Sequence          int64    `json:"sequence"`
	RecordType        string   `json:"recordType"`
	FeatureID         string   `json:"featureId,omitempty"`
	ObservedSequence  *int64   `json:"observedSequence,omitempty"`
	ChannelName       string   `json:"channelName,omitempty"`
	SampleValue       *float64 `json:"sampleValue,omitempty"`
	WatermarkSequence *int64   `json:"watermarkSequence,omitempty"`
}

func loadV2(dto *V2Project) (*kernel.Project, error) {
	project := &kernel.Project{
		SchemaVersion:  "v2",
		ProjectID:      dto.ProjectID,
		BatchID:        dto.BatchID,
		Seed:           dto.Seed,
		TimeBaseMs:     dto.TimeBaseMs,
		TickMs:         dto.TickMs,
		FixedTolerance: dto.Tolerance,
		Channels:       append([]string(nil), dto.Channels...),
		PathPolicy:     defaultPathPolicy(dto.PathThreshold),
		Budget: defaultBudget(kernel.Budget{
			Workers:              dto.WorkerBudget,
			MaxWindowsPerFeature: dto.WindowBuffer,
		}),
		WatermarkPolicy: kernel.WatermarkPolicy{ExplicitFieldEnabled: true, Status: "explicit_relative_sequence", LagMs: dto.WatermarkLagMs},
	}
	if project.FixedTolerance == 0 {
		project.FixedTolerance = 1e-6
	}

	records, err := v2Records(dto)
	if err != nil {
		return nil, err
	}
	project.Records = records
	anchor, ok := firstEventTime(records)
	if !ok {
		return nil, kernel.NewError(kernel.CodeSchemaInvalidCombination, "project has no event for first_data_point anchor", map[string]any{"project_id": dto.ProjectID})
	}

	for _, raw := range dto.Features {
		if raw.WindowBase != kernel.AlignmentFirstPoint {
			return nil, kernel.NewError(kernel.CodeSchemaInvalidCombination, "v2 requires first_data_point windowBase", map[string]any{"feature_id": raw.FeatureID, "window_base": raw.WindowBase})
		}
		aggs, err := normalizeAggregations(raw.Aggregations, map[string]any{"feature_id": raw.FeatureID})
		if err != nil {
			return nil, err
		}
		weights, err := ppmWeights(raw.ChannelWeights, raw.WeightScalePer10K, raw.FeatureID)
		if err != nil {
			return nil, err
		}
		def := kernel.FeatureDef{
			ID: raw.FeatureID, WidthMs: raw.WindowWidthMs, SlideMs: raw.SlideIntervalMs,
			Aggregations: aggs, AnchorMs: anchor, AllowedLatenessMs: raw.AllowedLatenessMs,
			ChannelWeightsPPM: weights,
		}
		if err := kernel.ValidateFeature(def); err != nil {
			return nil, err
		}
		project.Features = append(project.Features, def)
	}
	for _, raw := range dto.DerivedMetrics {
		project.Derived = append(project.Derived, kernel.DerivedSpec{
			ID: raw.DerivedID, BaseFeatureID: raw.BaseFeatureID,
			NumeratorFeature: raw.NumeratorFeatureID, DenominatorFeature: raw.DenominatorFeatureID,
			Expression: raw.Expression,
		})
	}
	if err := finalizeRecords(project); err != nil {
		return nil, err
	}
	return project, nil
}

func v2Records(dto *V2Project) ([]kernel.Record, error) {
	out := make([]kernel.Record, 0, len(dto.EventStream))
	for _, raw := range dto.EventStream {
		record := kernel.Record{Seq: raw.Sequence}
		restoreTime := func(seq int64) int64 { return dto.TimeBaseMs + seq*dto.TickMs }
		switch raw.RecordType {
		case string(kernel.RecordEvent):
			if raw.ObservedSequence == nil || raw.SampleValue == nil || raw.FeatureID == "" || raw.ChannelName == "" {
				return nil, kernel.NewError(kernel.CodeSchemaInvalidCombination, "v2 event requires featureId, observedSequence, channelName and sampleValue", map[string]any{"sequence": raw.Sequence})
			}
			timeMs := restoreTime(*raw.ObservedSequence)
			record.Kind = kernel.RecordEvent
			record.Event = &kernel.Event{
				ArrivalSeq: raw.Sequence, FeatureID: raw.FeatureID, TimeMs: timeMs,
				Channel: raw.ChannelName, Value: *raw.SampleValue,
			}
		case string(kernel.RecordWatermark):
			if raw.WatermarkSequence == nil {
				return nil, kernel.NewError(kernel.CodeSchemaInvalidCombination, "v2 watermark requires watermarkSequence", map[string]any{"sequence": raw.Sequence})
			}
			record.Kind = kernel.RecordWatermark
			record.Watermark = &kernel.Watermark{ArrivalSeq: raw.Sequence, TimeMs: restoreTime(*raw.WatermarkSequence)}
		default:
			return nil, kernel.NewError(kernel.CodeSchemaInvalidCombination, "unknown recordType", map[string]any{"sequence": raw.Sequence, "record_type": raw.RecordType})
		}
		out = append(out, record)
	}
	return out, nil
}
