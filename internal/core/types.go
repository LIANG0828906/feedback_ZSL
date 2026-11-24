package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
)

type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

func (p Point) Valid() bool {
	return !math.IsNaN(p.X) && !math.IsInf(p.X, 0) &&
		!math.IsNaN(p.Y) && !math.IsInf(p.Y, 0)
}

// Box always stores points in top-left, top-right, bottom-right, bottom-left
// order. Boxes are convex quadrilaterals in this project.
type Box [4]Point

type Rect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

func (r Rect) Empty() bool { return r.Width <= 0 || r.Height <= 0 }

func (r Rect) Right() int  { return r.X + r.Width }
func (r Rect) Bottom() int { return r.Y + r.Height }

func (r Rect) Intersect(other Rect) Rect {
	x0 := maxInt(r.X, other.X)
	y0 := maxInt(r.Y, other.Y)
	x1 := minInt(r.Right(), other.Right())
	y1 := minInt(r.Bottom(), other.Bottom())
	if x1 <= x0 || y1 <= y0 {
		return Rect{}
	}
	return Rect{X: x0, Y: y0, Width: x1 - x0, Height: y1 - y0}
}

func (r Rect) Pixels() int { return maxInt(0, r.Width) * maxInt(0, r.Height) }

type TruthRegion struct {
	ID             int     `json:"id"`
	LineIndex      int     `json:"line_index"`
	Box            Box     `json:"box"`
	Text           string  `json:"text"`
	CharCount      int     `json:"char_count"`
	AvgStrokeWidth float64 `json:"avg_stroke_width"`
	AngleDegrees   float64 `json:"angle_degrees"`
}

type ImageTruth struct {
	ID                  string        `json:"id"`
	File                string        `json:"file"`
	Width               int           `json:"width"`
	Height              int           `json:"height"`
	Channels            int           `json:"channels"`
	DeclaredRegionCount int           `json:"declared_region_count"`
	Regions             []TruthRegion `json:"regions"`
	SHA256              string        `json:"sha256"`
	DetectionTolerance  int           `json:"detection_tolerance"`
	MinDetectionIoU     float64       `json:"min_detection_iou"`
	Description         string        `json:"description"`
}

type Manifest struct {
	SchemaVersion      int          `json:"schema_version"`
	GeneratorVersion   string       `json:"generator_version"`
	Seed               int64        `json:"seed"`
	TemplateVersion    string       `json:"template_version"`
	CreatedBy          string       `json:"created_by"`
	Images             []ImageTruth `json:"images"`
	TruthFingerprint   string       `json:"truth_fingerprint"`
	DetectionTolerance int          `json:"detection_tolerance"`
	MinDetectionIoU    float64      `json:"min_detection_iou"`
}

func (m Manifest) Image(id string) (ImageTruth, bool) {
	for _, image := range m.Images {
		if image.ID == id {
			return image, true
		}
	}
	return ImageTruth{}, false
}

type Component struct {
	ID           int     `json:"id"`
	Rect         Rect    `json:"rect"`
	PixelCount   int     `json:"pixel_count"`
	StrokeWidth  float64 `json:"stroke_width"`
	Center       Point   `json:"center"`
	SourceTile   int     `json:"source_tile"`
	LocalPixels  []Point `json:"-"`
	MeanGray     float64 `json:"mean_gray"`
	MeanGradient float64 `json:"mean_gradient"`
	MaxGradient  int     `json:"max_gradient"`
	MinimumRun   int     `json:"minimum_run"`
}

type TextLine struct {
	ID             int     `json:"id"`
	Box            Box     `json:"box"`
	AngleRadians   float64 `json:"angle_radians"`
	StrokeWidth    float64 `json:"stroke_width"`
	ComponentCount int     `json:"component_count"`
	SourceTiles    []int   `json:"source_tiles"`
	ReadingIndex   int     `json:"reading_index"`
	CandidateHash  string  `json:"candidate_hash"`
}

func (l TextLine) Center() Point {
	var c Point
	for _, p := range l.Box {
		c.X += p.X / 4
		c.Y += p.Y / 4
	}
	return c
}

func (l TextLine) AxisAligned() Rect {
	return BoxBounds(l.Box)
}

type CharResult struct {
	Index       int     `json:"index"`
	Text        string  `json:"text"`
	Confidence  float64 `json:"confidence"`
	Box         Box     `json:"box"`
	BestScore   float64 `json:"best_score"`
	SecondScore float64 `json:"second_score"`
	Margin      float64 `json:"margin"`
}

type LineResult struct {
	ImageID       string        `json:"image_id"`
	Number        int           `json:"number"`
	ReadingIndex  int           `json:"reading_index"`
	TileIndex     int           `json:"tile_index"`
	Box           Box           `json:"box"`
	State         string        `json:"state"`
	Text          string        `json:"text"`
	Characters    []CharResult  `json:"characters"`
	Confidence    float64       `json:"confidence"`
	IsEmpty       bool          `json:"is_empty"`
	EmptyReason   string        `json:"empty_reason,omitempty"`
	StrokeWidth   float64       `json:"stroke_width"`
	SegmentParams SegmentParams `json:"segment_params"`
	ParameterHash string        `json:"parameter_hash"`
	ContentHash   string        `json:"content_hash"`
	CommittedSeq  int           `json:"committed_seq"`
	Error         string        `json:"error,omitempty"`
	TruthText     string        `json:"truth_text,omitempty"`
	TruthIoU      float64       `json:"truth_iou,omitempty"`
}

type TileResult struct {
	ImageID      string     `json:"image_id"`
	Index        int        `json:"index"`
	Row          int        `json:"row"`
	Col          int        `json:"col"`
	Core         Rect       `json:"core"`
	Expanded     Rect       `json:"expanded"`
	State        string     `json:"state"`
	Candidates   []TextLine `json:"candidates"`
	CommittedSeq int        `json:"committed_seq"`
	ContentHash  string     `json:"content_hash"`
	Error        string     `json:"error,omitempty"`
}

type Progress struct {
	CommittedTiles int `json:"committed_tiles"`
	TotalTiles     int `json:"total_tiles"`
	CommittedLines int `json:"committed_lines"`
	TotalLines     int `json:"total_lines"`
}

type Stats struct {
	LineCount         int     `json:"line_count"`
	EmptyLineCount    int     `json:"empty_line_count"`
	CharacterCount    int     `json:"character_count"`
	AverageConfidence float64 `json:"average_confidence"`
	MinConfidence     float64 `json:"min_confidence"`
	MaxConfidence     float64 `json:"max_confidence"`
	CommittedTiles    int     `json:"committed_tiles"`
	CommittedLines    int     `json:"committed_lines"`
}

type CommitSequence struct {
	Tiles []int `json:"tiles"`
	Lines []int `json:"lines"`
}

type OCRCandidateLine struct {
	Box            Box     `json:"box"`
	AngleRadians   float64 `json:"angle_radians"`
	StrokeWidth    float64 `json:"stroke_width"`
	ComponentCount int     `json:"component_count"`
	SourceTiles    []int   `json:"source_tiles"`
}

type ImageResult struct {
	ImageID          string         `json:"image_id"`
	TruthFingerprint string         `json:"truth_fingerprint"`
	ImageHash        string         `json:"image_hash"`
	TemplateVersion  string         `json:"template_version"`
	ParameterVersion string         `json:"parameter_version"`
	AlgorithmVersion string         `json:"algorithm_version"`
	Tiles            []TileResult   `json:"tiles"`
	Lines            []LineResult   `json:"lines"`
	Stats            Stats          `json:"stats"`
	Commits          CommitSequence `json:"commit_sequence"`
	Progress         Progress       `json:"progress"`
}

type RunResult struct {
	RunKind          string        `json:"run_kind"`
	Seed             int64         `json:"seed"`
	TemplateVersion  string        `json:"template_version"`
	ParameterVersion string        `json:"parameter_version"`
	AlgorithmVersion string        `json:"algorithm_version"`
	Images           []ImageResult `json:"images"`
}

func (r RunResult) Image(id string) (ImageResult, bool) {
	for _, image := range r.Images {
		if image.ImageID == id {
			return image, true
		}
	}
	return ImageResult{}, false
}

type SegmentParams struct {
	MinCharGap     float64 `json:"min_char_gap"`
	MaxCharWidth   float64 `json:"max_char_width"`
	MinCharArea    int     `json:"min_char_area"`
	MatchThreshold float64 `json:"match_threshold"`
	MarginScale    float64 `json:"margin_scale"`
}

func DefaultSegmentParams() SegmentParams {
	return SegmentParams{
		MinCharGap:     0.5,
		MaxCharWidth:   96,
		MinCharArea:    8,
		MatchThreshold: 0.34,
		MarginScale:    2.5,
	}
}

type DetectorParams struct {
	TileWidth            int     `json:"tile_width"`
	TileHeight           int     `json:"tile_height"`
	Band                 int     `json:"band"`
	MergeIoU             float64 `json:"merge_iou"`
	MergeStrokeRatio     float64 `json:"merge_stroke_ratio"`
	MinComponentArea     int     `json:"min_component_area"`
	StrokeRatioTolerance float64 `json:"stroke_ratio_tolerance"`
	MinComponentPixels   int     `json:"min_component_pixels"`
	AdaptiveRadius       int     `json:"adaptive_radius"`
	MaxWorkers           int     `json:"max_workers"`
	BufferBytesPerWorker int     `json:"buffer_bytes_per_worker"`
}

func DefaultDetectorParams() DetectorParams {
	return DetectorParams{
		TileWidth:            360,
		TileHeight:           300,
		Band:                 56,
		MergeIoU:             0.25,
		MergeStrokeRatio:     0.65,
		MinComponentArea:     12,
		StrokeRatioTolerance: 0.75,
		MinComponentPixels:   6,
		AdaptiveRadius:       18,
		MaxWorkers:           4,
		BufferBytesPerWorker: 32 << 20,
	}
}

func RequiredBand(p DetectorParams) int {
	side := minInt(p.TileWidth, p.TileHeight)
	return int(math.Ceil(p.MergeIoU * float64(side) * 0.5))
}

func HashAny(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	var generic any
	if err := json.Unmarshal(b, &generic); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(generic)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TruthFingerprint(manifest Manifest) (string, error) {
	type truthImage struct {
		ID                  string        `json:"id"`
		Width               int           `json:"width"`
		Height              int           `json:"height"`
		Channels            int           `json:"channels"`
		DeclaredRegionCount int           `json:"declared_region_count"`
		Regions             []TruthRegion `json:"regions"`
	}
	payload := struct {
		GeneratorVersion string       `json:"generator_version"`
		TemplateVersion  string       `json:"template_version"`
		Images           []truthImage `json:"images"`
	}{
		GeneratorVersion: manifest.GeneratorVersion,
		TemplateVersion:  manifest.TemplateVersion,
	}
	for _, image := range manifest.Images {
		payload.Images = append(payload.Images, truthImage{
			ID:                  image.ID,
			Width:               image.Width,
			Height:              image.Height,
			Channels:            image.Channels,
			DeclaredRegionCount: image.DeclaredRegionCount,
			Regions:             image.Regions,
		})
	}
	return HashAny(payload)
}

func SortTiles(tiles []TileResult) {
	sort.Slice(tiles, func(i, j int) bool {
		if tiles[i].Row != tiles[j].Row {
			return tiles[i].Row < tiles[j].Row
		}
		return tiles[i].Col < tiles[j].Col
	})
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
