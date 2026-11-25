package schema

import (
	"featurekernel/internal/kernel"
)

type V1Project struct {
	SchemaVersion  string      `json:"schema_version"`
	ProjectID      string      `json:"project_id"`
	BatchID        string      `json:"batch_id"`
	Seed           int64       `json:"seed"`
	TimeBaseMs     int64       `json:"time_base_ms"`
	TickMs         int64       `json:"tick_ms"`
	FixedTolerance float64     `json:"fixed_tolerance"`
	Channels       []string    `json:"channels"`
	Features       []V1Feature `json:"feature_defs"`
	Derived        []V1Derived `json:"derived_metrics"`
	Records        []V1Record  `json:"records"`
	WatermarkLagMs int64       `json:"watermark_lag_ms"`
	PathThreshold  float64     `json:"path_ratio_threshold"`
	WorkerBudget   int         `json:"worker_budget"`
	WindowBuffer   int         `json:"window_buffer_per_feature"`
}

type V1Feature struct {
	FeatureID         string           `json:"feature_id"`
	WindowWidthMs     int64            `json:"window_width_ms"`
	SlideMs           int64            `json:"slide_ms"`
	AggFuncs          []string         `json:"agg_funcs"`
	WindowBase        string           `json:"window_base"`
	AllowedLatenessMs int64            `json:"allowed_lateness_ms"`
	WeightScale       int64            `json:"weight_scale"`
	ChannelWeights    map[string]int64 `json:"channel_weights"`
}

type V1Derived struct {
	DerivedID            string `json:"derived_id"`
	BaseFeatureID        string `json:"base_feature_id"`
	NumeratorFeatureID   string `json:"numerator_feature_id"`
	DenominatorFeatureID string `json:"denominator_feature_id"`
	Expression           string `json:"expression"`
}

type V1Record struct {
	Seq         int64    `json:"seq"`
	Kind        string   `json:"kind"`
	FeatureID   string   `json:"feature_id,omitempty"`
	EventTimeMs *int64   `json:"event_time_ms,omitempty"`
	Channel     string   `json:"channel,omitempty"`
	SampleValue *float64 `json:"sample_value,omitempty"`
	WatermarkMs *int64   `json:"watermark_ms,omitempty"`
}

func loadV1(dto *V1Project) (*kernel.Project, error) {
	project := &kernel.Project{
		SchemaVersion:  "v1",
		ProjectID:      dto.ProjectID,
		BatchID:        dto.BatchID,
		Seed:           dto.Seed,
		TimeBaseMs:     dto.TimeBaseMs,
		TickMs:         dto.TickMs,
		FixedTolerance: dto.FixedTolerance,
		Channels:       append([]string(nil), dto.Channels...),
		PathPolicy:     defaultPathPolicy(dto.PathThreshold),
		Budget: defaultBudget(kernel.Budget{
			Workers:              dto.WorkerBudget,
			MaxWindowsPerFeature: dto.WindowBuffer,
		}),
		WatermarkPolicy: kernel.WatermarkPolicy{ExplicitFieldEnabled: true, Status: "explicit_epoch_ms", LagMs: dto.WatermarkLagMs},
	}
	if project.FixedTolerance == 0 {
		project.FixedTolerance = 1e-6
	}

	anchor := epochHourAnchor(dto.TimeBaseMs)
	for _, raw := range dto.Features {
		if raw.WindowBase != kernel.AlignmentEpochHour {
			return nil, kernel.NewError(kernel.CodeSchemaInvalidCombination, "v1 requires epoch_hour window_base", map[string]any{"feature_id": raw.FeatureID, "window_base": raw.WindowBase})
		}
		aggs, err := normalizeAggregations(raw.AggFuncs, map[string]any{"feature_id": raw.FeatureID})
		if err != nil {
			return nil, err
		}
		weights, err := ppmWeights(raw.ChannelWeights, raw.WeightScale, raw.FeatureID)
		if err != nil {
			return nil, err
		}
		def := kernel.FeatureDef{
			ID:                raw.FeatureID,
			WidthMs:           raw.WindowWidthMs,
			SlideMs:           raw.SlideMs,
			Aggregations:      aggs,
			AnchorMs:          anchor,
			AllowedLatenessMs: raw.AllowedLatenessMs,
			ChannelWeightsPPM: weights,
		}
		if err := kernel.ValidateFeature(def); err != nil {
			return nil, err
		}
		project.Features = append(project.Features, def)
	}

	for _, raw := range dto.Derived {
		project.Derived = append(project.Derived, kernel.DerivedSpec{
			ID: raw.DerivedID, BaseFeatureID: raw.BaseFeatureID,
			NumeratorFeature: raw.NumeratorFeatureID, DenominatorFeature: raw.DenominatorFeatureID,
			Expression: raw.Expression,
		})
	}

	records, err := v1Records(dto)
	if err != nil {
		return nil, err
	}
	project.Records = records
	if err := finalizeRecords(project); err != nil {
		return nil, err
	}
	return project, nil
}

func v1Records(dto *V1Project) ([]kernel.Record, error) {
	out := make([]kernel.Record, 0, len(dto.Records))
	for _, raw := range dto.Records {
		record := kernel.Record{Seq: raw.Seq}
		switch raw.Kind {
		case string(kernel.RecordEvent):
			if raw.EventTimeMs == nil || raw.SampleValue == nil || raw.FeatureID == "" || raw.Channel == "" {
				return nil, kernel.NewError(kernel.CodeSchemaInvalidCombination, "v1 event requires feature_id, event_time_ms, channel and sample_value", map[string]any{"seq": raw.Seq})
			}
			record.Kind = kernel.RecordEvent
			record.Event = &kernel.Event{
				ArrivalSeq: raw.Seq, FeatureID: raw.FeatureID, TimeMs: *raw.EventTimeMs,
				Channel: raw.Channel, Value: *raw.SampleValue,
			}
		case string(kernel.RecordWatermark):
			if raw.WatermarkMs == nil {
				return nil, kernel.NewError(kernel.CodeSchemaInvalidCombination, "v1 watermark requires watermark_ms", map[string]any{"seq": raw.Seq})
			}
			record.Kind = kernel.RecordWatermark
			record.Watermark = &kernel.Watermark{ArrivalSeq: raw.Seq, TimeMs: *raw.WatermarkMs}
		default:
			return nil, kernel.NewError(kernel.CodeSchemaInvalidCombination, "unknown record kind", map[string]any{"seq": raw.Seq, "kind": raw.Kind})
		}
		out = append(out, record)
	}
	return out, nil
}

func finalizeRecords(project *kernel.Project) error {
	sortRecords(project.Records)
	if err := validateRecordSequence(project.Records); err != nil {
		return err
	}
	featureIDs := map[string]bool{}
	for _, feature := range project.Features {
		featureIDs[feature.ID] = true
	}
	for _, record := range project.Records {
		if record.Kind == kernel.RecordEvent && record.Event != nil && !featureIDs[record.Event.FeatureID] {
			return kernel.NewError(kernel.CodeEventUnknownFeature, "event references unknown feature", map[string]any{"seq": record.Seq, "feature_id": record.Event.FeatureID})
		}
	}
	if err := kernel.ValidateBudget(project.Budget); err != nil {
		return err
	}
	return nil
}
