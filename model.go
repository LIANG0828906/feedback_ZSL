package downsample

import (
	"fmt"
	"sort"
)

type SchemaVersion string

const (
	SchemaV1 SchemaVersion = "v1"
	SchemaV2 SchemaVersion = "v2"
	SchemaV3 SchemaVersion = "v3"
)

type Alignment string

const (
	// AlignAnchor uses AnchorMS as the grid origin. Epoch/whole-minute and
	// first-point rules are both normalized to this representation.
	AlignAnchor Alignment = "anchor"
)

type SourceAlignment string

const (
	SourceAlignEpoch      SourceAlignment = "epoch_grid"
	SourceAlignFirstPoint SourceAlignment = "first_data_point"
)

type WeightScale int64

const (
	ScalePermille     WeightScale = 1000
	ScalePermyriad    WeightScale = 10000
	DefaultWeightBits             = 32
)

func (s WeightScale) Valid() bool { return s == ScalePermille || s == ScalePermyriad }

type Func string

const (
	FuncCount       Func = "count"
	FuncSum         Func = "sum"
	FuncAvg         Func = "avg"
	FuncMin         Func = "min"
	FuncMax         Func = "max"
	FuncFirst       Func = "first"
	FuncLast        Func = "last"
	FuncWeightedAvg Func = "weighted_avg"
	FuncWeightedSum Func = "weighted_sum"
	FuncWeightTotal Func = "weight_total"
	FuncDerived     Func = "derived_value"
)

type WindowStatus string

const (
	StatusOK      WindowStatus = "OK"
	StatusFilled  WindowStatus = "FILLED"
	StatusEmpty   WindowStatus = "EMPTY"
	StatusMissing WindowStatus = "MISSING"
)

// Point is the schema-independent sample point.
type Point struct {
	Seq               int64   `json:"seq"`
	MetricID          string  `json:"metric_id"`
	TimestampMS       int64   `json:"timestamp_ms"`
	Value             float64 `json:"value"`
	Weight            int64   `json:"weight_permyriad"`
	SourceWeightRaw   int64   `json:"source_weight_raw,omitempty"`
	SourceWeightScale int64   `json:"source_weight_scale,omitempty"`
	// RelativeWindowIndex and RelativeOffsetMS retain the source expression
	// for diagnostics. TimestampMS is the restored unified timeline value.
	RelativeWindowIndex *int64 `json:"relative_window_index,omitempty"`
	RelativeOffsetMS    *int64 `json:"relative_offset_ms,omitempty"`
}

type FillPolicy struct {
	Enabled            bool   `json:"enabled"`
	Strategy           string `json:"strategy"`
	MaxLookbackWindows int    `json:"max_lookback_windows"`
	FieldPresent       bool   `json:"field_present"`
	Degraded           bool   `json:"degraded"`
	DegradationRule    string `json:"degradation_rule,omitempty"`
}

type DerivedInput struct {
	MetricID string `json:"metric_id"`
	Function Func   `json:"function"`
	Lag      int    `json:"lag"`
}

type Derivation struct {
	Inputs []DerivedInput `json:"inputs"`
	Op     string         `json:"op"`
	Factor float64        `json:"factor,omitempty"`
}

type Metric struct {
	ID              string          `json:"id"`
	Ordinal         int             `json:"ordinal"`
	Kind            string          `json:"kind"` // raw | derived
	WindowMS        int64           `json:"window_ms"`
	AnchorMS        int64           `json:"anchor_ms"`
	SourceAlignment SourceAlignment `json:"source_alignment"`
	WeightScale     WeightScale     `json:"weight_scale"`
	WeightBits      int             `json:"weight_bits"`
	Functions       []Func          `json:"functions"`
	Fill            FillPolicy      `json:"fill"`
	Derivation      *Derivation     `json:"derivation,omitempty"`
}

type FunctionDef struct {
	Name string `json:"name"`
	Op   Func   `json:"op,omitempty"`
	Ref  string `json:"ref,omitempty"`
}

type FunctionTable struct {
	Organization string                   `json:"organization"` // flat | nested_tree
	Defs         map[string]FunctionDef   `json:"defs"`
	Groups       map[string][]FunctionDef `json:"groups,omitempty"`
}

type DegradationPolicy struct {
	MissingFillField string `json:"missing_fill_field"`
	FillLookback     int    `json:"fill_lookback"`
}

type Project struct {
	SchemaVersion SchemaVersion      `json:"schema_version"`
	ProjectID     string             `json:"project_id"`
	ConfigVersion string             `json:"config_version"`
	BaseTimeMS    int64              `json:"base_time_ms"`
	RangeStartMS  int64              `json:"range_start_ms"`
	RangeEndMS    int64              `json:"range_end_ms"`
	Tolerance     float64            `json:"tolerance"`
	Metrics       []Metric           `json:"metrics"`
	Series        map[string][]Point `json:"series"`
	Functions     FunctionTable      `json:"functions"`
	Degradation   DegradationPolicy  `json:"degradation"`
	// SourceDocument is retained only in memory for immutability checks.
	SourceDocument []byte `json:"-"`
}

func (p *Project) Metric(id string) (*Metric, bool) {
	for i := range p.Metrics {
		if p.Metrics[i].ID == id {
			return &p.Metrics[i], true
		}
	}
	return nil, false
}

func (p *Project) SortedMetrics() []Metric {
	out := append([]Metric(nil), p.Metrics...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Ordinal == out[j].Ordinal {
			return out[i].ID < out[j].ID
		}
		return out[i].Ordinal < out[j].Ordinal
	})
	return out
}

func (p *Project) SortedPoints(metricID string) []Point {
	pts := append([]Point(nil), p.Series[metricID]...)
	sort.SliceStable(pts, func(i, j int) bool {
		if pts[i].Seq == pts[j].Seq {
			return pts[i].TimestampMS < pts[j].TimestampMS
		}
		return pts[i].Seq < pts[j].Seq
	})
	return pts
}

func (p *Project) WindowCount(m Metric) int64 {
	if p.RangeEndMS <= p.RangeStartMS || m.WindowMS <= 0 {
		return 0
	}
	start := WindowStart(p.RangeStartMS, m.WindowMS, m.AnchorMS)
	last := WindowStart(p.RangeEndMS-1, m.WindowMS, m.AnchorMS)
	return (last-start)/m.WindowMS + 1
}

type WindowKey struct {
	MetricID    string `json:"metric_id"`
	WindowIndex int64  `json:"window_index"`
	StartMS     int64  `json:"start_ms"`
}

func (k WindowKey) String() string {
	return fmt.Sprintf("%s@%d", k.MetricID, k.StartMS)
}

type WeightAggregate struct {
	WeightedSumRaw float64 `json:"weighted_sum_raw"`
	WeightTotalRaw int64   `json:"weight_total_raw"`
	WeightedMean   float64 `json:"weighted_mean"`
	Scale          int64   `json:"canonical_scale"`
}

type FillInfo struct {
	SourceWindowStartMS int64            `json:"source_window_start_ms"`
	SourceWindowIndex   int64            `json:"source_window_index"`
	DistanceWindows     int              `json:"distance_windows"`
	Values              map[Func]float64 `json:"values,omitempty"`
}

type InvalidationSource struct {
	Kind             string   `json:"kind"` // initial | append | config | upstream | reused | checkpoint
	Reason           string   `json:"reason,omitempty"`
	ConfigVersion    string   `json:"config_version"`
	ChangedMetricID  string   `json:"changed_metric_id,omitempty"`
	ChangedField     string   `json:"changed_field,omitempty"`
	Sequence         int64    `json:"sequence,omitempty"`
	UpstreamMetricID string   `json:"upstream_metric_id,omitempty"`
	UpstreamWindowMS int64    `json:"upstream_window_start_ms,omitempty"`
	ClosurePath      []string `json:"closure_path,omitempty"`
}

type WindowResult struct {
	Key              WindowKey          `json:"key"`
	MetricID         string             `json:"metric_id"`
	WindowIndex      int64              `json:"ordinal_index"`
	StartMS          int64              `json:"start_ms"`
	EndMS            int64              `json:"end_ms"`
	PointCount       int                `json:"point_count"`
	Values           map[Func]float64   `json:"values,omitempty"`
	Weight           WeightAggregate    `json:"weight"`
	Status           WindowStatus       `json:"status"`
	Filled           bool               `json:"filled"`
	FillFieldPresent bool               `json:"fill_field_present"`
	FillDegraded     bool               `json:"fill_degraded"`
	FillAnnotation   string             `json:"fill_annotation,omitempty"`
	Fill             *FillInfo          `json:"fill,omitempty"`
	Invalidation     InvalidationSource `json:"invalidation"`
	ConfigVersion    string             `json:"config_version"`
}

func (r WindowResult) Clone() WindowResult {
	out := r
	if r.Values != nil {
		out.Values = make(map[Func]float64, len(r.Values))
		for k, v := range r.Values {
			out.Values[k] = v
		}
	}
	if r.Fill != nil {
		fill := *r.Fill
		if r.Fill.Values != nil {
			fill.Values = make(map[Func]float64, len(r.Fill.Values))
			for k, v := range r.Fill.Values {
				fill.Values[k] = v
			}
		}
		out.Fill = &fill
	}
	return out
}

// WindowStart returns the inclusive left edge under an anchor grid.
// The declared boundary convention is [start, end): a point exactly on the
// right edge belongs to the following window.
func WindowStart(ts, width, anchor int64) int64 {
	delta := ts - anchor
	q := floorDiv(delta, width)
	return anchor + q*width
}

func WindowBounds(ts, width, anchor int64) (int64, int64) {
	start := WindowStart(ts, width, anchor)
	return start, start + width
}

func floorDiv(a, b int64) int64 {
	q := a / b
	r := a % b
	if r != 0 && ((r < 0) != (b < 0)) {
		q--
	}
	return q
}

// AnchorChangeDecision is the declared adjudication for a grid window that
// straddles a logical alignment-baseline change point. Configuration edits
// invalidate the whole metric closure; this classifier makes that rule
// observable instead of silently mixing two grids in one result.
func AnchorChangeDecision(windowStart, windowEnd, changeMS int64) string {
	if windowStart < changeMS && changeMS < windowEnd {
		return "CROSSING_RECOMPUTE_UNDER_CURRENT_BASELINE"
	}
	if changeMS == windowStart {
		return "CHANGE_ON_LEFT_BOUNDARY_USE_NEW_GRID"
	}
	if changeMS == windowEnd {
		return "CHANGE_ON_RIGHT_BOUNDARY_USE_OLD_GRID"
	}
	return "NO_CROSSING"
}
