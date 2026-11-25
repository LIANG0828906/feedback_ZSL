package pipeline

import (
	"encoding/json"
	"fmt"
	"sort"
)

const (
	// ValueScale keeps event samples in thousandths. Aggregates using samples
	// (sum, avg, min, max) preserve this scale.
	ValueScale int64 = 1000

	// NormalizedWeightScale is the common weight scale used by every schema
	// version after loading. A source weight of 1.0 is represented as 1_000_000.
	NormalizedWeightScale int64 = 1_000_000

	EngineVersion = "featurepipe.engine/v1"
)

// AggKind is the closed set of aggregate functions understood by the engine.
type AggKind string

const (
	AggSum         AggKind = "sum"
	AggCount       AggKind = "count"
	AggAvg         AggKind = "avg"
	AggWeightedSum AggKind = "weighted_sum"
	AggMin         AggKind = "min"
	AggMax         AggKind = "max"
)

func (a AggKind) Valid() bool {
	switch a {
	case AggSum, AggCount, AggAvg, AggWeightedSum, AggMin, AggMax:
		return true
	default:
		return false
	}
}

// Aggregation binds an aggregate function to a normalized weight.
type Aggregation struct {
	Kind   AggKind `json:"kind"`
	Weight int64   `json:"weight"`
}

// WindowAlignment distinguishes the two accepted source window bases.
type WindowAlignment string

const (
	AlignEpoch     WindowAlignment = "epoch"
	AlignFirstData WindowAlignment = "first_data"
)

func (a WindowAlignment) Valid() bool {
	return a == AlignEpoch || a == AlignFirstData
}

// WeightScale names the source fixed-point scale.
type WeightScale string

const (
	WeightPerMille  WeightScale = "per_mille"
	WeightPerMyriad WeightScale = "per_myriad"
	WeightPPM       WeightScale = "ppm"
)

func (s WeightScale) Valid() bool {
	return s == WeightPerMille || s == WeightPerMyriad || s == WeightPPM
}

// FeatureDefinition is the normalized feature definition.
type FeatureDefinition struct {
	ID                string        `json:"id"`
	Name              string        `json:"name,omitempty"`
	WindowWidthMs     int64         `json:"window_width_ms"`
	WindowStepMs      int64         `json:"window_step_ms"`
	AllowedLatenessMs int64         `json:"allowed_lateness_ms"`
	Aggregations      []Aggregation `json:"aggregations"`
	DerivedFrom       []string      `json:"derived_from,omitempty"`
}

// ObservationEvent is the normalized event stream item. Seq is the global
// logical arrival sequence and is one-based.
type ObservationEvent struct {
	Seq           int64  `json:"seq"`
	FeatureID     string `json:"feature_id"`
	ObservedAtMs  int64  `json:"observed_at_ms"`
	Channel       string `json:"channel"`
	Value         int64  `json:"value"`
	WatermarkMs   *int64 `json:"watermark_ms,omitempty"`
	WatermarkTick *int64 `json:"-"`
}

// Project is the version-independent model produced by every schema loader.
type Project struct {
	SchemaVersion    string              `json:"schema_version"`
	ProjectID        string              `json:"project_id"`
	TimeOriginMs     int64               `json:"time_origin_ms"`
	TimeTickMs       int64               `json:"time_tick_ms"`
	Alignment        WindowAlignment     `json:"alignment"`
	WeightScale      WeightScale         `json:"weight_scale"`
	WatermarkEnabled bool                `json:"watermark_enabled"`
	Features         []FeatureDefinition `json:"features"`
	Events           []ObservationEvent  `json:"events"`
}

// Clone returns a deep copy suitable for edits and invalidation tests.
func (p *Project) Clone() *Project {
	if p == nil {
		return nil
	}
	cp := *p
	cp.Features = make([]FeatureDefinition, len(p.Features))
	for i := range p.Features {
		cp.Features[i] = p.Features[i]
		cp.Features[i].Aggregations = append([]Aggregation(nil), p.Features[i].Aggregations...)
		cp.Features[i].DerivedFrom = append([]string(nil), p.Features[i].DerivedFrom...)
	}
	cp.Events = make([]ObservationEvent, len(p.Events))
	for i := range p.Events {
		cp.Events[i] = p.Events[i]
		if p.Events[i].WatermarkMs != nil {
			v := *p.Events[i].WatermarkMs
			cp.Events[i].WatermarkMs = &v
		}
		if p.Events[i].WatermarkTick != nil {
			v := *p.Events[i].WatermarkTick
			cp.Events[i].WatermarkTick = &v
		}
	}
	return &cp
}

// FeatureByID returns a feature definition.
func (p *Project) FeatureByID(id string) (FeatureDefinition, bool) {
	for _, f := range p.Features {
		if f.ID == id {
			return f, true
		}
	}
	return FeatureDefinition{}, false
}

// SortEvents establishes the global arrival order used by all components.
func (p *Project) SortEvents() {
	sort.Slice(p.Events, func(i, j int) bool { return p.Events[i].Seq < p.Events[j].Seq })
}

// EventBySeq locates an event by its arrival sequence.
func (p *Project) EventBySeq(seq int64) (ObservationEvent, bool) {
	idx := sort.Search(len(p.Events), func(i int) bool { return p.Events[i].Seq >= seq })
	if idx < len(p.Events) && p.Events[idx].Seq == seq {
		return p.Events[idx], true
	}
	return ObservationEvent{}, false
}

// WindowKey identifies a fixed-width sliding window.
type WindowKey struct {
	FeatureID string `json:"feature_id"`
	StartMs   int64  `json:"start_ms"`
	EndMs     int64  `json:"end_ms"`
}

func (k WindowKey) String() string {
	return fmt.Sprintf("%s@%d-%d", k.FeatureID, k.StartMs, k.EndMs)
}

// WindowStatus is persisted with every window checkpoint.
type WindowStatus string

const (
	WindowOpen      WindowStatus = "open"
	WindowCommitted WindowStatus = "committed"
)

// AggregateResult is one aggregate value. Values are fixed-point at
// ValueScale; count is an integer count.
type AggregateResult struct {
	Kind   AggKind `json:"kind"`
	Weight int64   `json:"weight"`
	Value  int64   `json:"value"`
}

// WindowResult is the externally visible window output.
type WindowResult struct {
	Key              WindowKey         `json:"key"`
	Status           WindowStatus      `json:"status"`
	EventSeqs        []int64           `json:"event_seqs"`
	ObservationCount int               `json:"observation_count"`
	Aggregates       []AggregateResult `json:"aggregates"`
	RecomputeCount   int64             `json:"recompute_count"`
	Traversal        TraversalMode     `json:"traversal"`
	PathReasonCode   string            `json:"path_reason_code"`
}

// Aggregate returns an aggregate by kind.
func (w WindowResult) Aggregate(kind AggKind) (AggregateResult, bool) {
	for _, a := range w.Aggregates {
		if a.Kind == kind {
			return a, true
		}
	}
	return AggregateResult{}, false
}

// DiscardRecord captures one per-window late-event rejection.
type DiscardRecord struct {
	EventSeq     int64     `json:"event_seq"`
	FeatureID    string    `json:"feature_id"`
	Window       WindowKey `json:"window"`
	ObservedAtMs int64     `json:"observed_at_ms"`
	WatermarkMs  int64     `json:"watermark_ms"`
	LateByMs     int64     `json:"late_by_ms"`
	ReasonCode   string    `json:"reason_code"`
}

// TraversalMode is the selected fixed-window execution strategy.
type TraversalMode string

const (
	TraversalCumulative TraversalMode = "cumulative"
	TraversalRescan     TraversalMode = "rescan"
)

// PathDecision records why a traversal mode was selected.
type PathDecision struct {
	FeatureID  string        `json:"feature_id"`
	WidthMs    int64         `json:"width_ms"`
	StepMs     int64         `json:"step_ms"`
	RatioNum   int64         `json:"ratio_num"`
	RatioDen   int64         `json:"ratio_den"`
	Threshold  int64         `json:"threshold"`
	Mode       TraversalMode `json:"mode"`
	ReasonCode string        `json:"reason_code"`
}

// RunResult is deterministic for a project, options that do not alter
// semantics, and the selected traversal path.
type RunResult struct {
	ProjectID          string          `json:"project_id"`
	SchemaVersion      string          `json:"schema_version"`
	WatermarkEnabled   bool            `json:"watermark_enabled"`
	WatermarkApplied   bool            `json:"watermark_applied"`
	WatermarkNote      string          `json:"watermark_note"`
	PathDecisions      []PathDecision  `json:"path_decisions"`
	Windows            []WindowResult  `json:"windows"`
	Discards           []DiscardRecord `json:"discards"`
	RecomputeTotal     int64           `json:"recompute_total"`
	WorkersUsed        int             `json:"workers_used"`
	BufferLimit        int             `json:"buffer_limit"`
	LastCommittedSeq   int64           `json:"last_committed_seq"`
	ResumedFromSeq     int64           `json:"resumed_from_seq"`
	CheckpointFallback bool            `json:"checkpoint_fallback"`
	CheckpointNote     string          `json:"checkpoint_note,omitempty"`
	Interrupted        bool            `json:"interrupted"`
}

// WindowByKey returns a result by key.
func (r *RunResult) WindowByKey(key WindowKey) (WindowResult, bool) {
	for _, w := range r.Windows {
		if w.Key == key {
			return w, true
		}
	}
	return WindowResult{}, false
}

// CanonicalIdentity is the structural portion that must agree across schema
// versions: normalized features, logical events, and absolute window bounds.
// Optional watermark values and source alignment are not part of this
// structural identity; run results expose their runtime effect explicitly.
type CanonicalIdentity struct {
	ProjectID    string              `json:"project_id"`
	Features     []FeatureDefinition `json:"features"`
	Events       []ObservationEvent  `json:"events"`
	WindowBounds []WindowKey         `json:"window_bounds"`
}

// Identity builds a deterministic comparison value for tests and diagnostics.
func (p *Project) Identity() (CanonicalIdentity, error) {
	features := make([]FeatureDefinition, 0, len(p.Features))
	for _, f := range p.Features {
		copyF := f
		copyF.Aggregations = append([]Aggregation(nil), f.Aggregations...)
		copyF.DerivedFrom = append([]string(nil), f.DerivedFrom...)
		features = append(features, copyF)
	}
	events := make([]ObservationEvent, 0, len(p.Events))
	for _, e := range p.Events {
		copyE := e
		copyE.WatermarkMs = nil
		copyE.WatermarkTick = nil
		events = append(events, copyE)
	}
	bounds, err := WindowPlans(p)
	if err != nil {
		return CanonicalIdentity{}, err
	}
	keys := make([]WindowKey, 0)
	for _, plan := range bounds {
		keys = append(keys, plan.Windows...)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].FeatureID != keys[j].FeatureID {
			return keys[i].FeatureID < keys[j].FeatureID
		}
		return keys[i].StartMs < keys[j].StartMs
	})
	return CanonicalIdentity{
		ProjectID: p.ProjectID, Features: features, Events: events, WindowBounds: keys,
	}, nil
}

// IdentityJSON is used for deterministic diagnostics.
func (p *Project) IdentityJSON() string {
	b, _ := json.MarshalIndent(mustIdentity(p), "", "  ")
	return string(b)
}

func mustIdentity(p *Project) CanonicalIdentity {
	id, err := p.Identity()
	if err != nil {
		return CanonicalIdentity{ProjectID: p.ProjectID}
	}
	return id
}
