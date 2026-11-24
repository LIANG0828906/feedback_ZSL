package model

import (
	"errors"
	"fmt"
)

const (
	CodeDecode  = "E_DECODE"
	CodeMeta    = "E_META"
	CodePara    = "E_PARA"
	CodeRegion  = "E_REGION"
	CodeQual    = "E_QUAL"
	CodeState   = "E_STATE"
	CodeSeed    = "E_SEED"
	CodeDiverge = "E_DIVERGE"
)

type Error struct {
	Code    string `json:"code"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("%s field=%s: %s", e.Code, e.Field, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func NewError(code, field, format string, args ...any) *Error {
	return &Error{Code: code, Field: field, Message: fmt.Sprintf(format, args...)}
}

func WithDetails(err *Error, details any) *Error {
	err.Details = details
	return err
}

func AsError(err error) (*Error, bool) {
	var pe *Error
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}

type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type IntPoint struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type BBox struct {
	MinX   int `json:"min_x"`
	MinY   int `json:"min_y"`
	MaxX   int `json:"max_x"`
	MaxY   int `json:"max_y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

type Region struct {
	ID       int     `json:"id"`
	Label    string  `json:"label,omitempty"`
	Centroid Point   `json:"centroid"`
	Area     int     `json:"area"`
	BBox     BBox    `json:"bbox"`
	Contour  []Point `json:"contour,omitempty"`
	Closed   bool    `json:"closed,omitempty"`
	Extra    bool    `json:"extra,omitempty"`
	Missing  bool    `json:"missing,omitempty"`
}

type Tile struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

type Budget struct {
	WorkerCount            int `json:"worker_count"`
	PixelBufferPerThread   int `json:"pixel_buffer_per_thread"`
	PendingTaskQueueBudget int `json:"pending_task_queue_budget"`
}

type QualityTolerance struct {
	CentroidPixels float64 `json:"centroid_pixels"`
	AreaRelative   float64 `json:"area_relative"`
	BBoxPixels     float64 `json:"bbox_pixels"`
}

type DivergenceTolerance struct {
	CentroidPixels float64 `json:"centroid_pixels"`
	AreaRelative   float64 `json:"area_relative"`
	PixelMismatch  float64 `json:"pixel_mismatch_ratio"`
}

type DeclaredLimits struct {
	Tile             Tile                `json:"tile"`
	Budget           Budget              `json:"budget"`
	PreviewLatencyMS int                 `json:"preview_latency_ms"`
	Quality          QualityTolerance    `json:"quality"`
	Divergence       DivergenceTolerance `json:"divergence"`
}

type SceneManifest struct {
	ID               string         `json:"id"`
	SceneType        string         `json:"scene_type"`
	ImagePath        string         `json:"image_path"`
	TruthMaskPath    string         `json:"truth_mask_path"`
	Width            int            `json:"width"`
	Height           int            `json:"height"`
	Channels         int            `json:"channels"`
	Tile             Tile           `json:"tile"`
	Cols             int            `json:"cols"`
	Rows             int            `json:"rows"`
	ChunkCount       int            `json:"chunk_count"`
	Declared         DeclaredLimits `json:"declared"`
	Seeds            []IntPoint     `json:"seeds"`
	TruthRegionCount int            `json:"truth_region_count"`
	TruthRegions     []Region       `json:"truth_regions"`
	TruthFingerprint string         `json:"truth_fingerprint"`
	Parameters       ParameterSet   `json:"parameters"`
}

type SampleManifest struct {
	SchemaVersion    string          `json:"schema_version"`
	Generator        string          `json:"generator"`
	Seed             int64           `json:"seed"`
	AlgorithmVersion string          `json:"algorithm_version"`
	Samples          []SceneManifest `json:"samples"`
}

type ParameterSet struct {
	ParamVersion      string     `json:"param_version"`
	ColorTolerance    float64    `json:"color_tolerance"`
	GradientThreshold float64    `json:"gradient_threshold"`
	StructureSize     int        `json:"structure_size"`
	BinaryThreshold   float64    `json:"binary_threshold"`
	MinRegionArea     int        `json:"min_region_area"`
	MaxIterations     int        `json:"max_iterations"`
	Seeds             []IntPoint `json:"seeds"`
}

type Stage string

const (
	StageGrow  Stage = "grow"
	StageClean Stage = "clean"
	StageStats Stage = "stats"
)

type StageStatus string

const (
	StatusPending    StageStatus = "pending"
	StatusProcessing StageStatus = "processing"
	StatusCommitted  StageStatus = "committed"
	StatusFailed     StageStatus = "failed"
)

type ChunkCoord struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type ChunkKey struct {
	ChunkCoord
	Stage Stage `json:"stage"`
}

func (k ChunkKey) ID() string {
	return fmt.Sprintf("c%02d_%02d.%s", k.X, k.Y, k.Stage)
}
