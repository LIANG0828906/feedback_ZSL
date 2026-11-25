package schema

import (
	"featurekernel/internal/kernel"
)

type V3Project struct {
	SchemaVersion  string      `json:"schema-version"`
	ProjectID      string      `json:"project-id"`
	BatchID        string      `json:"batch-id"`
	Seed           int64       `json:"seed"`
	BaseEpochMs    int64       `json:"base-epoch-ms"`
	TickMs         int64       `json:"tick-ms"`
	Tolerance      float64     `json:"fixed-tolerance"`
	Channels       []string    `json:"channels"`
	Features       []V3Feature `json:"feature-definitions"`
	DerivedMetrics []V3Derived `json:"derived-metrics"`
	Stream         []V3Record  `json:"stream"`
	WatermarkLagMs int64       `json:"watermark-lag-ms"`
	PathThreshold  float64     `json:"path-threshold"`
	WorkerBudget   int         `json:"worker-budget"`
	WindowBuffer   int         `json:"window-buffer"`
}

type V3Feature struct {
	ID                string           `json:"id"`
	WidthMs           int64            `json:"width-ms"`
	StepMs            int64            `json:"step-ms"`
	Aggregates        []string         `json:"aggregates"`
	WindowBase        string           `json:"window-base"`
	LatenessMs        int64            `json:"lateness-ms"`
	WeightScalePer10K int64            `json:"weight-scale-per-10k"`
	ChannelWeights    map[string]int64 `json:"channel-weights"`
}

type V3Derived struct {
	ID                   string `json:"id"`
	BaseFeatureID        string `json:"base-feature-id"`
	NumeratorFeatureID   string `json:"numerator-feature-id"`
	DenominatorFeatureID string `json:"denominator-feature-id"`
	Expression           string `json:"expression"`
}

type V3Record struct {
	Seq     int64    `json:"seq"`
	Type    string   `json:"type"`
	FID     string   `json:"fid,omitempty"`
	EpochMs *int64   `json:"epoch-ms,omitempty"`
	Channel string   `json:"channel,omitempty"`
	Value   *float64 `json:"value,omitempty"`
}

func loadV3(dto *V3Project) (*kernel.Project, error) {
	if dto.WatermarkLagMs <= 0 {
		return nil, kernel.NewError(kernel.CodeSchemaInvalidCombination, "v3 missing watermark field requires positive declared watermark-lag-ms", map[string]any{"project_id": dto.ProjectID})
	}
	project := &kernel.Project{
		SchemaVersion:  "v3",
		ProjectID:      dto.ProjectID,
		BatchID:        dto.BatchID,
		Seed:           dto.Seed,
		TimeBaseMs:     dto.BaseEpochMs,
		TickMs:         dto.TickMs,
		FixedTolerance: dto.Tolerance,
		Channels:       append([]string(nil), dto.Channels...),
		PathPolicy:     defaultPathPolicy(dto.PathThreshold),
		Budget: defaultBudget(kernel.Budget{
			Workers:              dto.WorkerBudget,
			MaxWindowsPerFeature: dto.WindowBuffer,
		}),
		WatermarkPolicy: kernel.WatermarkPolicy{
			ExplicitFieldEnabled: false,
			Degraded:             true,
			Rule:                 kernel.WatermarkDegradeMaxSeenLag,
			LagMs:                dto.WatermarkLagMs,
			Status:               "explicit_watermark_disabled_degraded_rule_active",
		},
	}
	if project.FixedTolerance == 0 {
		project.FixedTolerance = 1e-6
	}

	records, err := v3Records(dto)
	if err != nil {
		return nil, err
	}
	if err := deriveV3Watermarks(records, dto.WatermarkLagMs); err != nil {
		return nil, err
	}
	project.Records = records
	anchor, ok := firstEventTime(records)
	if !ok {
		return nil, kernel.NewError(kernel.CodeSchemaInvalidCombination, "project has no event for first_data_point anchor", map[string]any{"project_id": dto.ProjectID})
	}

	for _, raw := range dto.Features {
		if raw.WindowBase != kernel.AlignmentFirstPoint {
			return nil, kernel.NewError(kernel.CodeSchemaInvalidCombination, "v3 requires first_data_point window-base", map[string]any{"feature_id": raw.ID, "window_base": raw.WindowBase})
		}
		aggs, err := normalizeAggregations(raw.Aggregates, map[string]any{"feature_id": raw.ID})
		if err != nil {
			return nil, err
		}
		weights, err := ppmWeights(raw.ChannelWeights, raw.WeightScalePer10K, raw.ID)
		if err != nil {
			return nil, err
		}
		def := kernel.FeatureDef{
			ID: raw.ID, WidthMs: raw.WidthMs, SlideMs: raw.StepMs,
			Aggregations: aggs, AnchorMs: anchor, AllowedLatenessMs: raw.LatenessMs,
			ChannelWeightsPPM: weights,
		}
		if err := kernel.ValidateFeature(def); err != nil {
			return nil, err
		}
		project.Features = append(project.Features, def)
	}
	for _, raw := range dto.DerivedMetrics {
		project.Derived = append(project.Derived, kernel.DerivedSpec{
			ID: raw.ID, BaseFeatureID: raw.BaseFeatureID,
			NumeratorFeature: raw.NumeratorFeatureID, DenominatorFeature: raw.DenominatorFeatureID,
			Expression: raw.Expression,
		})
	}
	if err := finalizeRecords(project); err != nil {
		return nil, err
	}
	return project, nil
}

func v3Records(dto *V3Project) ([]kernel.Record, error) {
	out := make([]kernel.Record, 0, len(dto.Stream))
	for _, raw := range dto.Stream {
		record := kernel.Record{Seq: raw.Seq}
		switch raw.Type {
		case string(kernel.RecordEvent):
			if raw.EpochMs == nil || raw.Value == nil || raw.FID == "" || raw.Channel == "" {
				return nil, kernel.NewError(kernel.CodeSchemaInvalidCombination, "v3 event requires fid, epoch-ms, channel and value", map[string]any{"seq": raw.Seq})
			}
			record.Kind = kernel.RecordEvent
			record.Event = &kernel.Event{
				ArrivalSeq: raw.Seq, FeatureID: raw.FID, TimeMs: *raw.EpochMs,
				Channel: raw.Channel, Value: *raw.Value,
			}
		case "barrier":
			record.Kind = kernel.RecordWatermark
			record.Watermark = &kernel.Watermark{ArrivalSeq: raw.Seq}
		default:
			return nil, kernel.NewError(kernel.CodeSchemaInvalidCombination, "unknown v3 record type", map[string]any{"seq": raw.Seq, "type": raw.Type})
		}
		out = append(out, record)
	}
	return out, nil
}

func deriveV3Watermarks(records []kernel.Record, lagMs int64) error {
	sortRecords(records)
	var maxTime int64
	haveTime := false
	for i := range records {
		record := &records[i]
		if record.Kind == kernel.RecordEvent && record.Event != nil {
			if !haveTime || record.Event.TimeMs > maxTime {
				maxTime = record.Event.TimeMs
				haveTime = true
			}
			continue
		}
		if record.Kind == kernel.RecordWatermark && record.Watermark != nil && record.Watermark.TimeMs == 0 {
			if !haveTime {
				return kernel.NewError(kernel.CodeSchemaInvalidCombination, "degraded watermark barrier appears before any event", map[string]any{"seq": record.Seq})
			}
			record.Watermark.TimeMs = maxTime - lagMs
		}
	}
	return nil
}
