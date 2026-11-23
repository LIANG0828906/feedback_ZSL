package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"stitchworkbench/internal/errs"
)

const AlgorithmVersion = "stitch-v1.0.0"

type Matrix3 [9]float64

func Identity() Matrix3 {
	return Matrix3{1, 0, 0, 0, 1, 0, 0, 0, 1}
}

func (m Matrix3) Mul(n Matrix3) Matrix3 {
	var out Matrix3
	for r := 0; r < 3; r++ {
		for c := 0; c < 3; c++ {
			for k := 0; k < 3; k++ {
				out[r*3+c] += m[r*3+k] * n[k*3+c]
			}
		}
	}
	return out
}

func (m Matrix3) Apply(x, y float64) (float64, float64) {
	d := m[6]*x + m[7]*y + m[8]
	if math.Abs(d) < 1e-15 {
		return math.NaN(), math.NaN()
	}
	return (m[0]*x + m[1]*y + m[2]) / d, (m[3]*x + m[4]*y + m[5]) / d
}

func (m Matrix3) Inverse() (Matrix3, bool) {
	a, b, c := m[0], m[1], m[2]
	d, e, f := m[3], m[4], m[5]
	g, h, i := m[6], m[7], m[8]
	A := e*i - f*h
	B := c*h - b*i
	C := b*f - c*e
	D := f*g - d*i
	E := a*i - c*g
	F := c*d - a*f
	G := d*h - e*g
	H := b*g - a*h
	I := a*e - b*d
	det := a*A + b*D + c*G
	if !isFinite(det) || math.Abs(det) < 1e-14 {
		return Matrix3{}, false
	}
	invDet := 1 / det
	out := Matrix3{A * invDet, B * invDet, C * invDet, D * invDet, E * invDet, F * invDet, G * invDet, H * invDet, I * invDet}
	return out, out.Finite()
}

func (m Matrix3) Finite() bool {
	for _, v := range m {
		if !isFinite(v) {
			return false
		}
	}
	return true
}

func (m Matrix3) Normalized() Matrix3 {
	if math.Abs(m[8]) > 1e-15 {
		s := 1 / m[8]
		for i := range m {
			m[i] *= s
		}
	}
	return m
}

func (m Matrix3) MaxAbsDiff(n Matrix3) float64 {
	a := m.Normalized()
	b := n.Normalized()
	max := 0.0
	for i := range a {
		d := math.Abs(a[i] - b[i])
		if d > max {
			max = d
		}
	}
	return max
}

func (m Matrix3) Slice() []float64 {
	return append([]float64(nil), m[:]...)
}

func MatrixFromSlice(v []float64) (Matrix3, bool) {
	if len(v) != 9 {
		return Matrix3{}, false
	}
	var m Matrix3
	copy(m[:], v)
	return m, m.Finite()
}

func isFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type PointPair struct {
	Source Point
	Target Point
}

type Correspondence struct {
	Image Point `json:"image"`
	Base  Point `json:"base"`
}

type Canvas struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

type ImageTruth struct {
	File             string           `json:"file"`
	Width            int              `json:"width"`
	Height           int              `json:"height"`
	Channels         int              `json:"channels"`
	Brightness       float64          `json:"brightness"`
	HomographyToBase []float64        `json:"homography_to_base"`
	Correspondences  []Correspondence `json:"correspondences,omitempty"`
}

type OverlapTruth struct {
	Base  string `json:"base"`
	Image string `json:"image"`
	Count int    `json:"count"`
}

type Manifest struct {
	Schema        string         `json:"schema"`
	Seed          int64          `json:"seed"`
	SetID         string         `json:"set_id"`
	Description   string         `json:"description"`
	BaseImage     string         `json:"base_image"`
	Canvas        Canvas         `json:"canvas"`
	HomographyTol float64        `json:"homography_tolerance"`
	ResidualTol   float64        `json:"residual_tolerance"`
	SeamTol       float64        `json:"seam_gradient_tolerance"`
	Images        []ImageTruth   `json:"images"`
	Overlaps      []OverlapTruth `json:"overlaps,omitempty"`
}

// Params are all editable, versioned algorithm controls.
type Params struct {
	FeatureBudget         int     `json:"feature_budget"`
	ScaleLevels           int     `json:"scale_levels"`
	ResponseThreshold     float64 `json:"response_threshold"`
	MatchRatio            float64 `json:"match_ratio"`
	MatchMin              int     `json:"match_min"`
	RANSACThreshold       float64 `json:"ransac_threshold"`
	RANSACIterations      int     `json:"ransac_iterations"`
	HomographyMaxCond     float64 `json:"homography_max_cond"`
	PreviewDownsample     int     `json:"preview_downsample"`
	PreviewFeatureBudget  int     `json:"preview_feature_budget"`
	DivergenceCoverageTol float64 `json:"divergence_coverage_tol"`
	DivergenceDecisionTol int     `json:"divergence_decision_tol"`
	ExposureMaxGain       float64 `json:"exposure_max_gain"`
	SeamGradientWeight    float64 `json:"seam_gradient_weight"`
	FusionLevels          int     `json:"fusion_levels"`
	FusionSigma           float64 `json:"fusion_sigma"`
	MaxWorkers            int     `json:"max_workers"`
	MaxPixelsPerWorker    int64   `json:"max_pixels_per_worker"`
}

func DefaultParams() Params {
	return Params{
		FeatureBudget:         12000,
		ScaleLevels:           4,
		ResponseThreshold:     0.008,
		MatchRatio:            0.84,
		MatchMin:              12,
		RANSACThreshold:       0.9,
		RANSACIterations:      4000,
		HomographyMaxCond:     1e9,
		PreviewDownsample:     2,
		PreviewFeatureBudget:  2200,
		DivergenceCoverageTol: 0.02,
		DivergenceDecisionTol: 0,
		ExposureMaxGain:       1.6,
		SeamGradientWeight:    0.35,
		FusionLevels:          4,
		FusionSigma:           1.4,
		MaxWorkers:            4,
		MaxPixelsPerWorker:    64 << 20,
	}
}

func ValidateParams(p Params) error {
	field := func(name, msg string) error { return errs.Field(errs.EPara, msg, name) }
	if p.FeatureBudget < 32 || p.FeatureBudget > 200000 {
		return field("feature_budget", "feature budget out of range [32,200000]")
	}
	if p.ScaleLevels < 1 || p.ScaleLevels > 7 {
		return field("scale_levels", "scale levels out of range [1,7]")
	}
	if p.ResponseThreshold <= 0 || p.ResponseThreshold >= 1 || math.IsNaN(p.ResponseThreshold) {
		return field("response_threshold", "response threshold must be in (0,1)")
	}
	if p.MatchRatio <= 0 || p.MatchRatio >= 1 || math.IsNaN(p.MatchRatio) {
		return field("match_ratio", "match ratio must be in (0,1)")
	}
	if p.MatchMin < 4 || p.MatchMin > 100000 {
		return field("match_min", "match minimum out of range [4,100000]")
	}
	if p.RANSACThreshold <= 0 || p.RANSACThreshold > 100 || math.IsNaN(p.RANSACThreshold) {
		return field("ransac_threshold", "RANSAC threshold must be in (0,100]")
	}
	if p.RANSACIterations < 20 || p.RANSACIterations > 200000 {
		return field("ransac_iterations", "RANSAC iterations out of range [20,200000]")
	}
	if p.HomographyMaxCond < 10 || math.IsInf(p.HomographyMaxCond, 0) || math.IsNaN(p.HomographyMaxCond) {
		return field("homography_max_cond", "homography condition limit must be finite and >= 10")
	}
	if p.PreviewDownsample < 1 || p.PreviewDownsample > 8 {
		return field("preview_downsample", "preview downsample out of range [1,8]")
	}
	if p.PreviewFeatureBudget < 32 || p.PreviewFeatureBudget > 200000 {
		return field("preview_feature_budget", "preview feature budget out of range [32,200000]")
	}
	if p.PreviewFeatureBudget > p.FeatureBudget {
		return field("preview_feature_budget", "preview feature budget must not exceed feature budget")
	}
	if p.DivergenceCoverageTol < 0 || p.DivergenceCoverageTol > 1 || math.IsNaN(p.DivergenceCoverageTol) {
		return field("divergence_coverage_tol", "coverage tolerance must be in [0,1]")
	}
	if p.DivergenceDecisionTol < 0 {
		return field("divergence_decision_tol", "decision tolerance must be >= 0")
	}
	if p.ExposureMaxGain < 1.05 || p.ExposureMaxGain > 4 || math.IsNaN(p.ExposureMaxGain) {
		return field("exposure_max_gain", "exposure max gain out of range [1.05,4]")
	}
	if p.SeamGradientWeight < 0 || p.SeamGradientWeight > 4 || math.IsNaN(p.SeamGradientWeight) {
		return field("seam_gradient_weight", "seam gradient weight out of range [0,4]")
	}
	if p.FusionLevels < 1 || p.FusionLevels > 6 {
		return field("fusion_levels", "fusion levels out of range [1,6]")
	}
	if p.FusionSigma < 0.4 || p.FusionSigma > 8 || math.IsNaN(p.FusionSigma) {
		return field("fusion_sigma", "fusion sigma out of range [0.4,8]")
	}
	if p.MaxWorkers < 1 || p.MaxWorkers > 32 {
		return field("max_workers", "max workers out of range [1,32]")
	}
	if p.MaxPixelsPerWorker < 1<<20 {
		return field("max_pixels_per_worker", "per-worker pixel buffer must be at least 1048576")
	}
	return nil
}

// ApplyPartial merges explicitly provided JSON fields and then validates.
func (p Params) ApplyPartial(raw map[string]json.RawMessage) (Params, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return p, err
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		return p, err
	}
	for k, v := range raw {
		if _, ok := obj[k]; !ok {
			return p, fmt.Errorf("unknown parameter %q", k)
		}
		obj[k] = v
	}
	merged, err := json.Marshal(obj)
	if err != nil {
		return p, err
	}
	var out Params
	if err := json.Unmarshal(merged, &out); err != nil {
		return p, err
	}
	return out, nil
}

func (p Params) Hash() string {
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type ParamSnapshot struct {
	Version   int64     `json:"version"`
	Params    Params    `json:"params"`
	Hash      string    `json:"hash"`
	CreatedAt time.Time `json:"created_at"`
}

type PairStats struct {
	PairIndex         int       `json:"pair_index"`
	PairID            string    `json:"pair_id"`
	SourceImage       string    `json:"source_image"`
	TargetImage       string    `json:"target_image"`
	Homography        []float64 `json:"homography"`
	TruthHomography   []float64 `json:"truth_homography,omitempty"`
	HomographyErr     float64   `json:"homography_error"`
	Matches           int       `json:"matches"`
	Inliers           int       `json:"inliers"`
	InlierRate        float64   `json:"inlier_rate"`
	ResidualMedian    float64   `json:"residual_median"`
	ConditionNumber   float64   `json:"condition_number"`
	OverlapCoverage   float64   `json:"overlap_coverage"`
	Decision          bool      `json:"decision"`
	Path              []string  `json:"path,omitempty"`
	ExposureGain      []float64 `json:"exposure_gain"`
	ExposureOffset    []float64 `json:"exposure_offset"`
	ContentHash       string    `json:"content_hash"`
	ParamVersion      int64     `json:"param_version"`
	AlgorithmVersion  string    `json:"algorithm_version"`
	PreviewDownsample int       `json:"preview_downsample,omitempty"`
	PreviewFeature    int       `json:"preview_feature_budget,omitempty"`
	Committed         bool      `json:"committed"`
	CommittedVersion  int64     `json:"committed_version,omitempty"`
	ErrorCode         string    `json:"error_code,omitempty"`
	ErrorMessage      string    `json:"error_message,omitempty"`
}

type DivergenceReport struct {
	ComparedPairs       int      `json:"compared_pairs"`
	PreviewAccepted     []string `json:"preview_accepted"`
	ExactAccepted       []string `json:"exact_accepted"`
	DecisionDiff        int      `json:"decision_diff"`
	MaxCoverageDelta    float64  `json:"max_coverage_delta"`
	CoverageTolerance   float64  `json:"coverage_tolerance"`
	DecisionTolerance   int      `json:"decision_tolerance"`
	Consistent          bool     `json:"consistent"`
	PreviewImageHash    string   `json:"preview_image_hash"`
	ExactImageHash      string   `json:"exact_image_hash"`
	PreviewParamVersion int64    `json:"preview_param_version"`
	ExactParamVersion   int64    `json:"exact_param_version"`
	PreviewMs           int64    `json:"preview_ms"`
	ExactMs             int64    `json:"exact_ms"`
}

type PreviewResult struct {
	SetID           string             `json:"set_id"`
	ParamVersion    int64              `json:"param_version"`
	ParamsHash      string             `json:"params_hash"`
	ContentHash     string             `json:"content_hash"`
	Downsample      int                `json:"downsample"`
	FeatureBudget   int                `json:"feature_budget"`
	DurationMs      int64              `json:"duration_ms"`
	Pairs           []PairStats        `json:"pairs"`
	Canvas          Canvas             `json:"canvas"`
	PanoramaURL     string             `json:"panorama_url"`
	DiagnosticsURL  string             `json:"diagnostics_url"`
	ElapsedBudgetMs int64              `json:"elapsed_budget_ms"`
	ImageWidth      int                `json:"image_width"`
	ImageHeight     int                `json:"image_height"`
	CoverageByPair  map[string]float64 `json:"coverage_by_pair"`
}

type ExactResult struct {
	SetID            string           `json:"set_id"`
	TaskID           string           `json:"task_id"`
	ParamVersion     int64            `json:"param_version"`
	ParamsHash       string           `json:"params_hash"`
	ContentHash      string           `json:"content_hash"`
	Canvas           Canvas           `json:"canvas"`
	Pairs            []PairStats      `json:"pairs"`
	Divergence       DivergenceReport `json:"divergence"`
	PanoramaURL      string           `json:"panorama_url"`
	DiagnosticsURL   string           `json:"diagnostics_url"`
	HeatmapURL       string           `json:"heatmap_url"`
	SeamOverlayURL   string           `json:"seam_overlay_url"`
	MatchOverlayURL  string           `json:"match_overlay_url"`
	DurationMs       int64            `json:"duration_ms"`
	CommittedCount   int              `json:"committed_count"`
	MaxParallelism   int              `json:"max_parallelism"`
	SeamMaxGradient  float64          `json:"seam_max_gradient"`
	SeamGradientP95  float64          `json:"seam_gradient_p95"`
	SeamGradientTol  float64          `json:"seam_gradient_tolerance"`
	CumulativeError  float64          `json:"cumulative_error"`
	AlgorithmVersion string           `json:"algorithm_version"`
}

type CheckpointHeader struct {
	Format           string            `json:"format"`
	SetID            string            `json:"set_id"`
	ParamVersion     int64             `json:"param_version"`
	ParamsHash       string            `json:"params_hash"`
	BaseContentHash  string            `json:"base_content_hash"`
	ContentHashes    map[string]string `json:"content_hashes"`
	AlgorithmVersion string            `json:"algorithm_version"`
	Sequence         int64             `json:"sequence"`
	UpdatedAt        time.Time         `json:"updated_at"`
}

type CheckpointRecord struct {
	PairIndex        int       `json:"pair_index"`
	PairID           string    `json:"pair_id"`
	SourceImage      string    `json:"source_image"`
	TargetImage      string    `json:"target_image"`
	Homography       []float64 `json:"homography"`
	Matches          int       `json:"matches"`
	Inliers          int       `json:"inliers"`
	ResidualMedian   float64   `json:"residual_median"`
	ConditionNumber  float64   `json:"condition_number"`
	ExposureGain     []float64 `json:"exposure_gain"`
	ExposureOffset   []float64 `json:"exposure_offset"`
	ContentHash      string    `json:"content_hash"`
	ParamVersion     int64     `json:"param_version"`
	AlgorithmVersion string    `json:"algorithm_version"`
	CommittedAt      time.Time `json:"committed_at"`
}

type CheckpointSnapshot struct {
	Header  CheckpointHeader   `json:"header"`
	Records []CheckpointRecord `json:"records"`
}

type MatchPoint struct {
	Source Point `json:"source"`
	Target Point `json:"target"`
}

type MatchDiagnostic struct {
	PairID  string       `json:"pair_id"`
	Points  []MatchPoint `json:"points"`
	Inliers []bool       `json:"inliers"`
}
