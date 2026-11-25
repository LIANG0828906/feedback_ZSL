package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

const AlgorithmVersion = "edge-refine-v1.0.0"
const SchemaVersion = 1

type GrayImage struct {
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	Channels int     `json:"channels"`
	BitDepth int     `json:"bit_depth"`
	Pix      []uint8 `json:"-"`
}

func (im *GrayImage) At(x, y int) uint8 {
	if x < 0 {
		x = 0
	} else if x >= im.Width {
		x = im.Width - 1
	}
	if y < 0 {
		y = 0
	} else if y >= im.Height {
		y = im.Height - 1
	}
	return im.Pix[y*im.Width+x]
}

type Rect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

type TileCoord struct {
	TX int `json:"tx"`
	TY int `json:"ty"`
}

func (t TileCoord) Array() [2]int { return [2]int{t.TX, t.TY} }

type Params struct {
	TileSize          int     `json:"tile_size"`
	IterBudget        int     `json:"iter_budget"`
	Radius            int     `json:"radius"`
	PreviewDelayMS    int     `json:"preview_delay_ms"`
	BitDepth          int     `json:"bit_depth"`
	FGQuantile        float64 `json:"fg_quantile"`
	BGQuantile        float64 `json:"bg_quantile"`
	ConvergeTolerance float64 `json:"converge_tolerance_px"`
	EdgeTolerance     float64 `json:"edge_tolerance_px"`
	EdgeUpperBound    float64 `json:"edge_upper_bound_px"`
	PreviewMedianTol  float64 `json:"preview_median_tol_px"`
	PreviewPixelTol   float64 `json:"preview_pixel_ratio_tol"`
	ConvergeRatioTol  float64 `json:"converge_ratio_tol"`
	ParamVersion      int     `json:"param_version"`
}

func DefaultParams() Params {
	return Params{
		TileSize:          32,
		IterBudget:        5,
		Radius:            1,
		PreviewDelayMS:    100,
		BitDepth:          8,
		FGQuantile:        0.96,
		BGQuantile:        0.04,
		ConvergeTolerance: 0.85,
		EdgeTolerance:     0.75,
		EdgeUpperBound:    4.0,
		PreviewMedianTol:  0.75,
		PreviewPixelTol:   0.08,
		ConvergeRatioTol:  0.10,
		ParamVersion:      1,
	}
}

type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type TruthEdge struct {
	ID             int     `json:"id"`
	Kind           string  `json:"kind"`
	Polarity       string  `json:"polarity"`
	X1             float64 `json:"x1"`
	Y1             float64 `json:"y1"`
	X2             float64 `json:"x2"`
	Y2             float64 `json:"y2"`
	SubpixelOffset float64 `json:"subpixel_offset"`
	ReferenceLine  string  `json:"reference_line"`
}

func (e TruthEdge) Start() Point { return Point{X: e.X1, Y: e.Y1} }
func (e TruthEdge) End() Point   { return Point{X: e.X2, Y: e.Y2} }

type Manifest struct {
	SchemaVersion     int         `json:"schema_version"`
	AlgorithmVersion  string      `json:"algorithm_version"`
	SampleID          string      `json:"sample_id"`
	Seed              int64       `json:"seed"`
	ImageFile         string      `json:"image_file"`
	Width             int         `json:"width"`
	Height            int         `json:"height"`
	Channels          int         `json:"channels"`
	BitDepth          int         `json:"bit_depth"`
	ReferenceMaskFile string      `json:"reference_mask_file"`
	ReferenceMaskHash string      `json:"reference_mask_hash"`
	Params            Params      `json:"params"`
	Edges             []TruthEdge `json:"edges"`
	TruthFingerprint  string      `json:"truth_fingerprint,omitempty"`
}

func (m Manifest) TileCounts() (int, int) {
	return m.Width / m.Params.TileSize, m.Height / m.Params.TileSize
}

type Histogram []int

type ThresholdSnapshot struct {
	SampleID        string    `json:"sample_id"`
	Tile            TileCoord `json:"tile"`
	BitDepth        int       `json:"bit_depth"`
	HistHash        string    `json:"hist_hash"`
	BGQuantile      float64   `json:"bg_quantile"`
	FGQuantile      float64   `json:"fg_quantile"`
	BGLevel         int       `json:"bg_level"`
	FGLevel         int       `json:"fg_level"`
	Threshold       float64   `json:"threshold"`
	ClassSeparation int       `json:"class_separation"`
	Homogeneous     bool      `json:"homogeneous"`
	Manual          *float64  `json:"manual,omitempty"`
	ParamVersion    int       `json:"param_version"`
}

type IterationRecord struct {
	Iteration     int     `json:"iteration"`
	MaxResidual   float64 `json:"max_residual"`
	MeanResidual  float64 `json:"mean_residual"`
	ChangedPixels int     `json:"changed_pixels"`
	Converged     bool    `json:"converged"`
}

type EdgePoint struct {
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	PixelX   int     `json:"pixel_x"`
	PixelY   int     `json:"pixel_y"`
	Axis     int     `json:"axis"`
	Signed   float64 `json:"signed_residual"`
	Gradient float64 `json:"gradient"`
}

type TileResult struct {
	SampleID         string            `json:"sample_id"`
	Tile             TileCoord         `json:"tile"`
	Rect             Rect              `json:"rect"`
	Threshold        ThresholdSnapshot `json:"threshold"`
	SeparationMask   []byte            `json:"separation_mask"`
	CleanMask        []byte            `json:"clean_mask"`
	SkeletonMask     []byte            `json:"skeleton_mask"`
	RefinedMask      []byte            `json:"refined_mask"`
	DeviationMap     []float64         `json:"deviation_map"`
	EdgePoints       []EdgePoint       `json:"edge_points"`
	Iterations       []IterationRecord `json:"iterations"`
	FinalResidual    float64           `json:"final_residual"`
	Converged        bool              `json:"converged"`
	ActualBudget     int               `json:"actual_budget"`
	ElementStep      int               `json:"element_step"`
	DecisionBasis    string            `json:"decision_basis"`
	ParamVersion     int               `json:"param_version"`
	AlgorithmVersion string            `json:"algorithm_version"`
}

type EdgeStat struct {
	EdgeID        int     `json:"edge_id"`
	Median        float64 `json:"median_px"`
	Upper         float64 `json:"upper_px"`
	P95           float64 `json:"p95_px"`
	Max           float64 `json:"max_px"`
	SignedMedian  float64 `json:"signed_median_px"`
	Count         int     `json:"count"`
	Pass          bool    `json:"pass"`
	TruthPoint    Point   `json:"truth_point"`
	ObservedPoint Point   `json:"observed_point"`
}

type AccuracyReport struct {
	Median         float64    `json:"median_px"`
	Upper          float64    `json:"upper_px"`
	P95            float64    `json:"p95_px"`
	Max            float64    `json:"max_px"`
	SignedMedian   float64    `json:"signed_median_px"`
	Pass           bool       `json:"pass"`
	Edges          []EdgeStat `json:"edges"`
	OutOfTolerance []int      `json:"out_of_tolerance"`
}

type TileConvergence struct {
	Tile            TileCoord `json:"tile"`
	Converged       bool      `json:"converged"`
	Residual        float64   `json:"residual"`
	CommittedStages int       `json:"committed_stages"`
}

type ReferenceReport struct {
	ReferenceMaskHash string  `json:"reference_mask_hash"`
	IoU               float64 `json:"iou"`
	MismatchRatio     float64 `json:"mismatch_ratio"`
	MeanAbsoluteError float64 `json:"mean_absolute_error"`
	FalsePositive     int     `json:"false_positive"`
	FalseNegative     int     `json:"false_negative"`
}

type RunReport struct {
	SampleID           string              `json:"sample_id"`
	AlgorithmVersion   string              `json:"algorithm_version"`
	ParamVersion       int                 `json:"param_version"`
	Workers            int                 `json:"workers"`
	TilesX             int                 `json:"tiles_x"`
	TilesY             int                 `json:"tiles_y"`
	TotalStages        int                 `json:"total_stages"`
	CommittedStages    int                 `json:"committed_stages"`
	Progress           float64             `json:"progress"`
	ConvergedTiles     int                 `json:"converged_tiles"`
	TotalTiles         int                 `json:"total_tiles"`
	ConvergedTileRatio float64             `json:"converged_tile_ratio"`
	MaskHash           string              `json:"mask_hash"`
	DeviationHash      string              `json:"deviation_hash"`
	CommitSequence     []string            `json:"commit_sequence"`
	Tiles              []TileResult        `json:"-"`
	Convergence        []TileConvergence   `json:"convergence"`
	Accuracy           *AccuracyReport     `json:"accuracy,omitempty"`
	Reference          *ReferenceReport    `json:"reference,omitempty"`
	Preview            *PreviewReport      `json:"preview,omitempty"`
	Divergence         *DivergenceReport   `json:"divergence,omitempty"`
	ThreePath          *ThreePathReport    `json:"three_path,omitempty"`
	Verification       *VerificationReport `json:"verification,omitempty"`
}

type PreviewReport struct {
	ActualBudget   int     `json:"actual_budget"`
	ElementStep    int     `json:"element_step"`
	DelayMS        int64   `json:"delay_ms"`
	DelayLimitMS   int     `json:"delay_limit_ms"`
	WithinLimit    bool    `json:"within_limit"`
	MedianBias     float64 `json:"median_bias_placeholder"`
	ConvergedRatio float64 `json:"converged_ratio_placeholder"`
	MaskHash       string  `json:"mask_hash"`
	DecisionBasis  string  `json:"decision_basis"`
}

type DivergenceReport struct {
	MedianPreview        float64 `json:"median_preview"`
	MedianExact          float64 `json:"median_exact"`
	MedianDifference     float64 `json:"median_difference"`
	ConvergePreview      float64 `json:"converge_preview"`
	ConvergeExact        float64 `json:"converge_exact"`
	ConvergeDifference   float64 `json:"converge_difference"`
	PixelDifferenceRatio float64 `json:"pixel_difference_ratio"`
	FirstTile            [2]int  `json:"first_tile"`
	DiffPixel            [2]int  `json:"diff_pixel"`
	Pass                 bool    `json:"pass"`
}

type ThreePathReport struct {
	Paths                []string `json:"paths"`
	MaskHashes           []string `json:"mask_hashes"`
	DeviationHashes      []string `json:"deviation_hashes"`
	AccuracyHashes       []string `json:"accuracy_hashes"`
	AccuracyEqual        bool     `json:"accuracy_equal"`
	CommitSequencesEqual bool     `json:"commit_sequences_equal"`
	AllPixelEqual        bool     `json:"all_pixel_equal"`
}

type VerificationReport struct {
	OK               bool            `json:"ok"`
	CheckedRows      int             `json:"checked_rows"`
	TruthFingerprint string          `json:"truth_fingerprint"`
	FirstViolation   *Violation      `json:"first_violation,omitempty"`
	Message          string          `json:"message"`
	Accuracy         *AccuracyReport `json:"accuracy,omitempty"`
}

type Violation struct {
	Tile     [2]int `json:"tile"`
	Stage    string `json:"stage"`
	Field    string `json:"field"`
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
}

func HashBytes(prefix string, b []byte) string {
	h := sha256.New()
	h.Write([]byte(prefix))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

func CanonicalHash(v any) string {
	b, _ := json.Marshal(v)
	return HashBytes("canonical-json:", b)
}

func SortedEdgeIDs(edges []TruthEdge) {
	sort.Slice(edges, func(i, j int) bool { return edges[i].ID < edges[j].ID })
}
