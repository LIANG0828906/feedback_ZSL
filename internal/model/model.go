package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

const (
	SchemaVersion     = "sample-v1"
	ParamVersion      = "param-v1"
	AlgorithmVersion  = "algo-v1"
	TruthVersion      = "truth-v1"
	FixedFractionBits = 8
	FixedScale        = int64(1) << FixedFractionBits
)

type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type BlockCoord struct {
	Index int `json:"index"`
	BX    int `json:"block_x"`
	BY    int `json:"block_y"`
	X0    int `json:"x0"`
	Y0    int `json:"y0"`
	X1    int `json:"x1"`
	Y1    int `json:"y1"`
}

func (b BlockCoord) Contains(x, y int) bool {
	return x >= b.X0 && x < b.X1 && y >= b.Y0 && y < b.Y1
}

type Manifest struct {
	SchemaVersion string   `json:"schema_version"`
	Name          string   `json:"name"`
	Width         int      `json:"width"`
	Height        int      `json:"height"`
	Channels      int      `json:"channels"`
	TileSize      int      `json:"tile_size"`
	QuantGear     int      `json:"quant_gear"`
	QuantBits     int      `json:"quant_bits"`
	Seed          int64    `json:"seed"`
	Categories    []string `json:"categories"`
	Metric        string   `json:"metric"`
	WindowRadius  int      `json:"window_radius"`
	BandWidth     int      `json:"band_width"`
	ParamVersion  string   `json:"param_version"`
	AlgoVersion   string   `json:"algo_version"`
}

func (m Manifest) BlockCols() int { return m.Width / m.TileSize }
func (m Manifest) BlockRows() int { return m.Height / m.TileSize }
func (m Manifest) BlockCount() int {
	return m.BlockCols() * m.BlockRows()
}

func (m Manifest) Block(index int) BlockCoord {
	cols := m.BlockCols()
	bx := index % cols
	by := index / cols
	return BlockCoord{
		Index: index,
		BX:    bx,
		BY:    by,
		X0:    bx * m.TileSize,
		Y0:    by * m.TileSize,
		X1:    (bx + 1) * m.TileSize,
		Y1:    (by + 1) * m.TileSize,
	}
}

func (m Manifest) Blocks() []BlockCoord {
	blocks := make([]BlockCoord, 0, m.BlockCount())
	for i := 0; i < m.BlockCount(); i++ {
		blocks = append(blocks, m.Block(i))
	}
	return blocks
}

type MaskRun struct {
	Y  int `json:"y"`
	X0 int `json:"x0"`
	X1 int `json:"x1"`
}

type SampleExpectation struct {
	X                  int    `json:"x"`
	Y                  int    `json:"y"`
	Label              int    `json:"label"`
	Metric             string `json:"metric"`
	ExpectedDistanceQ8 int64  `json:"expected_distance_q8"`
}

type Tolerance struct {
	AreaAbs             int   `json:"area_abs"`
	MeanAbsQ8           int64 `json:"mean_abs_q8"`
	StdAbsQ8            int64 `json:"std_abs_q8"`
	BoundaryAbsQ8       int64 `json:"boundary_abs_q8"`
	SampleDistanceAbsQ8 int64 `json:"sample_distance_abs_q8"`
	BoundAbsQ8          int64 `json:"bound_abs_q8"`
}

type RegionTruth struct {
	Label                int       `json:"label"`
	Name                 string    `json:"name"`
	Area                 int       `json:"area"`
	Center               Point     `json:"center"`
	MaskRuns             []MaskRun `json:"mask_runs"`
	ExpectedMeanQ8       int64     `json:"expected_mean_q8"`
	ExpectedStdQ8        int64     `json:"expected_std_q8"`
	ExpectedBoundaryQ8   int64     `json:"expected_boundary_q8"`
	ExpectedBlockJumpsQ8 []int64   `json:"expected_block_jumps_q8,omitempty"`
}

type Truth struct {
	TruthVersion     string              `json:"truth_version"`
	Width            int                 `json:"width"`
	Height           int                 `json:"height"`
	Channels         int                 `json:"channels"`
	TileSize         int                 `json:"tile_size"`
	QuantGear        int                 `json:"quant_gear"`
	QuantBits        int                 `json:"quant_bits"`
	Seed             int64               `json:"seed"`
	Metric           string              `json:"metric"`
	WindowRadius     int                 `json:"window_radius"`
	Tolerance        Tolerance           `json:"tolerance"`
	Regions          []RegionTruth       `json:"regions"`
	Samples          []SampleExpectation `json:"samples"`
	TruthFingerprint string              `json:"truth_fingerprint"`
}

func (t Truth) Region(label int) (RegionTruth, bool) {
	for _, region := range t.Regions {
		if region.Label == label {
			return region, true
		}
	}
	return RegionTruth{}, false
}

type Accumulator struct {
	Label         int   `json:"label"`
	Area          int64 `json:"area"`
	SumDistanceQ8 int64 `json:"sum_distance_q8"`
	SumSquaredQ16 int64 `json:"sum_squared_q16"`
	BoundaryEdges int64 `json:"boundary_edges"`
	BoundarySumQ8 int64 `json:"boundary_sum_q8"`
}

type PixelSummary struct {
	SHA256      string   `json:"sha256"`
	PixelCount  int      `json:"pixel_count"`
	ChannelSums [4]int64 `json:"channel_sums"`
	MinLuma     int      `json:"min_luma"`
	MaxLuma     int      `json:"max_luma"`
}

type QuantRecord struct {
	Gear               int   `json:"gear"`
	Bits               int   `json:"bits"`
	Step               int   `json:"step"`
	TruncationBits     int   `json:"truncation_bits"`
	ErrorChannelSum    int64 `json:"error_channel_sum"`
	ErrorChannelMax    int   `json:"error_channel_max"`
	BeforeMetricSumQ8  int64 `json:"before_metric_sum_q8"`
	AfterMetricSumQ8   int64 `json:"after_metric_sum_q8"`
	AfterMetricMaxQ8   int64 `json:"after_metric_max_q8"`
	QuantTruncationSum int64 `json:"quant_truncation_sum"`
	QuantTruncationMax int   `json:"quant_truncation_max"`
}

type SeamRecord struct {
	VerticalEdges       int   `json:"vertical_edges"`
	HorizontalEdges     int   `json:"horizontal_edges"`
	JumpSumQ8           int64 `json:"jump_sum_q8"`
	VerticalJumpSumQ8   int64 `json:"vertical_jump_sum_q8"`
	HorizontalJumpSumQ8 int64 `json:"horizontal_jump_sum_q8"`
	JumpMaxQ8           int64 `json:"jump_max_q8"`
}

type RegionSeamRecord struct {
	Label               int   `json:"label"`
	VerticalEdges       int64 `json:"vertical_edges"`
	VerticalJumpSumQ8   int64 `json:"vertical_jump_sum_q8"`
	HorizontalEdges     int64 `json:"horizontal_edges"`
	HorizontalJumpSumQ8 int64 `json:"horizontal_jump_sum_q8"`
}

type TruncationMark struct {
	Stage       string `json:"stage"`
	Position    string `json:"position"`
	Numerator   int64  `json:"numerator,omitempty"`
	Denominator int64  `json:"denominator,omitempty"`
	Remainder   int64  `json:"remainder,omitempty"`
}

type BlockRecord struct {
	Block          BlockCoord         `json:"block"`
	InputHash      string             `json:"input_hash"`
	ManifestHash   string             `json:"manifest_hash"`
	ParamVersion   string             `json:"param_version"`
	AlgoVersion    string             `json:"algo_version"`
	BandWidth      int                `json:"band_width"`
	RequiredBand   int                `json:"required_band"`
	Accumulators   []Accumulator      `json:"accumulators"`
	RegionLabels   []int              `json:"region_labels"`
	Quant          QuantRecord        `json:"quant"`
	Seams          SeamRecord         `json:"seams"`
	RegionSeams    []RegionSeamRecord `json:"region_seams"`
	ReconSummary   PixelSummary       `json:"recon_summary"`
	Truncations    []TruncationMark   `json:"truncations"`
	MissingEdges   int                `json:"missing_edges"`
	BandSufficient bool               `json:"band_sufficient"`
}

type ErrorBound struct {
	RegionLabel        int   `json:"region_label"`
	MeanUpperQ8        int64 `json:"mean_upper_q8"`
	StdUpperQ8         int64 `json:"std_upper_q8"`
	BoundaryUpperQ8    int64 `json:"boundary_upper_q8"`
	PropagationUpperQ8 int64 `json:"propagation_upper_q8"`
	QuantGear          int   `json:"quant_gear"`
	QuantBits          int   `json:"quant_bits"`
	TileSize           int   `json:"tile_size"`
	FixedFractionBits  int   `json:"fixed_fraction_bits"`
}

type RegionMetrics struct {
	Label          int              `json:"label"`
	Name           string           `json:"name,omitempty"`
	Area           int64            `json:"area"`
	MeanDistanceQ8 int64            `json:"mean_distance_q8"`
	StdDistanceQ8  int64            `json:"std_distance_q8"`
	BoundaryJumpQ8 int64            `json:"boundary_jump_q8"`
	BoundaryEdges  int64            `json:"boundary_edges"`
	BlockIndices   []int            `json:"block_indices"`
	Truncations    []TruncationMark `json:"truncations,omitempty"`
}

type RegionReport struct {
	Metrics RegionMetrics `json:"metrics"`
	Bound   ErrorBound    `json:"bound"`
}

type PropagationPath struct {
	RegionLabel int    `json:"region_label"`
	FromBlock   int    `json:"from_block"`
	ToBlock     int    `json:"to_block"`
	Direction   string `json:"direction"`
	EdgeCount   int64  `json:"edge_count"`
	JumpAvgQ8   int64  `json:"jump_avg_q8"`
}

type Checkpoint struct {
	ManifestHash     string       `json:"manifest_hash"`
	InputHash        string       `json:"input_hash"`
	TruthFingerprint string       `json:"truth_fingerprint"`
	ParamVersion     string       `json:"param_version"`
	AlgoVersion      string       `json:"algo_version"`
	Block            BlockRecord  `json:"block"`
	Bounds           []ErrorBound `json:"bounds"`
	CommitOrder      int          `json:"commit_order"`
}

func CanonicalHash(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func BytesHash(parts ...[]byte) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write(part)
	}
	return hex.EncodeToString(h.Sum(nil))
}
