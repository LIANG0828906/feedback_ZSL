package model

import (
	"fmt"
	"strings"
)

const (
	ParamVersion = "param-v1"
	AlgoVersion  = "algo-v1"

	CodeSchema     = "E_SCHEMA"
	CodeDim        = "E_DIM"
	CodeKernel     = "E_KERNEL"
	CodeConv       = "E_CONV"
	CodeDiff       = "E_DIFF"
	CodeCheckpoint = "E_CHECKPOINT"
	CodeResource   = "E_RESOURCE"
)

type PipelineError struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	ConfigSeq   *int   `json:"config_seq,omitempty"`
	Plane       string `json:"plane,omitempty"`
	Factor      int    `json:"factor,omitempty"`
	SourceSpace string `json:"source_space,omitempty"`
	TargetSpace string `json:"target_space,omitempty"`
	Field       string `json:"field,omitempty"`
	ChunkX      int    `json:"chunk_x,omitempty"`
	ChunkY      int    `json:"chunk_y,omitempty"`
	StageSeq    int    `json:"stage_seq,omitempty"`
}

func (e *PipelineError) Error() string {
	parts := []string{e.Code + ": " + e.Message}
	if e.ConfigSeq != nil {
		parts = append(parts, fmt.Sprintf("config_seq=%d", *e.ConfigSeq))
	}
	if e.Plane != "" {
		parts = append(parts, "plane="+e.Plane)
	}
	if e.Factor != 0 {
		parts = append(parts, fmt.Sprintf("factor=%d", e.Factor))
	}
	if e.SourceSpace != "" || e.TargetSpace != "" {
		parts = append(parts, fmt.Sprintf("%s->%s", e.SourceSpace, e.TargetSpace))
	}
	if e.Field != "" {
		parts = append(parts, "field="+e.Field)
	}
	return strings.Join(parts, " ")
}

func SchemaError(format string, args ...any) *PipelineError {
	return &PipelineError{Code: CodeSchema, Message: fmt.Sprintf(format, args...)}
}

func DimError(plane string, factor int, format string, args ...any) *PipelineError {
	return &PipelineError{Code: CodeDim, Message: fmt.Sprintf(format, args...), Plane: plane, Factor: factor}
}

func KernelError(seq int, format string, args ...any) *PipelineError {
	return &PipelineError{Code: CodeKernel, Message: fmt.Sprintf(format, args...), ConfigSeq: &seq}
}

func ConvError(seq int, source, target string, format string, args ...any) *PipelineError {
	return &PipelineError{
		Code: CodeConv, Message: fmt.Sprintf(format, args...), ConfigSeq: &seq,
		SourceSpace: source, TargetSpace: target,
	}
}

type Rect struct {
	X0 int `json:"x0"`
	Y0 int `json:"y0"`
	X1 int `json:"x1"`
	Y1 int `json:"y1"`
}

func (r Rect) Empty() bool { return r.X1 <= r.X0 || r.Y1 <= r.Y0 }
func (r Rect) Width() int  { return max(0, r.X1-r.X0) }
func (r Rect) Height() int { return max(0, r.Y1-r.Y0) }

func (r Rect) Contains(x, y int) bool {
	return x >= r.X0 && x < r.X1 && y >= r.Y0 && y < r.Y1
}

func (r Rect) Intersect(o Rect) Rect {
	return Rect{
		X0: max(r.X0, o.X0),
		Y0: max(r.Y0, o.Y0),
		X1: min(r.X1, o.X1),
		Y1: min(r.Y1, o.Y1),
	}
}

type Plane struct {
	Name       string  `json:"name"`
	Role       string  `json:"role"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	SubsampleX int     `json:"subsample_x"`
	SubsampleY int     `json:"subsample_y"`
	BitDepth   int     `json:"bit_depth"`
	Values     []int64 `json:"values"`
	ColorSpace string  `json:"color_space,omitempty"`
}

func (p *Plane) Index(x, y int) int { return y*p.Width + x }

func (p *Plane) RectValues(r Rect) []int64 {
	out := make([]int64, 0, r.Width()*r.Height())
	for y := r.Y0; y < r.Y1; y++ {
		out = append(out, p.Values[y*p.Width+r.X0:y*p.Width+r.X1]...)
	}
	return out
}

type FixedMatrix struct {
	Scale   int64       `json:"scale"`
	Rows    [3][3]int64 `json:"rows"`
	Offsets [3]int64    `json:"offsets"`
}

type StageConfig struct {
	Seq         int          `json:"seq"`
	ID          string       `json:"id"`
	Type        string       `json:"type"`
	Kernel      string       `json:"kernel,omitempty"`
	Factor      int          `json:"factor,omitempty"`
	FactorX     int          `json:"factor_x,omitempty"`
	FactorY     int          `json:"factor_y,omitempty"`
	Taps        int          `json:"taps,omitempty"`
	Radius      int          `json:"radius,omitempty"`
	FixedScale  int64        `json:"fixed_scale,omitempty"`
	Planes      []string     `json:"planes,omitempty"`
	Intent      string       `json:"intent,omitempty"`
	SourceSpace string       `json:"source_space,omitempty"`
	TargetSpace string       `json:"target_space,omitempty"`
	InputNames  []string     `json:"input_names,omitempty"`
	OutputNames []string     `json:"output_names,omitempty"`
	Matrix      *FixedMatrix `json:"matrix,omitempty"`
	BitDepth    int          `json:"bit_depth,omitempty"`
	Region      *Rect        `json:"region,omitempty"`
	Description string       `json:"description,omitempty"`
}

type Project struct {
	SchemaVersion    string        `json:"schema_version"`
	ProjectID        string        `json:"project_id"`
	Seed             int64         `json:"seed"`
	FullWidth        int           `json:"full_width"`
	FullHeight       int           `json:"full_height"`
	ChromaFormat     string        `json:"chroma_format"`
	ValueScale       int64         `json:"value_scale"`
	SupportedDepths  []int         `json:"supported_depths"`
	TruthFile        string        `json:"truth_file,omitempty"`
	TruthFingerprint string        `json:"truth_fingerprint,omitempty"`
	ParamVersion     string        `json:"param_version"`
	AlgoVersion      string        `json:"algo_version"`
	Planes           []*Plane      `json:"planes"`
	Config           []StageConfig `json:"config"`
}

type KnownPoint struct {
	ID          string `json:"id"`
	StageSeq    int    `json:"stage_seq"`
	Plane       string `json:"plane"`
	X           int    `json:"x"`
	Y           int    `json:"y"`
	ExpectedFX  int64  `json:"expected_fixed"`
	ToleranceFX int64  `json:"tolerance_fixed"`
}

type BlockBoundary struct {
	ID         string `json:"id"`
	AfterSeq   int    `json:"after_seq"`
	Plane      string `json:"plane"`
	Axis       string `json:"axis"`
	Coordinate int    `json:"coordinate"`
	LoValueFX  int64  `json:"lo_value_fixed,omitempty"`
	HiValueFX  int64  `json:"hi_value_fixed,omitempty"`
}

type TruthErrorBound struct {
	StageSeq          int    `json:"stage_seq"`
	Category          string `json:"category"`
	CumulativeBoundFX int64  `json:"cumulative_bound_fixed"`
	Basis             string `json:"basis"`
}

type TruthManifest struct {
	SchemaVersion   string            `json:"schema_version"`
	ProjectID       string            `json:"project_id"`
	Seed            int64             `json:"seed"`
	Fingerprint     string            `json:"fingerprint,omitempty"`
	KnownPoints     []KnownPoint      `json:"known_points"`
	BlockBoundaries []BlockBoundary   `json:"block_boundaries"`
	ErrorBounds     []TruthErrorBound `json:"error_bounds"`
	AnalyticNotes   []string          `json:"analytic_notes"`
}

type TruncRecord struct {
	StageSeq    int    `json:"stage_seq"`
	ChunkX      int    `json:"chunk_x"`
	ChunkY      int    `json:"chunk_y"`
	Plane       string `json:"plane,omitempty"`
	OutputX     int    `json:"output_x"`
	OutputY     int    `json:"output_y"`
	TapIndex    int    `json:"tap_index,omitempty"`
	Kind        string `json:"kind"`
	Numerator   int64  `json:"numerator"`
	Denominator int64  `json:"denominator"`
	Remainder   int64  `json:"remainder"`
}

type ResidualStat struct {
	StageSeq    int    `json:"stage_seq"`
	Category    string `json:"category"`
	Plane       string `json:"plane,omitempty"`
	Unit        string `json:"unit,omitempty"`
	Count       int    `json:"count"`
	MaxAbs      int64  `json:"max_abs"`
	SumAbs      int64  `json:"sum_abs"`
	MeanMilli   int64  `json:"mean_milli"`
	MaxAbsMilli int64  `json:"max_abs_milli,omitempty"`
}

type Tile struct {
	Plane  string  `json:"plane"`
	Rect   Rect    `json:"rect"`
	Values []int64 `json:"values"`
}

type LevelArtifact struct {
	Seq         int            `json:"seq"`
	Type        string         `json:"type"`
	Config      StageConfig    `json:"config"`
	Tiles       []Tile         `json:"tiles"`
	Truncations []TruncRecord  `json:"truncations,omitempty"`
	Residuals   []ResidualStat `json:"residuals,omitempty"`
}

type ChunkArtifact struct {
	ProjectID  string          `json:"project_id"`
	ChunkIndex int             `json:"chunk_index"`
	ChunkX     int             `json:"chunk_x"`
	ChunkY     int             `json:"chunk_y"`
	ChunkSize  int             `json:"chunk_size"`
	FullRect   Rect            `json:"full_rect"`
	Levels     []LevelArtifact `json:"levels"`
}

type InvalidationRecord struct {
	ChunkIndex int    `json:"chunk_index"`
	ChunkX     int    `json:"chunk_x"`
	ChunkY     int    `json:"chunk_y"`
	StageSeq   int    `json:"stage_seq"`
	Reason     string `json:"reason"`
	OldKey     string `json:"old_key,omitempty"`
	NewKey     string `json:"new_key,omitempty"`
}

type QueueEvent struct {
	ChunkIndex  int    `json:"chunk_index"`
	StageSeq    int    `json:"stage_seq"`
	Worker      int    `json:"worker"`
	Reason      string `json:"reason"`
	QueueLength int    `json:"queue_length"`
	BufferLimit int    `json:"buffer_limit"`
}

type DiffLocation struct {
	ChunkIndex int    `json:"chunk_index"`
	ChunkX     int    `json:"chunk_x"`
	ChunkY     int    `json:"chunk_y"`
	StageSeq   int    `json:"stage_seq"`
	Field      string `json:"field"`
	Expected   string `json:"expected,omitempty"`
	Actual     string `json:"actual,omitempty"`
}

func (d DiffLocation) Error() string {
	return fmt.Sprintf("%s: first mismatch chunk=%d coord=(%d,%d) stage=%d field=%s expected=%s actual=%s",
		CodeDiff, d.ChunkIndex, d.ChunkX, d.ChunkY, d.StageSeq, d.Field, d.Expected, d.Actual)
}
