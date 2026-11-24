package domain

import "time"

const (
	SchemaVersion       = "expression-workspace/v1"
	AlgorithmVersion    = "overlay-engine/1"
	DefaultParamVersion = "parameters/1"

	InterpolationLinear     = "linear"
	InterpolationHold       = "hold"
	InterpolationSmoothstep = "smoothstep"

	TargetVelocity  = "velocity"
	TargetVibrato   = "vibrato"
	TargetPitchbend = "pitchbend"
	TargetSustain   = "sustain"
)

// Project is the deterministic input for a performance expression workspace.
// SamplesPerBeat defines the truth and playback sample grid.
type Project struct {
	SchemaVersion  string    `json:"schema_version"`
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Seed           int64     `json:"seed"`
	SamplesPerBeat int       `json:"samples_per_beat"`
	DurationBeats  float64   `json:"duration_beats"`
	Segments       []Segment `json:"segments"`
}

type Segment struct {
	ID          string         `json:"id"`
	StartBeat   float64        `json:"start_beat"`
	EndBeat     float64        `json:"end_beat"`
	TempoBPM    float64        `json:"tempo_bpm"`
	NoteDensity float64        `json:"note_density"`
	Channels    []Channel      `json:"channels"`
	Priorities  []PriorityRule `json:"priorities"`
	Truth       Truth          `json:"truth"`
}

type Channel struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Target string  `json:"target"`
	Layers []Layer `json:"layers"`
}

type Layer struct {
	ID     string         `json:"id"`
	Points []ControlPoint `json:"points"`
}

type ControlPoint struct {
	Beat  float64 `json:"beat"`
	Value float64 `json:"value"`
}

type PriorityRule struct {
	Higher string `json:"higher"`
	Lower  string `json:"lower"`
	Basis  string `json:"basis"`
}

type Truth struct {
	Interpolation string        `json:"interpolation"`
	Tolerance     float64       `json:"tolerance"`
	Samples       []TruthSample `json:"samples"`
}

type TruthSample struct {
	Tick      int                `json:"tick"`
	Beat      float64            `json:"beat"`
	Values    map[string]float64 `json:"values"`
	Sources   map[string]string  `json:"sources"`
	Decisions []TruthDecision    `json:"decisions,omitempty"`
}

type TruthDecision struct {
	Tick            int     `json:"tick"`
	Beat            float64 `json:"beat"`
	Target          string  `json:"target"`
	Higher          string  `json:"higher"`
	Overridden      string  `json:"overridden"`
	OverriddenValue float64 `json:"overridden_value"`
	Basis           string  `json:"basis"`
}

// Parameters are injected at runtime and are versioned for checkpoint validity.
type Parameters struct {
	Interpolation    string  `json:"interpolation"`
	Tolerance        float64 `json:"tolerance"`
	ParamVersion     string  `json:"param_version"`
	AlgorithmVersion string  `json:"algorithm_version"`
}

func DefaultParameters() Parameters {
	return Parameters{
		Interpolation:    InterpolationLinear,
		Tolerance:        1e-9,
		ParamVersion:     DefaultParamVersion,
		AlgorithmVersion: AlgorithmVersion,
	}
}

type SourceRef struct {
	Channel  string  `json:"channel"`
	Priority int     `json:"priority"`
	Value    float64 `json:"value"`
}

type Decision struct {
	Tick            int     `json:"tick"`
	Beat            float64 `json:"beat"`
	Target          string  `json:"target"`
	Channel         string  `json:"channel"`
	Overridden      string  `json:"overridden"`
	OverriddenValue float64 `json:"overridden_value"`
	Basis           string  `json:"basis"`
}

type SampleResult struct {
	Tick      int                  `json:"tick"`
	Beat      float64              `json:"beat"`
	Values    map[string]float64   `json:"values"`
	Sources   map[string]SourceRef `json:"sources"`
	Decisions []Decision           `json:"decisions,omitempty"`
}

type SegmentResult struct {
	SegmentID       string         `json:"segment_id"`
	StartBeat       float64        `json:"start_beat"`
	EndBeat         float64        `json:"end_beat"`
	PriorityVersion string         `json:"priority_version"`
	Samples         []SampleResult `json:"samples"`
}

type ResultSet struct {
	ProjectID        string          `json:"project_id"`
	ParamVersion     string          `json:"param_version"`
	AlgorithmVersion string          `json:"algorithm_version"`
	Interpolation    string          `json:"interpolation"`
	Tolerance        float64         `json:"tolerance"`
	Segments         []SegmentResult `json:"segments"`
	GeneratedAt      time.Time       `json:"generated_at"`
}

type SegmentSnapshot struct {
	SegmentID   string         `json:"segment_id"`
	StartBeat   float64        `json:"start_beat"`
	EndBeat     float64        `json:"end_beat"`
	TempoBPM    float64        `json:"tempo_bpm"`
	NoteDensity float64        `json:"note_density"`
	Channels    []Channel      `json:"channels"`
	Priorities  []PriorityRule `json:"priorities"`
}

type ResultSummary struct {
	SegmentID     string `json:"segment_id"`
	ResultHash    string `json:"result_hash"`
	SampleCount   int    `json:"sample_count"`
	DecisionCount int    `json:"decision_count"`
}

type Checkpoint struct {
	ProjectID        string          `json:"project_id"`
	BatchID          string          `json:"batch_id"`
	SegmentID        string          `json:"segment_id"`
	Snapshot         SegmentSnapshot `json:"snapshot"`
	Summary          ResultSummary   `json:"summary"`
	PriorityVersion  string          `json:"priority_version"`
	ContentHash      string          `json:"content_hash"`
	ParamVersion     string          `json:"param_version"`
	AlgorithmVersion string          `json:"algorithm_version"`
	Operations       []EditOp        `json:"operations"`
	OperationsHash   string          `json:"operations_hash"`
}

type EditKind string

const (
	EditMovePoint   EditKind = "move_point"
	EditAddLayer    EditKind = "add_layer"
	EditRemoveLayer EditKind = "remove_layer"
	EditSetPriority EditKind = "set_priority"
)

// EditOp is serializable so checkpoint recovery can deterministically replay
// committed batches from an empty working state.
type EditOp struct {
	Kind       EditKind       `json:"kind"`
	SegmentID  string         `json:"segment_id"`
	ChannelID  string         `json:"channel_id,omitempty"`
	LayerID    string         `json:"layer_id,omitempty"`
	PointIndex int            `json:"point_index,omitempty"`
	Beat       float64        `json:"beat,omitempty"`
	Value      float64        `json:"value,omitempty"`
	Target     string         `json:"target,omitempty"`
	Layer      *Layer         `json:"layer,omitempty"`
	Priorities []PriorityRule `json:"priorities,omitempty"`
	BatchID    string         `json:"batch_id,omitempty"`
}

type CommitReceipt struct {
	BatchID     string    `json:"batch_id"`
	SegmentIDs  []string  `json:"segment_ids"`
	Sequence    int64     `json:"sequence"`
	CommittedAt time.Time `json:"committed_at"`
}

type SegmentState struct {
	SegmentID string    `json:"segment_id"`
	Dirty     bool      `json:"dirty"`
	Failed    bool      `json:"failed"`
	LastError *AppError `json:"last_error,omitempty"`
	Committed int64     `json:"committed"`
}

// CloneProject returns a deep copy. Callers never receive aliases into session
// state, which is essential for working-copy isolation.
func CloneProject(p Project) Project {
	out := p
	out.Segments = make([]Segment, len(p.Segments))
	for i, seg := range p.Segments {
		out.Segments[i] = CloneSegment(seg)
	}
	return out
}

func CloneSegment(seg Segment) Segment {
	out := seg
	out.Channels = make([]Channel, len(seg.Channels))
	for i, ch := range seg.Channels {
		out.Channels[i] = CloneChannel(ch)
	}
	out.Priorities = append([]PriorityRule(nil), seg.Priorities...)
	out.Truth = CloneTruth(seg.Truth)
	return out
}

func CloneChannel(ch Channel) Channel {
	out := ch
	out.Layers = make([]Layer, len(ch.Layers))
	for i, layer := range ch.Layers {
		out.Layers[i] = Layer{
			ID:     layer.ID,
			Points: append([]ControlPoint(nil), layer.Points...),
		}
	}
	return out
}

func CloneTruth(t Truth) Truth {
	out := t
	out.Samples = make([]TruthSample, len(t.Samples))
	for i, sample := range t.Samples {
		out.Samples[i] = TruthSample{
			Tick:      sample.Tick,
			Beat:      sample.Beat,
			Values:    cloneFloatMap(sample.Values),
			Sources:   cloneStringMap(sample.Sources),
			Decisions: append([]TruthDecision(nil), sample.Decisions...),
		}
	}
	return out
}

func cloneFloatMap(in map[string]float64) map[string]float64 {
	if in == nil {
		return nil
	}
	out := make(map[string]float64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (p Project) SegmentByID(id string) (Segment, int, bool) {
	for i, seg := range p.Segments {
		if seg.ID == id {
			return seg, i, true
		}
	}
	return Segment{}, -1, false
}

func (s Segment) ChannelByID(id string) (Channel, int, bool) {
	for i, ch := range s.Channels {
		if ch.ID == id {
			return ch, i, true
		}
	}
	return Channel{}, -1, false
}
