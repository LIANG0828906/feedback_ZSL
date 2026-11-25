package pipeline

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
)

const (
	DefaultSeed int64 = 20241126
	// SampleOriginMs is divisible by every sample step and therefore the epoch
	// aligned and first-data aligned encodings resolve to identical boundaries.
	SampleOriginMs int64 = 1_704_067_200_000
	SampleTickMs   int64 = 1_000
)

// GeneratedSample is the canonical, version-independent fixture.
type GeneratedSample struct {
	Seed int64    `json:"seed"`
	Base *Project `json:"base"`
}

type arrivalTemplate struct {
	tick          int64
	featureID     string
	watermarkTick int64
	late          bool
}

// GenerateSample deterministically creates the acceptance fixture. The normal
// observations are interleaved in event-time order. Three intentionally late
// observations exercise open-window correction, expired-window discards, and
// the watermark-disabled downgrade.
func GenerateSample(seed int64) (*GeneratedSample, error) {
	base := &Project{
		SchemaVersion:    "canonical",
		ProjectID:        fmt.Sprintf("featurepipe-demo-%d", seed),
		TimeOriginMs:     SampleOriginMs,
		TimeTickMs:       SampleTickMs,
		Alignment:        AlignEpoch,
		WeightScale:      WeightPPM,
		WatermarkEnabled: true,
		Features: []FeatureDefinition{
			{
				ID: "F_TRAFFIC", Name: "traffic-volume",
				WindowWidthMs: 20_000, WindowStepMs: 5_000, AllowedLatenessMs: 10_000,
				Aggregations: []Aggregation{
					{Kind: AggSum, Weight: NormalizedWeightScale},
					{Kind: AggAvg, Weight: NormalizedWeightScale},
					{Kind: AggCount, Weight: NormalizedWeightScale},
				},
			},
			{
				ID: "F_LATENCY", Name: "request-latency",
				WindowWidthMs: 10_000, WindowStepMs: 10_000, AllowedLatenessMs: 5_000,
				Aggregations: []Aggregation{
					{Kind: AggAvg, Weight: NormalizedWeightScale},
					{Kind: AggMin, Weight: NormalizedWeightScale},
					{Kind: AggMax, Weight: NormalizedWeightScale},
					{Kind: AggCount, Weight: NormalizedWeightScale},
				},
			},
			{
				ID: "F_SCORE", Name: "quality-score",
				WindowWidthMs: 30_000, WindowStepMs: 5_000, AllowedLatenessMs: 15_000,
				Aggregations: []Aggregation{
					{Kind: AggWeightedSum, Weight: 750_000},
					{Kind: AggAvg, Weight: 500_000},
					{Kind: AggCount, Weight: NormalizedWeightScale},
				},
			},
			{
				ID: "F_BLEND", Name: "traffic-latency-blend",
				WindowWidthMs: 30_000, WindowStepMs: 10_000, AllowedLatenessMs: 10_000,
				DerivedFrom: []string{"F_TRAFFIC", "F_LATENCY"},
				Aggregations: []Aggregation{
					{Kind: AggSum, Weight: NormalizedWeightScale},
					{Kind: AggAvg, Weight: NormalizedWeightScale},
					{Kind: AggCount, Weight: NormalizedWeightScale},
				},
			},
		},
	}

	freq := map[string]int64{
		"F_TRAFFIC": 5,
		"F_LATENCY": 10,
		"F_SCORE":   5,
		"F_BLEND":   10,
	}
	lastTick := map[string]int64{
		"F_TRAFFIC": 50,
		"F_LATENCY": 40,
		"F_SCORE":   35,
		"F_BLEND":   40,
	}

	templates := make([]arrivalTemplate, 0, 32)
	for _, feature := range base.Features {
		for tick := int64(0); tick <= lastTick[feature.ID]; tick += freq[feature.ID] {
			templates = append(templates, arrivalTemplate{tick: tick, featureID: feature.ID, watermarkTick: tick})
		}
	}
	sort.SliceStable(templates, func(i, j int) bool {
		if templates[i].tick != templates[j].tick {
			return templates[i].tick < templates[j].tick
		}
		return templates[i].featureID < templates[j].featureID
	})

	// A late F_SCORE observation is still inside its windows' allowed lateness.
	// It is inserted before the first normal event whose watermark is 40s.
	insertAccepted := func(t arrivalTemplate) {
		idx := sort.Search(len(templates), func(i int) bool {
			return templates[i].watermarkTick > t.watermarkTick
		})
		templates = append(templates, arrivalTemplate{})
		copy(templates[idx+1:], templates[idx:])
		templates[idx] = t
	}
	insertAccepted(arrivalTemplate{tick: 10, featureID: "F_SCORE", watermarkTick: 35, late: true})
	insertAccepted(arrivalTemplate{tick: 10, featureID: "F_BLEND", watermarkTick: 40, late: true})

	// Two observations arrive after their candidate windows are fully closed.
	// They are deterministic discards whenever watermark processing is enabled.
	templates = append(templates,
		arrivalTemplate{tick: 5, featureID: "F_TRAFFIC", watermarkTick: 50, late: true},
		arrivalTemplate{tick: 10, featureID: "F_LATENCY", watermarkTick: 50, late: true},
	)

	rng := rand.New(rand.NewSource(seed))
	channels := []string{"edge", "mobile", "web", "partner"}
	base.Events = make([]ObservationEvent, 0, len(templates))
	for i, tmpl := range templates {
		seq := int64(i + 1)
		value := int64(rng.Intn(20_001) - 10_000)
		channel := channels[rng.Intn(len(channels))]
		wm := SampleOriginMs + tmpl.watermarkTick*SampleTickMs
		wmCopy := wm
		base.Events = append(base.Events, ObservationEvent{
			Seq:           seq,
			FeatureID:     tmpl.featureID,
			ObservedAtMs:  SampleOriginMs + tmpl.tick*SampleTickMs,
			Channel:       channel,
			Value:         value,
			WatermarkMs:   &wmCopy,
			WatermarkTick: ptrInt64(tmpl.watermarkTick),
		})
	}
	return &GeneratedSample{Seed: seed, Base: base}, nil
}

// GenerateDefault uses the documented fixed acceptance seed.
func GenerateDefault() (*GeneratedSample, error) { return GenerateSample(DefaultSeed) }

func ptrInt64(v int64) *int64 { return &v }

// VersionDocuments emits v1, v2, and v3 JSON encodings of the same fixture.
func (s *GeneratedSample) VersionDocuments() (map[string][]byte, error) {
	out := make(map[string][]byte, 3)
	for _, version := range []string{"v1", "v2", "v3"} {
		b, err := s.VersionDocument(version)
		if err != nil {
			return nil, err
		}
		out[version] = b
	}
	return out, nil
}

// VersionDocument emits one version of the fixture.
func (s *GeneratedSample) VersionDocument(version string) ([]byte, error) {
	switch version {
	case "v1":
		return json.MarshalIndent(s.v1Document(), "", "  ")
	case "v2":
		return json.MarshalIndent(s.v2Document(), "", "  ")
	case "v3":
		return json.MarshalIndent(s.v3Document(), "", "  ")
	default:
		return nil, NewBusinessError(CodeSchemaUnsupported, "unknown document version %q", version).
			WithContext("version", version)
	}
}

type docV1 struct {
	SchemaVersion    string      `json:"schema_version"`
	ProjectID        string      `json:"project_id"`
	TimeOriginMs     int64       `json:"time_origin_ms"`
	TimeUnitMs       int64       `json:"time_unit_ms"`
	Alignment        string      `json:"alignment"`
	WeightScale      string      `json:"weight_scale"`
	WatermarkEnabled bool        `json:"watermark_enabled"`
	Features         []featureV1 `json:"features"`
	Events           []eventV1   `json:"events"`
}

type featureV1 struct {
	FeatureID         string          `json:"feature_id"`
	Name              string          `json:"name,omitempty"`
	WindowWidthMs     int64           `json:"window_width_ms"`
	WindowStepMs      int64           `json:"window_step_ms"`
	AllowedLatenessMs int64           `json:"allowed_lateness_ms"`
	Aggregations      []aggregationV1 `json:"aggregations"`
	DerivedFrom       []string        `json:"derived_from,omitempty"`
}

type aggregationV1 struct {
	Function       string `json:"function"`
	WeightPermille int64  `json:"weight_permille"`
}

type eventV1 struct {
	ArrivalSeq   int64  `json:"arrival_seq"`
	FeatureID    string `json:"feature_id"`
	ObservedAtMs int64  `json:"observed_at_ms"`
	Channel      string `json:"channel"`
	Value        int64  `json:"value"`
	WatermarkMs  *int64 `json:"watermark_ms,omitempty"`
}

func (s *GeneratedSample) v1Document() docV1 {
	d := docV1{
		SchemaVersion: "v1", ProjectID: s.Base.ProjectID,
		TimeOriginMs: s.Base.TimeOriginMs, TimeUnitMs: s.Base.TimeTickMs,
		Alignment: "epoch", WeightScale: "per_mille", WatermarkEnabled: false,
	}
	for _, f := range s.Base.Features {
		nf := featureV1{
			FeatureID: f.ID, Name: f.Name, WindowWidthMs: f.WindowWidthMs,
			WindowStepMs: f.WindowStepMs, AllowedLatenessMs: f.AllowedLatenessMs,
			DerivedFrom: append([]string(nil), f.DerivedFrom...),
		}
		for _, a := range f.Aggregations {
			nf.Aggregations = append(nf.Aggregations, aggregationV1{
				Function: string(a.Kind), WeightPermille: a.Weight / 1_000,
			})
		}
		d.Features = append(d.Features, nf)
	}
	for _, e := range s.Base.Events {
		d.Events = append(d.Events, eventV1{
			ArrivalSeq: e.Seq, FeatureID: e.FeatureID, ObservedAtMs: e.ObservedAtMs,
			Channel: e.Channel, Value: e.Value,
		})
	}
	return d
}

type docV2 struct {
	SchemaVersion    string      `json:"schemaVersion"`
	ProjectID        string      `json:"projectId"`
	TimeOriginMs     int64       `json:"timeOriginMs"`
	TimeUnitMs       int64       `json:"timeUnitMs"`
	Alignment        string      `json:"alignment"`
	WeightScale      string      `json:"weightScale"`
	WatermarkEnabled bool        `json:"watermarkEnabled"`
	Features         []featureV2 `json:"features"`
	Events           []eventV2   `json:"events"`
}

type featureV2 struct {
	FeatureID       string          `json:"featureId"`
	Name            string          `json:"name,omitempty"`
	WindowWidth     int64           `json:"windowWidth"`
	WindowStep      int64           `json:"windowStep"`
	AllowedLateness int64           `json:"allowedLateness"`
	Aggregations    []aggregationV2 `json:"aggregations"`
	DerivedFrom     []string        `json:"derivedFrom,omitempty"`
}

type aggregationV2 struct {
	Function        string `json:"fn"`
	WeightPerMyriad int64  `json:"weightPerMyriad"`
}

type eventV2 struct {
	ArrivalSeq    int64  `json:"arrivalSeq"`
	FeatureID     string `json:"featureId"`
	ObservedTick  int64  `json:"observedTick"`
	ChannelID     string `json:"channelId"`
	Sample        int64  `json:"sample"`
	WatermarkTick *int64 `json:"watermarkTick,omitempty"`
}

func (s *GeneratedSample) v2Document() docV2 {
	d := docV2{
		SchemaVersion: "v2", ProjectID: s.Base.ProjectID,
		TimeOriginMs: s.Base.TimeOriginMs, TimeUnitMs: s.Base.TimeTickMs,
		Alignment: "first_data", WeightScale: "per_myriad", WatermarkEnabled: true,
	}
	for _, f := range s.Base.Features {
		nf := featureV2{
			FeatureID: f.ID, Name: f.Name, WindowWidth: f.WindowWidthMs, WindowStep: f.WindowStepMs,
			AllowedLateness: f.AllowedLatenessMs,
			DerivedFrom:     append([]string(nil), f.DerivedFrom...),
		}
		for _, a := range f.Aggregations {
			nf.Aggregations = append(nf.Aggregations, aggregationV2{
				Function: string(a.Kind), WeightPerMyriad: a.Weight / 100,
			})
		}
		d.Features = append(d.Features, nf)
	}
	for _, e := range s.Base.Events {
		tick := (e.ObservedAtMs - s.Base.TimeOriginMs) / s.Base.TimeTickMs
		var wm *int64
		if e.WatermarkMs != nil {
			v := (*e.WatermarkMs - s.Base.TimeOriginMs) / s.Base.TimeTickMs
			wm = &v
		}
		d.Events = append(d.Events, eventV2{
			ArrivalSeq: e.Seq, FeatureID: e.FeatureID, ObservedTick: tick,
			ChannelID: e.Channel, Sample: e.Value, WatermarkTick: wm,
		})
	}
	return d
}

type docV3 struct {
	SchemaVersion    string      `json:"schema_version"`
	ProjectID        string      `json:"project_id"`
	EpochOriginMs    int64       `json:"epoch_origin_ms"`
	EventTickMs      int64       `json:"event_tick_ms"`
	WindowAlignment  string      `json:"window_alignment"`
	WeightScale      string      `json:"weight_scale"`
	WatermarkEnabled bool        `json:"watermark_enabled"`
	Features         []featureV3 `json:"features"`
	Events           []eventV3   `json:"events"`
}

type featureV3 struct {
	ID          string          `json:"id"`
	Name        string          `json:"name,omitempty"`
	WinMs       int64           `json:"win_ms"`
	StrideMs    int64           `json:"stride_ms"`
	LateAllowMs int64           `json:"late_allow_ms"`
	Aggs        []aggregationV3 `json:"aggs"`
	Parents     []string        `json:"parent_features,omitempty"`
}

type aggregationV3 struct {
	Kind   string `json:"kind"`
	Weight int64  `json:"weight"`
}

type eventV3 struct {
	Seq     int64  `json:"seq"`
	Feature string `json:"feature"`
	EpochMs int64  `json:"epoch_ms"`
	Channel string `json:"channel"`
	Value   int64  `json:"v"`
	WM      *int64 `json:"wm_ms,omitempty"`
}

func (s *GeneratedSample) v3Document() docV3 {
	d := docV3{
		SchemaVersion: "v3", ProjectID: s.Base.ProjectID,
		EpochOriginMs: s.Base.TimeOriginMs, EventTickMs: s.Base.TimeTickMs,
		WindowAlignment: "first_data", WeightScale: "per_mille", WatermarkEnabled: true,
	}
	for _, f := range s.Base.Features {
		nf := featureV3{
			ID: f.ID, Name: f.Name, WinMs: f.WindowWidthMs, StrideMs: f.WindowStepMs,
			LateAllowMs: f.AllowedLatenessMs, Parents: append([]string(nil), f.DerivedFrom...),
		}
		for _, a := range f.Aggregations {
			nf.Aggs = append(nf.Aggs, aggregationV3{Kind: string(a.Kind), Weight: a.Weight / 1_000})
		}
		d.Features = append(d.Features, nf)
	}
	for _, e := range s.Base.Events {
		d.Events = append(d.Events, eventV3{
			Seq: e.Seq, Feature: e.FeatureID, EpochMs: e.ObservedAtMs,
			Channel: e.Channel, Value: e.Value, WM: e.WatermarkMs,
		})
	}
	return d
}
