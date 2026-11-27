package stage

import (
	"encoding/json"
	"fmt"
	"sort"
)

const (
	InternalSchema                = "stage.internal/1"
	AlgorithmVersion              = "cone-kernel-v1"
	DefaultParameterVersion       = "params-v1"
	DefaultProjectID              = "stage-demo"
	DefaultSampleSeed       int64 = 42

	CCTMinKelvin int64 = 1800
	CCTMaxKelvin int64 = 12000
)

type Vec3 [3]int64

func (v Vec3) Add(o Vec3) Vec3 {
	return Vec3{v[0] + o[0], v[1] + o[1], v[2] + o[2]}
}

func (v Vec3) Sub(o Vec3) Vec3 {
	return Vec3{v[0] - o[0], v[1] - o[1], v[2] - o[2]}
}

func (v Vec3) Mul(n int64) Vec3 {
	return Vec3{v[0] * n, v[1] * n, v[2] * n}
}

func (v Vec3) Div(n int64) Vec3 {
	return Vec3{v[0] / n, v[1] / n, v[2] / n}
}

// Fixture is the internal representation. Positions are micrometres and angles
// are microdegrees so every schema conversion stays integer-only.
type Fixture struct {
	ID                string `json:"id"`
	Rig               string `json:"rig,omitempty"`
	Group             string `json:"group,omitempty"`
	PositionUM        Vec3   `json:"position_um"`
	PanMicroDeg       int64  `json:"pan_microdeg"`
	TiltMicroDeg      int64  `json:"tilt_microdeg"`
	InnerHalfMicroDeg int64  `json:"inner_half_microdeg"`
	OuterHalfMicroDeg int64  `json:"outer_half_microdeg"`
	CCTKelvin         int64  `json:"cct_kelvin"`
	LevelPermyriad    int64  `json:"level_permyriad"`
	RGBPermyriad      Vec3   `json:"rgb_permyriad"`

	// BaseAim is reserved for integration tests and adapters that receive a
	// direction vector directly. Loaded schema fixtures normally leave it nil
	// and derive direction from pan/tilt.
	BaseAim *Vec3 `json:"base_aim,omitempty"`
}

type CueKind string

const (
	CueFade  CueKind = "fade"
	CueSweep CueKind = "sweep"
	CueColor CueKind = "color"
)

// Cue values not used by a particular kind stay zero. Times are internal ms.
type Cue struct {
	ID        string   `json:"id"`
	FixtureID string   `json:"fixture_id"`
	Kind      CueKind  `json:"kind"`
	StartMS   int64    `json:"start_ms"`
	EndMS     int64    `json:"end_ms"`
	FromLevel int64    `json:"from_level,omitempty"`
	ToLevel   int64    `json:"to_level,omitempty"`
	FromPan   int64    `json:"from_pan_microdeg,omitempty"`
	ToPan     int64    `json:"to_pan_microdeg,omitempty"`
	FromTilt  int64    `json:"from_tilt_microdeg,omitempty"`
	ToTilt    int64    `json:"to_tilt_microdeg,omitempty"`
	FromRGB   [3]int64 `json:"from_rgb_permyriad,omitempty"`
	ToRGB     [3]int64 `json:"to_rgb_permyriad,omitempty"`
}

type SampleWindow struct {
	Number  int     `json:"number"`
	StartMS int64   `json:"start_ms"`
	EndMS   int64   `json:"end_ms"`
	Samples []int64 `json:"samples_ms"`
}

type Timeline struct {
	DurationMS int64          `json:"duration_ms"`
	Windows    []SampleWindow `json:"windows"`
}

type FixtureGroup struct {
	Rig   string `json:"rig"`
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
}

type ConversionTrace struct {
	FixtureID     string `json:"fixture_id,omitempty"`
	CueID         string `json:"cue_id,omitempty"`
	Field         string `json:"field"`
	SourceVersion string `json:"source_version"`
	TargetVersion string `json:"target_version"`
	SourceValue   string `json:"source_value"`
	InternalValue string `json:"internal_value"`
	Operation     string `json:"operation"`
	Rounding      string `json:"rounding,omitempty"`
	Remainder     int64  `json:"remainder,omitempty"`
	DefaultSource string `json:"default_source,omitempty"`
}

type Rounding string

const (
	RoundNearest    Rounding = "nearest"
	RoundFloor      Rounding = "floor"
	RoundCeil       Rounding = "ceil"
	RoundTowardZero Rounding = "toward_zero"
	RoundExact      Rounding = "exact"
)

type Project struct {
	ProjectID        string            `json:"project_id"`
	SourceVersion    string            `json:"source_version"`
	SourceHash       string            `json:"source_hash"`
	Seed             int64             `json:"seed"`
	ParameterVersion string            `json:"parameter_version"`
	Revision         int64             `json:"revision"`
	Fixtures         []Fixture         `json:"fixtures"`
	Cues             []Cue             `json:"cues"`
	Timeline         Timeline          `json:"timeline"`
	Groups           []FixtureGroup    `json:"groups,omitempty"`
	Traces           []ConversionTrace `json:"conversion_traces,omitempty"`
}

type SamplingConfig struct {
	DistancesMM      []int64 `json:"distances_mm"`
	Density          int     `json:"density"`
	AngularSegments  int     `json:"angular_segments"`
	CellMM           int64   `json:"cell_mm"`
	AccumulationBits int     `json:"accumulation_bits"`
}

func ExactSampling() SamplingConfig {
	return SamplingConfig{
		DistancesMM:      []int64{1500, 3000},
		Density:          4,
		AngularSegments:  12,
		CellMM:           300,
		AccumulationBits: 32,
	}
}

func PreviewSampling() SamplingConfig {
	return SamplingConfig{
		DistancesMM:      []int64{1500, 3000},
		Density:          2,
		AngularSegments:  12,
		CellMM:           900,
		AccumulationBits: 12,
	}
}

type Tolerance struct {
	Intensity int64 `json:"intensity"`
	Chroma    int64 `json:"chroma"`
}

type FixturePose struct {
	FixtureID         string     `json:"fixture_id"`
	TimeMS            int64      `json:"time_ms"`
	PositionMM        Vec3       `json:"position_mm"`
	Direction         [3]float64 `json:"direction"`
	PanMicroDeg       int64      `json:"pan_microdeg"`
	TiltMicroDeg      int64      `json:"tilt_microdeg"`
	InnerHalfMicroDeg int64      `json:"inner_half_microdeg"`
	OuterHalfMicroDeg int64      `json:"outer_half_microdeg"`
	LevelPermyriad    int64      `json:"level_permyriad"`
	CCTKelvin         int64      `json:"cct_kelvin"`
	RGBPermyriad      Vec3       `json:"rgb_permyriad"`
	FixtureHash       string     `json:"fixture_hash"`
	UpstreamKeys      []string   `json:"upstream_keys,omitempty"`
}

type LightPoint struct {
	Key               string   `json:"key"`
	CoordinateMM      Vec3     `json:"coordinate_mm"`
	DistanceMM        int64    `json:"distance_mm"`
	Intensity         int64    `json:"intensity"`
	LowerBound        int64    `json:"lower_bound"`
	UpperBound        int64    `json:"upper_bound"`
	ColorContribution [3]int64 `json:"color_contribution"`
	Uncertain         bool     `json:"uncertain"`
}

type FixtureSample struct {
	FixtureID      string       `json:"fixture_id"`
	Pose           FixturePose  `json:"pose"`
	CacheKey       string       `json:"cache_key"`
	Points         []LightPoint `json:"points"`
	TotalIntensity int64        `json:"total_intensity"`
	TotalColor     [3]int64     `json:"total_color_contribution"`
	UncertainCount int          `json:"uncertain_count"`
}

type AggregatePoint struct {
	Cell              Vec3     `json:"cell_mm"`
	CoordinateMM      Vec3     `json:"coordinate_mm"`
	Intensity         int64    `json:"intensity"`
	LowerBound        int64    `json:"lower_bound"`
	UpperBound        int64    `json:"upper_bound"`
	ColorContribution [3]int64 `json:"color_contribution"`
	Uncertain         bool     `json:"uncertain"`
	Contributors      []string `json:"contributors"`
}

type WindowDependency struct {
	FixtureIDs []string `json:"fixture_ids"`
	CacheKeys  []string `json:"cache_keys"`
	NodeIDs    []string `json:"node_ids"`
	WindowKey  string   `json:"window_key"`
}

type WindowResult struct {
	WindowNumber     int              `json:"window_number"`
	SampleTimes      []int64          `json:"sample_times_ms"`
	ParameterVersion string           `json:"parameter_version"`
	AlgorithmVersion string           `json:"algorithm_version"`
	Sampling         SamplingConfig   `json:"sampling"`
	Fixtures         []FixtureSample  `json:"fixtures"`
	Aggregates       []AggregatePoint `json:"aggregates"`
	UncertainCount   int              `json:"uncertain_count"`
	Dependencies     WindowDependency `json:"dependencies"`
	ContentHash      string           `json:"content_hash"`
}

type Invalidation struct {
	WindowNumber int      `json:"window_number"`
	FixtureIDs   []string `json:"fixture_ids,omitempty"`
	CueIDs       []string `json:"cue_ids,omitempty"`
	Reasons      []string `json:"reasons"`
	Downstream   []string `json:"downstream"`
}

type RunResult struct {
	Project        string         `json:"project"`
	Windows        []WindowResult `json:"windows"`
	Committed      []int          `json:"committed"`
	Reused         []int          `json:"reused"`
	CommitSequence []int          `json:"commit_sequence"`
	Invalidated    []Invalidation `json:"invalidated,omitempty"`
}

type PreviewStatistics struct {
	WindowCount    int   `json:"window_count"`
	FixtureSamples int   `json:"fixture_samples"`
	PointCount     int   `json:"point_count"`
	UncertainCount int   `json:"uncertain_count"`
	EstimatedBytes int64 `json:"estimated_bytes"`
}

type PreviewReport struct {
	Result       RunResult         `json:"result"`
	Sampling     SamplingConfig    `json:"sampling"`
	ElapsedMS    int64             `json:"elapsed_ms"`
	BudgetMS     int64             `json:"budget_ms"`
	WithinBudget bool              `json:"within_budget"`
	Statistics   PreviewStatistics `json:"statistics"`
	Decision     string            `json:"decision"`
}

type Mismatch struct {
	Path         string  `json:"path"`
	WindowNumber int     `json:"window_number,omitempty"`
	FixtureID    string  `json:"fixture_id,omitempty"`
	Field        string  `json:"field"`
	Coordinate   []int64 `json:"coordinate_mm,omitempty"`
	Actual       any     `json:"actual,omitempty"`
	Expected     any     `json:"expected,omitempty"`
}

type PreviewComparison struct {
	OK          bool      `json:"ok"`
	Compared    int       `json:"compared_points"`
	PreviewOnly int       `json:"preview_only_points"`
	First       *Mismatch `json:"first_mismatch,omitempty"`
	Tolerance   Tolerance `json:"tolerance"`
}

type PathDigest struct {
	Name           string `json:"name"`
	Windows        int    `json:"windows"`
	CommitSequence []int  `json:"commit_sequence"`
	ResultHash     string `json:"result_hash"`
}

type VerifyReport struct {
	OK          bool         `json:"ok"`
	Paths       []PathDigest `json:"paths"`
	First       *Mismatch    `json:"first_mismatch,omitempty"`
	CheckedAtMS int64        `json:"-"`
}

func cloneProject(p *Project) (*Project, error) {
	if p == nil {
		return nil, domainError(CodeValue, "nil project", "project")
	}
	data, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("clone project: %w", err)
	}
	var out Project
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("clone project: %w", err)
	}
	return &out, nil
}

func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func splitDegrees(value int64) (int64, int64) {
	return value / 1_000_000, value % 1_000_000
}
