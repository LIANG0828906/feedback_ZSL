package kernel

import (
	"fmt"
	"sort"
)

const (
	CanonicalVersion = "canonical.v1"

	AlignmentEpochHour    = "epoch_hour"
	AlignmentFirstPoint   = "first_data_point"
	AttributionInteger    = "R-WIN-INT-RATIO"
	AttributionFractional = "R-WIN-FRACTIONAL-RATIO"

	PathIncremental  = "incremental_accumulation"
	PathRescan       = "full_window_rescan"
	PathDecisionRule = "PATH-WIDTH-STEP-RATIO-GT-THRESHOLD"
	PathBoundaryRule = "EQUAL_TO_THRESHOLD_FALLS_BACK_TO_RESCAN"

	WatermarkDegradeMaxSeenLag = "D-WM-MAX-SEEN-LAG"
)

type AggFunc string

const (
	AggCount       AggFunc = "count"
	AggSum         AggFunc = "sum"
	AggAvg         AggFunc = "avg"
	AggMin         AggFunc = "min"
	AggMax         AggFunc = "max"
	AggWeightedAvg AggFunc = "weighted_avg"
)

func ParseAggFunc(value string) (AggFunc, error) {
	switch AggFunc(value) {
	case AggCount, AggSum, AggAvg, AggMin, AggMax, AggWeightedAvg:
		return AggFunc(value), nil
	default:
		return "", NewError(CodeAggUnknownFunction, "unknown aggregation function", map[string]any{"function": value})
	}
}

type FeatureDef struct {
	ID                string           `json:"id"`
	WidthMs           int64            `json:"width_ms"`
	SlideMs           int64            `json:"slide_ms"`
	Aggregations      []AggFunc        `json:"aggregations"`
	AnchorMs          int64            `json:"anchor_ms"`
	AllowedLatenessMs int64            `json:"allowed_lateness_ms"`
	ChannelWeightsPPM map[string]int64 `json:"channel_weights_ppm"`
}

type DerivedSpec struct {
	ID                 string `json:"id"`
	BaseFeatureID      string `json:"base_feature_id"`
	NumeratorFeature   string `json:"numerator_feature"`
	DenominatorFeature string `json:"denominator_feature"`
	Expression         string `json:"expression"`
}

type Event struct {
	ArrivalSeq int64   `json:"arrival_seq"`
	FeatureID  string  `json:"feature_id"`
	TimeMs     int64   `json:"time_ms"`
	Channel    string  `json:"channel"`
	Value      float64 `json:"value"`
}

type Watermark struct {
	ArrivalSeq int64 `json:"arrival_seq"`
	TimeMs     int64 `json:"time_ms"`
}

type RecordKind string

const (
	RecordEvent     RecordKind = "event"
	RecordWatermark RecordKind = "watermark"
)

type Record struct {
	Seq       int64      `json:"seq"`
	Kind      RecordKind `json:"kind"`
	Event     *Event     `json:"event,omitempty"`
	Watermark *Watermark `json:"watermark,omitempty"`
}

type WatermarkPolicy struct {
	ExplicitFieldEnabled bool   `json:"explicit_field_enabled"`
	Degraded             bool   `json:"degraded"`
	Rule                 string `json:"rule,omitempty"`
	LagMs                int64  `json:"lag_ms,omitempty"`
	Status               string `json:"status"`
}

type Project struct {
	SchemaVersion   string          `json:"schema_version,omitempty"`
	ProjectID       string          `json:"project_id"`
	BatchID         string          `json:"batch_id"`
	Seed            int64           `json:"seed"`
	TimeBaseMs      int64           `json:"time_base_ms"`
	TickMs          int64           `json:"tick_ms"`
	FixedTolerance  float64         `json:"fixed_tolerance"`
	Features        []FeatureDef    `json:"features"`
	Derived         []DerivedSpec   `json:"derived,omitempty"`
	Records         []Record        `json:"records"`
	Channels        []string        `json:"channels"`
	WatermarkPolicy WatermarkPolicy `json:"watermark_policy"`
	PathPolicy      PathPolicy      `json:"path_policy"`
	Budget          Budget          `json:"budget"`
}

type PathPolicy struct {
	IncrementalRatioThreshold float64 `json:"incremental_ratio_threshold"`
	Boundary                  string  `json:"boundary"`
}

type Budget struct {
	Workers              int `json:"workers"`
	MaxWindowsPerFeature int `json:"max_windows_per_feature"`
	PendingTasks         int `json:"pending_tasks,omitempty"`
}

type AggregationValue struct {
	Func  AggFunc `json:"func"`
	Value float64 `json:"value"`
}

type WindowResult struct {
	FeatureID       string             `json:"feature_id"`
	WindowID        string             `json:"window_id"`
	WindowIndex     int64              `json:"window_index"`
	StartMs         int64              `json:"start_ms"`
	EndMs           int64              `json:"end_ms"`
	EventCount      int                `json:"event_count"`
	Aggregations    []AggregationValue `json:"aggregations"`
	AttributionRule string             `json:"attribution_rule"`
	Path            string             `json:"path"`
	Submitted       bool               `json:"submitted"`
	Invalidation    *ResultProvenance  `json:"invalidation,omitempty"`
}

type DerivedResult struct {
	DerivedID    string            `json:"derived_id"`
	WindowID     string            `json:"window_id"`
	WindowIndex  int64             `json:"window_index"`
	StartMs      int64             `json:"start_ms"`
	EndMs        int64             `json:"end_ms"`
	Value        float64           `json:"value"`
	Dependencies []string          `json:"dependencies"`
	Invalidation *ResultProvenance `json:"invalidation,omitempty"`
}

type Recomputation struct {
	FeatureID       string `json:"feature_id"`
	WindowID        string `json:"window_id"`
	WindowIndex     int64  `json:"window_index"`
	TriggerSeq      int64  `json:"trigger_seq"`
	EventArrivalSeq int64  `json:"event_arrival_seq"`
	LateMs          int64  `json:"late_ms"`
	Reason          string `json:"reason"`
}

type DiscardedRecord struct {
	ArrivalSeq  int64   `json:"arrival_seq"`
	FeatureID   string  `json:"feature_id"`
	WindowID    string  `json:"window_id"`
	WindowIndex int64   `json:"window_index"`
	StartMs     int64   `json:"start_ms"`
	EndMs       int64   `json:"end_ms"`
	TimeMs      int64   `json:"time_ms"`
	Channel     string  `json:"channel"`
	Value       float64 `json:"value"`
	LateMs      int64   `json:"late_ms"`
	Code        string  `json:"code"`
}

type PathDecision struct {
	FeatureID string  `json:"feature_id"`
	Ratio     float64 `json:"ratio"`
	Threshold float64 `json:"threshold"`
	Path      string  `json:"path"`
	Rule      string  `json:"rule"`
	Boundary  string  `json:"boundary"`
}

type FeatureDiagnostic struct {
	FeatureID          string `json:"feature_id"`
	ElapsedNanoseconds int64  `json:"elapsed_nanoseconds"`
	EventsProcessed    int    `json:"events_processed"`
	WindowsProcessed   int    `json:"windows_processed"`
	Path               string `json:"path"`
	WorkerID           int    `json:"worker_id,omitempty"`
}

type ResultSet struct {
	ProjectID         string              `json:"project_id"`
	BatchID           string              `json:"batch_id"`
	CanonicalVersion  string              `json:"canonical_version"`
	Windows           []WindowResult      `json:"windows"`
	Derived           []DerivedResult     `json:"derived,omitempty"`
	Recomputations    []Recomputation     `json:"recomputations"`
	RecomputedWindows []string            `json:"recomputed_windows"`
	Discarded         []DiscardedRecord   `json:"discarded"`
	PathDecisions     []PathDecision      `json:"path_decisions"`
	Diagnostics       []FeatureDiagnostic `json:"diagnostics,omitempty"`
	WatermarkStatus   WatermarkPolicy     `json:"watermark_status"`
}

func WindowID(featureID string, index int64) string {
	return fmt.Sprintf("%s:w:%d", featureID, index)
}

func SortedAggNames(values []AggregationValue) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, string(value.Func))
	}
	sort.Strings(out)
	return out
}
