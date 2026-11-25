package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

const (
	AlgorithmVersion = "raster-int-alpha-v1"
	OrderVersion     = "priority-asc-then-stroke-no-v1"
	ParamsVersion    = "weight-bp10000-hardness-bp10000-v1"
	TopologyVersion  = "topology-fg8-hole4-gradient36-v1"
	SchemaInternal   = "v3"

	WeightScaleBP   = 10000
	HardnessScaleBP = 10000
	AlphaThreshold  = 128
	DirectionBins   = 36
)

type ErrorCode string

const (
	CodeSchema ErrorCode = "E_SCHEMA"
	CodeRound  ErrorCode = "E_ROUND"
	CodePara   ErrorCode = "E_PARA"
	CodeTop    ErrorCode = "E_TOP"
	CodeCache  ErrorCode = "E_CACHE"
	CodeState  ErrorCode = "E_STATE"
)

type Error struct {
	Code     ErrorCode      `json:"code"`
	Field    string         `json:"field,omitempty"`
	StrokeID string         `json:"stroke_id,omitempty"`
	Message  string         `json:"message"`
	Original any            `json:"original,omitempty"`
	Residual any            `json:"residual,omitempty"`
	Details  map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e.StrokeID != "" {
		return fmt.Sprintf("%s field=%s stroke=%s: %s", e.Code, e.Field, e.StrokeID, e.Message)
	}
	if e.Field != "" {
		return fmt.Sprintf("%s field=%s: %s", e.Code, e.Field, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func NewError(code ErrorCode, field, strokeID, format string, args ...any) *Error {
	return &Error{Code: code, Field: field, StrokeID: strokeID, Message: fmt.Sprintf(format, args...)}
}

type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type BBox struct {
	MinX int `json:"min_x"`
	MinY int `json:"min_y"`
	MaxX int `json:"max_x"`
	MaxY int `json:"max_y"`
}

func (b BBox) Width() int  { return b.MaxX - b.MinX + 1 }
func (b BBox) Height() int { return b.MaxY - b.MinY + 1 }

type Mask struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Pix    []byte `json:"pix"`
}

func NewMask(width, height int) Mask {
	return Mask{Width: width, Height: height, Pix: make([]byte, width*height)}
}

func (m Mask) Empty() bool { return m.Width == 0 || m.Height == 0 }

func (m Mask) Index(x, y int) int { return y*m.Width + x }

func (m Mask) InBounds(x, y int) bool {
	return x >= 0 && y >= 0 && x < m.Width && y < m.Height
}

func (m Mask) At(x, y int) byte {
	if !m.InBounds(x, y) {
		return 0
	}
	return m.Pix[m.Index(x, y)]
}

func (m Mask) Clone() Mask {
	pix := make([]byte, len(m.Pix))
	copy(pix, m.Pix)
	return Mask{Width: m.Width, Height: m.Height, Pix: pix}
}

func (m Mask) Binary(threshold byte) Mask {
	out := NewMask(m.Width, m.Height)
	for i, v := range m.Pix {
		if v >= threshold {
			out.Pix[i] = 1
		}
	}
	return out
}

func (m Mask) Equal(other Mask) bool {
	if m.Width != other.Width || m.Height != other.Height || len(m.Pix) != len(other.Pix) {
		return false
	}
	for i := range m.Pix {
		if m.Pix[i] != other.Pix[i] {
			return false
		}
	}
	return true
}

type Chunk struct {
	ID     string `json:"id"`
	IndexX int    `json:"index_x"`
	IndexY int    `json:"index_y"`
	X0     int    `json:"x0"`
	Y0     int    `json:"y0"`
	X1     int    `json:"x1"`
	Y1     int    `json:"y1"`
}

func BuildChunks(width, height, size int) []Chunk {
	if size <= 0 {
		size = 16
	}
	var chunks []Chunk
	for y0, cy := 0, 0; y0 < height; y0, cy = y0+size, cy+1 {
		y1 := min(y0+size, height)
		for x0, cx := 0, 0; x0 < width; x0, cx = x0+size, cx+1 {
			x1 := min(x0+size, width)
			chunks = append(chunks, Chunk{
				ID:     fmt.Sprintf("c%02d-%02d", cy, cx),
				IndexX: cx,
				IndexY: cy,
				X0:     x0,
				Y0:     y0,
				X1:     x1,
				Y1:     y1,
			})
		}
	}
	return chunks
}

func (c Chunk) Width() int  { return c.X1 - c.X0 }
func (c Chunk) Height() int { return c.Y1 - c.Y0 }

type FieldTrace struct {
	Field         string `json:"field"`
	SourcePath    string `json:"source_path"`
	SourceValue   any    `json:"source_value,omitempty"`
	InternalField string `json:"internal_field"`
	InternalValue any    `json:"internal_value,omitempty"`
	Conversion    string `json:"conversion"`
	Residual      int    `json:"residual,omitempty"`
}

type Stroke struct {
	ID            string       `json:"id"`
	No            int          `json:"no"`
	Priority      int          `json:"priority"`
	From          Point        `json:"from"`
	To            Point        `json:"to"`
	WidthPx       int          `json:"width_px"`
	WeightBP      int          `json:"weight_bp"`
	HardnessBP    int          `json:"hardness_bp"`
	HasHardness   bool         `json:"has_hardness"`
	DeclaredIndex int          `json:"declared_index"`
	SourceVersion string       `json:"source_version"`
	ParamVersion  string       `json:"param_version"`
	ContentHash   string       `json:"content_hash"`
	Traces        []FieldTrace `json:"traces,omitempty"`
}

type OrderDecision struct {
	ChunkID              string   `json:"chunk_id"`
	Basis                string   `json:"basis"`
	Participating        []string `json:"participating_strokes"`
	ParticipatingNumbers []int    `json:"participating_numbers"`
	ResolvedOrder        []string `json:"resolved_order"`
	Winner               string   `json:"winner"`
	WinnerBasis          string   `json:"winner_basis"`
}

type DomainReport struct {
	Index               int       `json:"index"`
	ID                  string    `json:"id"`
	Area                int       `json:"area"`
	CentroidX           float64   `json:"centroid_x"`
	CentroidY           float64   `json:"centroid_y"`
	BBox                BBox      `json:"bbox"`
	Holes               int       `json:"holes"`
	RingWidthPx         float64   `json:"ring_width_px"`
	DirectionBins       []int     `json:"direction_bins"`
	DirectionDegrees    []float64 `json:"direction_degrees"`
	DirectionConfidence float64   `json:"direction_confidence"`
}

type TopologyReport struct {
	Version       string         `json:"version"`
	Algorithm     string         `json:"algorithm"`
	Threshold     byte           `json:"threshold"`
	DomainCount   int            `json:"domain_count"`
	Domains       []DomainReport `json:"domains"`
	FirstMismatch string         `json:"first_mismatch,omitempty"`
}

type Project struct {
	ID             string          `json:"id"`
	Seed           int64           `json:"seed"`
	Width          int             `json:"width"`
	Height         int             `json:"height"`
	ChunkSize      int             `json:"chunk_size"`
	SourceVersion  string          `json:"source_version"`
	LoadedVersion  string          `json:"loaded_version"`
	Strokes        []Stroke        `json:"strokes"`
	Chunks         []Chunk         `json:"chunks"`
	Candidate      Mask            `json:"candidate"`
	Reference      Mask            `json:"reference"`
	Truth          TopologyReport  `json:"truth"`
	Traces         []FieldTrace    `json:"traces,omitempty"`
	OrderDecisions []OrderDecision `json:"order_decisions,omitempty"`
}

type ChunkResult struct {
	ChunkID     string `json:"chunk_id"`
	CacheKey    string `json:"cache_key"`
	InputHash   string `json:"input_hash"`
	OutputHash  string `json:"output_hash"`
	Coverage    []byte `json:"coverage"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Reused      bool   `json:"reused,omitempty"`
	WaitMillis  int64  `json:"wait_millis,omitempty"`
	QueueReason string `json:"queue_reason,omitempty"`
}

type InvalidationItem struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Reason   string `json:"reason"`
	StrokeID string `json:"stroke_id,omitempty"`
	Field    string `json:"field,omitempty"`
	OldValue string `json:"old_value,omitempty"`
	NewValue string `json:"new_value,omitempty"`
}

type InvalidationPlan struct {
	Items              []InvalidationItem `json:"items"`
	AffectedStrokes    []string           `json:"affected_strokes"`
	AffectedChunks     []string           `json:"affected_chunks"`
	InvalidateTopology bool               `json:"invalidate_topology"`
	ReusedChunks       []string           `json:"reused_chunks"`
}

type RunResult struct {
	Mode            string            `json:"mode"`
	ProjectID       string            `json:"project_id"`
	FinalCoverage   Mask              `json:"final_coverage"`
	FinalMask       Mask              `json:"final_mask"`
	Chunks          []ChunkResult     `json:"chunks"`
	Topology        TopologyReport    `json:"topology"`
	CommitSequence  []string          `json:"commit_sequence"`
	OrderDecisions  []OrderDecision   `json:"order_decisions"`
	Invalidation    *InvalidationPlan `json:"invalidation,omitempty"`
	QueueEvents     []QueueEvent      `json:"queue_events,omitempty"`
	IsolationEvents []IsolationEvent  `json:"isolation_events,omitempty"`
	KilledAfter     int               `json:"killed_after,omitempty"`
}

type QueueEvent struct {
	ChunkID    string `json:"chunk_id"`
	StrokeID   string `json:"stroke_id,omitempty"`
	Reason     string `json:"reason"`
	WaitNanos  int64  `json:"wait_nanos"`
	WaitMillis int64  `json:"wait_millis"`
}

type IsolationEvent struct {
	TaskID  string `json:"task_id"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type PathComparison struct {
	Equal               bool   `json:"equal"`
	CoverageEqual       bool   `json:"coverage_equal"`
	TopologyEqual       bool   `json:"topology_equal"`
	CommitSequenceEqual bool   `json:"commit_sequence_equal"`
	FirstPixelX         *int   `json:"first_pixel_x,omitempty"`
	FirstPixelY         *int   `json:"first_pixel_y,omitempty"`
	FirstDomain         *int   `json:"first_domain,omitempty"`
	RealtimeKey         string `json:"realtime_key,omitempty"`
	ResumeKey           string `json:"resume_key,omitempty"`
	FullKey             string `json:"full_key,omitempty"`
	Message             string `json:"message"`
}

func HashCanonical(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("hash-error:%v", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func HashBytes(prefix string, parts ...[]byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(prefix))
	for _, p := range parts {
		var lenBuf [8]byte
		for i := 0; i < 8; i++ {
			lenBuf[i] = byte(uint64(len(p)) >> (8 * i))
		}
		_, _ = h.Write(lenBuf[:])
		_, _ = h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
