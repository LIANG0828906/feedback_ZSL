package model

import (
	"sort"
)

const (
	SchemaV1 = "v1"
	SchemaV2 = "v2"
	SchemaV3 = "v3"

	SourceAlert    = "alert"
	SourceChange   = "change"
	SourceHealth   = "health"
	SourceCapacity = "capacity"
	SourceRecovery = "recovery"
)

// Project is the only normalized representation used by domain algorithms.
// All timestamps and durations are integer milliseconds on one global axis.
type Project struct {
	ID       string         `json:"id"`
	Revision string         `json:"revision"`
	Seed     int64          `json:"seed"`
	Schema   string         `json:"schema,omitempty"`
	Window   Window         `json:"window"`
	Policy   Policy         `json:"policy"`
	Services []Service      `json:"services"`
	Edges    []Edge         `json:"edges"`
	Events   []Event        `json:"events"`
	Truth    *ExpectedTruth `json:"truth,omitempty"`
}

type Window struct {
	StartMS int64 `json:"start_ms"`
	EndMS   int64 `json:"end_ms"`
	TickMS  int64 `json:"tick_ms"`
}

type Policy struct {
	DedupToleranceMS int64 `json:"dedup_tolerance_ms"`
	MergeGapMS       int64 `json:"merge_gap_ms"`
	LookbackMS       int64 `json:"lookback_ms"`
	PropagationGen   int   `json:"propagation_gen"`
	FixedScale       int64 `json:"fixed_scale"`
	FixedWidthBits   int   `json:"fixed_width_bits"`
	ScoreTolerance   int64 `json:"score_tolerance_raw"`
}

type Service struct {
	ID                    string `json:"id"`
	Name                  string `json:"name"`
	ErrorThresholdBP      int    `json:"error_threshold_bp"`
	SaturationThresholdBP int    `json:"saturation_threshold_bp"`
	BaselineErrorBP       int    `json:"baseline_error_bp"`
	BaselineSaturationBP  int    `json:"baseline_saturation_bp"`
}

type Edge struct {
	// Caller calls Callee. A fault on Callee propagates to Caller/dependents.
	Caller string `json:"caller"`
	Callee string `json:"callee"`
}

type Event struct {
	ID           string         `json:"id"`
	Seq          int            `json:"seq"`
	Source       string         `json:"source"`
	ServiceID    string         `json:"service_id"`
	Kind         string         `json:"kind"`
	Fingerprint  string         `json:"fingerprint"`
	OccurredMS   int64          `json:"occurred_ms"`
	IngestLagMS  int64          `json:"ingest_lag_ms"`
	AliveMS      int64          `json:"alive_ms"`
	ChangeType   string         `json:"change_type,omitempty"`
	TypeWeightBP int            `json:"type_weight_bp"`
	ErrorRateBP  *int           `json:"error_rate_bp,omitempty"`
	SaturationBP *int           `json:"saturation_bp,omitempty"`
	RecoveryFor  string         `json:"recovery_for,omitempty"`
	Raw          map[string]any `json:"raw,omitempty"`
}

type ExpectedTruth struct {
	AlignedEvents []TruthEvent     `json:"aligned_events"`
	Intervals     []TruthInterval  `json:"intervals"`
	Candidates    []TruthCandidate `json:"candidates"`
}

type TruthEvent struct {
	ID             string   `json:"id"`
	KeptID         string   `json:"kept_id"`
	EffectiveMS    int64    `json:"effective_ms"`
	ServiceID      string   `json:"service_id"`
	DedupedCount   int      `json:"deduped_count"`
	MergedRawCount int      `json:"merged_raw_count"`
	MergedIDs      []string `json:"merged_ids,omitempty"`
}

type TruthInterval struct {
	Ordinal     int      `json:"ordinal"`
	ServiceID   string   `json:"service_id"`
	StartMS     int64    `json:"start_ms"`
	EndMS       int64    `json:"end_ms"`
	EvidenceIDs []string `json:"evidence_ids"`
	ThresholdBP int      `json:"threshold_bp"`
	Metric      string   `json:"metric"`
}

type TruthCandidate struct {
	EventID  string `json:"event_id"`
	ScoreRaw int64  `json:"score_raw"`
	Rank     int    `json:"rank"`
}

func (p *Project) ServiceIDs() []string {
	out := make([]string, 0, len(p.Services))
	for _, svc := range p.Services {
		out = append(out, svc.ID)
	}
	sort.Strings(out)
	return out
}

func (p *Project) Service(id string) (Service, bool) {
	for _, svc := range p.Services {
		if svc.ID == id {
			return svc, true
		}
	}
	return Service{}, false
}

func (p *Project) SortedEvents() []Event {
	out := append([]Event(nil), p.Events...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Seq != out[j].Seq {
			return out[i].Seq < out[j].Seq
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (p *Project) EdgesSorted() []Edge {
	out := append([]Edge(nil), p.Edges...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Caller != out[j].Caller {
			return out[i].Caller < out[j].Caller
		}
		return out[i].Callee < out[j].Callee
	})
	return out
}
