package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

const AlgorithmVersion = "integrity-algo-v1"
const SchemaVersion = "truth-v1"

type Rect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

func (r Rect) MaxX() int { return r.X + r.Width }
func (r Rect) MaxY() int { return r.Y + r.Height }

func (r Rect) InFrame(width, height int) bool {
	return r.X >= 0 && r.Y >= 0 && r.Width > 0 && r.Height > 0 &&
		r.MaxX() <= width && r.MaxY() <= height
}

func (r Rect) Contains(x, y int) bool {
	return x >= r.X && x < r.MaxX() && y >= r.Y && y < r.MaxY()
}

type FrameMeta struct {
	Index           int    `json:"index"`
	File            string `json:"file"`
	BaselineFile    string `json:"baseline_file"`
	TimestampMillis int64  `json:"timestamp_millis"`
}

type Primitive struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	BBox        Rect   `json:"bbox"`
	Orientation string `json:"orientation,omitempty"`
}

type ReplacementTruth struct {
	StartFrame  int    `json:"start_frame"`
	EndFrame    int    `json:"end_frame"`
	FrameMask   []int  `json:"frame_mask"`
	Patch       Rect   `json:"patch"`
	SourceFrame int    `json:"source_frame"`
	SourceRect  *Rect  `json:"source_rect,omitempty"`
	Method      string `json:"method"`
}

type Manifest struct {
	SchemaVersion    string           `json:"schema_version"`
	Seed             int64            `json:"seed"`
	Width            int              `json:"width"`
	Height           int              `json:"height"`
	Channels         int              `json:"channels"`
	FrameCount       int              `json:"frame_count"`
	Frames           []FrameMeta      `json:"frames"`
	WatermarkRegion  Rect             `json:"watermark_region"`
	Primitives       []Primitive      `json:"primitives"`
	Replacement      ReplacementTruth `json:"replacement"`
	BaselineDir      string           `json:"baseline_dir"`
	BaselineHashes   map[int]string   `json:"baseline_hashes,omitempty"`
	FrameHashes      map[int]string   `json:"frame_hashes,omitempty"`
	TruthFingerprint string           `json:"truth_fingerprint,omitempty"`
}

type TruthFingerprint struct {
	SchemaVersion    string `json:"schema_version"`
	ManifestSHA256   string `json:"manifest_sha256"`
	TruthSHA256      string `json:"truth_sha256"`
	ReplacementStart int    `json:"replacement_start"`
	ReplacementEnd   int    `json:"replacement_end"`
	FrameCount       int    `json:"frame_count"`
	AlgorithmVersion string `json:"algorithm_version"`
}

type Params struct {
	Version string `json:"version,omitempty"`

	SampleCols          int     `json:"sample_cols"`
	SampleRows          int     `json:"sample_rows"`
	WatermarkThreshold  float64 `json:"watermark_threshold"`
	ConfidenceThreshold float64 `json:"confidence_threshold"`
	SearchRadius        int     `json:"search_radius"`
	AlignmentTolerance  int     `json:"alignment_tolerance"`

	ResidualWindow int     `json:"residual_window"`
	ResidualZScale float64 `json:"residual_z_scale"`

	BlockSize            int     `json:"block_size"`
	NoiseWindow          int     `json:"noise_window"`
	NoiseZThreshold      float64 `json:"noise_z_threshold"`
	EdgeBins             int     `json:"edge_bins"`
	EdgeAnomalyThreshold float64 `json:"edge_anomaly_threshold"`

	WatermarkWeight  float64 `json:"watermark_weight"`
	ResidualWeight   float64 `json:"residual_weight"`
	NoiseWeight      float64 `json:"noise_weight"`
	VerdictThreshold float64 `json:"verdict_threshold"`
	FrameTolerance   int     `json:"frame_tolerance"`

	MaxParallel        int `json:"max_parallel"`
	MaxPixelsPerWorker int `json:"max_pixels_per_worker"`
	BatchSize          int `json:"batch_size"`
}

func DefaultParams() Params {
	return Params{
		SampleCols:          16,
		SampleRows:          3,
		WatermarkThreshold:  0.5,
		ConfidenceThreshold: 0.72,
		SearchRadius:        3,
		AlignmentTolerance:  1,

		ResidualWindow: 3,
		ResidualZScale: 4.0,

		BlockSize:            16,
		NoiseWindow:          3,
		NoiseZThreshold:      3.0,
		EdgeBins:             4,
		EdgeAnomalyThreshold: 0.45,

		WatermarkWeight:  0.42,
		ResidualWeight:   0.22,
		NoiseWeight:      0.36,
		VerdictThreshold: 0.50,
		FrameTolerance:   1,

		MaxParallel:        4,
		MaxPixelsPerWorker: 4 * 1024 * 1024,
		BatchSize:          8,
	}
}

func CanonicalJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func HashCanonical(v any) string {
	return SHA256Hex(CanonicalJSON(v))
}

func (p Params) EffectiveVersion() string {
	if p.Version != "" {
		return p.Version
	}
	return "param-" + HashCanonical(p)[:16]
}

type PositionConfidence struct {
	X          int     `json:"x"`
	Y          int     `json:"y"`
	Column     int     `json:"column"`
	Row        int     `json:"row"`
	Luma       float64 `json:"luma"`
	Bit        int     `json:"bit"`
	Confidence float64 `json:"confidence"`
}

type WatermarkEvidence struct {
	Frame             int                  `json:"frame"`
	Region            Rect                 `json:"region"`
	BestRegion        Rect                 `json:"best_region"`
	OffsetX           int                  `json:"offset_x"`
	OffsetY           int                  `json:"offset_y"`
	DecodedFrame      int                  `json:"decoded_frame"`
	ExpectedFrame     int                  `json:"expected_frame"`
	MatchesExpected   bool                 `json:"matches_expected"`
	OverallConfidence float64              `json:"overall_confidence"`
	Missing           bool                 `json:"missing"`
	Misaligned        bool                 `json:"misaligned"`
	Positions         []PositionConfidence `json:"positions"`
}

type ResidualLag struct {
	Lag        int     `json:"lag"`
	OtherFrame int     `json:"other_frame"`
	Energy     float64 `json:"energy"`
	MAE        float64 `json:"mae"`
}

type ResidualEvidence struct {
	Frame           int           `json:"frame"`
	Lags            []ResidualLag `json:"lags"`
	MedianEnergy    float64       `json:"median_energy"`
	MaxEnergy       float64       `json:"max_energy"`
	PeakBlockEnergy float64       `json:"peak_block_energy"`
	AnomalyScore    float64       `json:"anomaly_score"`
}

type BlockStat struct {
	BlockX              int     `json:"block_x"`
	BlockY              int     `json:"block_y"`
	PixelX              int     `json:"pixel_x"`
	PixelY              int     `json:"pixel_y"`
	Width               int     `json:"width"`
	Height              int     `json:"height"`
	NoiseVariance       float64 `json:"noise_variance"`
	SpatialZ            float64 `json:"spatial_z"`
	TemporalZ           float64 `json:"temporal_z"`
	Dispersion          float64 `json:"dispersion"`
	OrientationBin      int     `json:"orientation_bin"`
	OrientationStrength float64 `json:"orientation_strength"`
	EdgeConsistency     float64 `json:"edge_consistency"`
	Anomalous           bool    `json:"anomalous"`
}

type NoiseEvidence struct {
	Frame           int                 `json:"frame"`
	BlockSize       int                 `json:"block_size"`
	GridCols        int                 `json:"grid_cols"`
	GridRows        int                 `json:"grid_rows"`
	Blocks          []BlockStat         `json:"blocks"`
	MedianVariance  float64             `json:"median_variance"`
	MADVariance     float64             `json:"mad_variance"`
	AnomalousBlocks int                 `json:"anomalous_blocks"`
	AnomalyFraction float64             `json:"anomaly_fraction"`
	MaxTemporalZ    float64             `json:"max_temporal_z"`
	EdgeAnomaly     float64             `json:"edge_anomaly"`
	AnomalyScore    float64             `json:"anomaly_score"`
	PrimitiveEdges  []PrimitiveEdgeStat `json:"primitive_edges"`
}

type PrimitiveEdgeStat struct {
	ID             string  `json:"id"`
	DominantBin    int     `json:"dominant_bin"`
	Consistency    float64 `json:"consistency"`
	GradientEnergy float64 `json:"gradient_energy"`
}

type VerdictEvidence struct {
	Frame          int      `json:"frame"`
	Tampered       bool     `json:"tampered"`
	Confidence     float64  `json:"confidence"`
	Score          float64  `json:"score"`
	WatermarkScore float64  `json:"watermark_score"`
	ResidualScore  float64  `json:"residual_score"`
	NoiseScore     float64  `json:"noise_score"`
	Reasons        []string `json:"reasons"`
}

type Interval struct {
	StartFrame int     `json:"start_frame"`
	EndFrame   int     `json:"end_frame"`
	Confidence float64 `json:"confidence"`
	FrameCount int     `json:"frame_count"`
}

type TimelineBreak struct {
	AfterFrame int    `json:"after_frame"`
	FromValue  int    `json:"from_value"`
	ToValue    int    `json:"to_value"`
	Reason     string `json:"reason"`
}

type WatermarkTimeline struct {
	DecodedValues []int           `json:"decoded_values"`
	Confidences   []float64       `json:"confidences"`
	Breaks        []TimelineBreak `json:"breaks"`
}

type FrameReport struct {
	Frame       int                `json:"frame"`
	ContentHash string             `json:"content_hash"`
	Watermark   *WatermarkEvidence `json:"watermark,omitempty"`
	Residual    *ResidualEvidence  `json:"residual,omitempty"`
	Noise       *NoiseEvidence     `json:"noise,omitempty"`
	Verdict     *VerdictEvidence   `json:"verdict,omitempty"`
}

type EvidenceRef struct {
	Frame     int    `json:"frame"`
	Type      string `json:"type"`
	CacheKey  string `json:"cache_key"`
	Digest    string `json:"digest"`
	ParamHash string `json:"param_hash"`
}

type InvalidationRecord struct {
	Frame  int    `json:"frame"`
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

type CommitRecord struct {
	Ordinal      int      `json:"ordinal"`
	Frame        int      `json:"frame"`
	Type         string   `json:"type"`
	CacheKey     string   `json:"cache_key"`
	Digest       string   `json:"digest"`
	UpstreamKeys []string `json:"upstream_keys,omitempty"`
	BatchSeq     int      `json:"batch_seq,omitempty"`
	Action       string   `json:"action,omitempty"`
}

type QueueEvent struct {
	Frame  int    `json:"frame"`
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

type FailureRecord struct {
	Frame    int    `json:"frame"`
	Type     string `json:"type"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Attempts int    `json:"attempts"`
}

type Report struct {
	RootHash         string               `json:"root_hash"`
	ParamVersion     string               `json:"param_version"`
	AlgorithmVersion string               `json:"algorithm_version"`
	Frames           []FrameReport        `json:"frames"`
	Timeline         WatermarkTimeline    `json:"timeline"`
	Intervals        []Interval           `json:"intervals"`
	CommitSequence   []CommitRecord       `json:"commit_sequence"`
	Invalidations    []InvalidationRecord `json:"invalidations,omitempty"`
	QueueEvents      []QueueEvent         `json:"queue_events,omitempty"`
	Failures         []FailureRecord      `json:"failures,omitempty"`
}

func SortedInvalidations(in []InvalidationRecord) []InvalidationRecord {
	out := append([]InvalidationRecord(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Frame != out[j].Frame {
			return out[i].Frame < out[j].Frame
		}
		return out[i].Type < out[j].Type
	})
	return out
}

func MustJSON(v any) string {
	return string(CanonicalJSON(v))
}

func SafeFinite(v float64) float64 {
	if v != v || v > 1e308 || v < -1e308 {
		return 0
	}
	return v
}

func SprintFrameField(frame int, field string) string {
	return fmt.Sprintf("frame=%d field=%s", frame, field)
}
