package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

const (
	AlgorithmVersion = "algo-1.0.0"
	GeneratorVersion = "generator-1.0.0"
	SchemaVersion    = "checkpoint-1.0.0"

	CodeDecode   = "E_DECODE"
	CodeMeta     = "E_META"
	CodePara     = "E_PARA"
	CodeMerge    = "E_MERGE"
	CodeRegion   = "E_REGION"
	CodeDegen    = "E_DEGEN"
	CodeVer      = "E_VER"
	CodeDiverge  = "E_DIVERGE"
	CodeState    = "E_STATE"
	CodeSnapshot = "E_SNAPSHOT"
	CodeResource = "E_RESOURCE"
	CodeNotFound = "E_NOT_FOUND"
)

type Error struct {
	Code           string             `json:"code"`
	Message        string             `json:"message"`
	Field          string             `json:"field,omitempty"`
	Min            any                `json:"min,omitempty"`
	Max            any                `json:"max,omitempty"`
	Expected       any                `json:"expected,omitempty"`
	Actual         any                `json:"actual,omitempty"`
	VersionA       string             `json:"version_a,omitempty"`
	VersionB       string             `json:"version_b,omitempty"`
	RemainingPairs int                `json:"remaining_pairs,omitempty"`
	MinCost        float64            `json:"min_cost,omitempty"`
	BlockX         int                `json:"block_x,omitempty"`
	BlockY         int                `json:"block_y,omitempty"`
	Layer          string             `json:"layer,omitempty"`
	FirstRegionID  string             `json:"first_region_id,omitempty"`
	DiffX          int                `json:"diff_x,omitempty"`
	DiffY          int                `json:"diff_y,omitempty"`
	Extra          []RegionRef        `json:"extra,omitempty"`
	Missing        []RegionRef        `json:"missing,omitempty"`
	Metrics        *DivergenceMetrics `json:"metrics,omitempty"`
}

type RegionRef struct {
	ID   string `json:"id"`
	Area int    `json:"area"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) WithField(field string, min, max any) *Error {
	e.Field = field
	e.Min = min
	e.Max = max
	if e.Message == "" {
		e.Message = fmt.Sprintf("field %s is outside legal range [%v, %v]", field, min, max)
	}
	return e
}

type BBox struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

type RegionStat struct {
	ID        string  `json:"id"`
	Index     int     `json:"index"`
	CentroidX float64 `json:"centroid_x"`
	CentroidY float64 `json:"centroid_y"`
	Area      int     `json:"area"`
	BBox      BBox    `json:"bbox"`
	Holes     int     `json:"holes"`
}

type Tolerances struct {
	MaskIoUMin           float64 `json:"mask_iou_min"`
	MaskMismatchRatioMax float64 `json:"mask_mismatch_ratio_max"`
	CentroidPx           float64 `json:"centroid_px"`
	AreaRel              float64 `json:"area_rel"`
	HoleCount            int     `json:"hole_count"`
	PreviewRegionCount   int     `json:"preview_region_count"`
	PreviewCentroidPx    float64 `json:"preview_centroid_px"`
	PreviewAreaRel       float64 `json:"preview_area_rel"`
}

type Budget struct {
	TileSize             int   `json:"tile_size"`
	WorkerThreads        int   `json:"worker_threads"`
	PixelBufferPerThread int   `json:"pixel_buffer_per_thread"`
	PreviewDelayMS       int64 `json:"preview_delay_ms"`
	Seed                 int64 `json:"seed"`
}

type ImageManifest struct {
	ID                     string       `json:"id"`
	File                   string       `json:"file"`
	MaskFile               string       `json:"mask_file"`
	Width                  int          `json:"width"`
	Height                 int          `json:"height"`
	Channels               int          `json:"channels"`
	SHA256                 string       `json:"sha256"`
	MaskSHA256             string       `json:"mask_sha256"`
	ReferenceMaskPNGBase64 string       `json:"reference_mask_png_base64"`
	TruthFingerprint       string       `json:"truth_fingerprint"`
	Regions                []RegionStat `json:"regions"`
}

type Manifest struct {
	Seed             int64           `json:"seed"`
	GeneratorVersion string          `json:"generator_version"`
	AlgorithmVersion string          `json:"algorithm_version"`
	DataDir          string          `json:"data_dir"`
	Budget           Budget          `json:"budget"`
	Tolerances       Tolerances      `json:"tolerances"`
	Images           []ImageManifest `json:"images"`
}

type Params struct {
	ParamVersion     int64   `json:"param_version"`
	GradientOperator string  `json:"gradient_operator"`
	SmoothKernel     int     `json:"smooth_kernel"`
	FloodThreshold   float64 `json:"flood_threshold"`
	MinRegionArea    int     `json:"min_region_area"`
	MergeCostCap     float64 `json:"merge_cost_cap"`
	AreaDiffCap      int     `json:"area_diff_cap"`
	MorphRadius      int     `json:"morph_radius"`
}

func DefaultParams() Params {
	return Params{
		ParamVersion:     1,
		GradientOperator: "sobel",
		SmoothKernel:     3,
		FloodThreshold:   72,
		MinRegionArea:    24,
		MergeCostCap:     42,
		AreaDiffCap:      2500,
		MorphRadius:      1,
	}
}

func (p Params) CanonicalHash() string {
	p.ParamVersion = 0
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:12])
}

type DivergenceMetrics struct {
	RegionCountExpected  int     `json:"region_count_expected"`
	RegionCountActual    int     `json:"region_count_actual"`
	RegionCountDelta     int     `json:"region_count_delta"`
	MaxCentroidErrorPx   float64 `json:"max_centroid_error_px"`
	MaxAreaRelativeError float64 `json:"max_area_relative_error"`
	MismatchPixels       int     `json:"mismatch_pixels"`
	IoU                  float64 `json:"iou"`
}

type Judgment struct {
	ID               int64             `json:"id"`
	ParamVersion     int64             `json:"param_version"`
	ImageID          string            `json:"image_id"`
	Tolerances       Tolerances        `json:"tolerances"`
	Metrics          DivergenceMetrics `json:"metrics"`
	OK               bool              `json:"ok"`
	Code             string            `json:"code,omitempty"`
	FirstRegionID    string            `json:"first_region_id,omitempty"`
	DiffX            int               `json:"diff_x,omitempty"`
	DiffY            int               `json:"diff_y,omitempty"`
	DownsampleFactor int               `json:"downsample_factor,omitempty"`
	FloodSamples     int               `json:"flood_samples,omitempty"`
	Basis            string            `json:"basis,omitempty"`
}

type QueueRecord struct {
	BlockIndex int    `json:"block_index"`
	Layer      string `json:"layer"`
	Pixels     int    `json:"pixels"`
	Reason     string `json:"reason"`
}
