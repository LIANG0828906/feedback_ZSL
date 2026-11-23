package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"math"
	"sort"
	"strings"
)

const (
	AlgorithmVersion  = "ocr-workbench/1.0.0"
	ManifestSchema    = "1.0"
	TruthSchema       = "1.0"
	DefaultBaseSeed   = int64(20261124)
	DefaultTileSize   = 256
	DefaultBandFactor = 6.0
	DefaultMergeIoU   = 0.18
	DefaultStrokeTol  = 0.55
	DefaultConfidence = 0.55
	DefaultTolerance  = 1
)

type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type Rect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

func (r Rect) Right() int  { return r.X + r.W }
func (r Rect) Bottom() int { return r.Y + r.H }
func (r Rect) Area() int   { return r.W * r.H }
func (r Rect) Empty() bool { return r.W <= 0 || r.H <= 0 }

func (r Rect) Intersection(other Rect) Rect {
	x1 := maxInt(r.X, other.X)
	y1 := maxInt(r.Y, other.Y)
	x2 := minInt(r.Right(), other.Right())
	y2 := minInt(r.Bottom(), other.Bottom())
	if x2 <= x1 || y2 <= y1 {
		return Rect{}
	}
	return Rect{X: x1, Y: y1, W: x2 - x1, H: y2 - y1}
}

func (r Rect) Union(other Rect) Rect {
	if r.Empty() {
		return other
	}
	if other.Empty() {
		return r
	}
	x1 := minInt(r.X, other.X)
	y1 := minInt(r.Y, other.Y)
	x2 := maxInt(r.Right(), other.Right())
	y2 := maxInt(r.Bottom(), other.Bottom())
	return Rect{X: x1, Y: y1, W: x2 - x1, H: y2 - y1}
}

func (r Rect) IoU(other Rect) float64 {
	inter := r.Intersection(other).Area()
	if inter == 0 {
		return 0
	}
	union := r.Area() + other.Area() - inter
	if union <= 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func (r Rect) Center() Point {
	return Point{X: r.X + r.W/2, Y: r.Y + r.H/2}
}

type TruthRegion struct {
	ID             string   `json:"id"`
	LineIndex      int      `json:"line_index"`
	Text           string   `json:"text"`
	CharCount      int      `json:"char_count"`
	AvgStrokeWidth float64  `json:"avg_stroke_width"`
	Quad           [4]Point `json:"quad"`
	BBox           Rect     `json:"bbox"`
}

type TruthFile struct {
	SchemaVersion string        `json:"schema_version"`
	ImageID       string        `json:"image_id"`
	Width         int           `json:"width"`
	Height        int           `json:"height"`
	Channels      int           `json:"channels"`
	RegionCount   int           `json:"region_count"`
	Regions       []TruthRegion `json:"regions"`
}

type SampleMeta struct {
	ID             string  `json:"id"`
	Title          string  `json:"title"`
	Description    string  `json:"description"`
	Image          string  `json:"image"`
	Truth          string  `json:"truth"`
	Width          int     `json:"width"`
	Height         int     `json:"height"`
	Channels       int     `json:"channels"`
	RegionCount    int     `json:"region_count"`
	LineCount      int     `json:"line_count"`
	Seed           int64   `json:"seed"`
	Generator      string  `json:"generator"`
	RotationDegree float64 `json:"rotation_degree"`
	ImageHash      string  `json:"image_hash"`
}

type Manifest struct {
	SchemaVersion    string       `json:"schema_version"`
	GeneratorVersion string       `json:"generator_version"`
	Seed             int64        `json:"seed"`
	FontVersion      string       `json:"font_version"`
	GeneratedLocally bool         `json:"generated_locally"`
	NetworkUsed      bool         `json:"network_used"`
	Samples          []SampleMeta `json:"samples"`
}

type LoadedSample struct {
	Meta        SampleMeta
	Manifest    Manifest
	Truth       TruthFile
	ImageBytes  []byte
	Image       image.Image
	Gray        *image.Gray
	Width       int
	Height      int
	Channels    int
	ContentHash string
	FilePath    string
	TruthPath   string
}

type SplitParams struct {
	Algorithm      string  `json:"algorithm"`
	MinGapRatio    float64 `json:"min_gap_ratio"`
	SpaceGapRatio  float64 `json:"space_gap_ratio"`
	PaddingRatio   float64 `json:"padding_ratio"`
	DeskewMaxDeg   float64 `json:"deskew_max_deg"`
	DeskewStepDeg  float64 `json:"deskew_step_deg"`
	TemplateWidth  int     `json:"template_width"`
	TemplateHeight int     `json:"template_height"`
}

func DefaultSplitParams() SplitParams {
	return SplitParams{
		Algorithm:      "column-projection+deskew/v1",
		MinGapRatio:    0.35,
		SpaceGapRatio:  1.55,
		PaddingRatio:   0.45,
		DeskewMaxDeg:   10,
		DeskewStepDeg:  0.25,
		TemplateWidth:  24,
		TemplateHeight: 32,
	}
}

type Params struct {
	TileSize            int         `json:"tile_size"`
	BandFactor          float64     `json:"band_factor"`
	MinBand             int         `json:"min_band"`
	MergeIoU            float64     `json:"merge_iou"`
	StrokeConsistency   float64     `json:"stroke_consistency"`
	MinCandidates       int         `json:"min_candidates"`
	ConfidenceThreshold float64     `json:"confidence_threshold"`
	DetectorMinArea     int         `json:"detector_min_area"`
	CountTolerance      int         `json:"count_tolerance"`
	Split               SplitParams `json:"split"`
	TemplateVersion     string      `json:"template_version"`
}

func DefaultParams(templateVersion string) Params {
	return Params{
		TileSize:            DefaultTileSize,
		BandFactor:          DefaultBandFactor,
		MinBand:             4,
		MergeIoU:            DefaultMergeIoU,
		StrokeConsistency:   DefaultStrokeTol,
		MinCandidates:       0,
		ConfidenceThreshold: DefaultConfidence,
		DetectorMinArea:     3,
		CountTolerance:      DefaultTolerance,
		Split:               DefaultSplitParams(),
		TemplateVersion:     templateVersion,
	}
}

func (p Params) Validate() *AppError {
	if p.TileSize < 64 || p.TileSize > 4096 {
		return ParaError("tile_size", "64..4096", p.TileSize)
	}
	if p.BandFactor <= 0 || p.BandFactor > 32 {
		return ParaError("band_factor", ">0 and <=32", p.BandFactor)
	}
	if p.MinBand < 0 || p.MinBand > p.TileSize/2 {
		return ParaError("min_band", fmt.Sprintf("0..%d", p.TileSize/2-1), p.MinBand)
	}
	if p.MergeIoU < 0 || p.MergeIoU > 1 {
		return ParaError("merge_iou", "0..1", p.MergeIoU)
	}
	if p.StrokeConsistency < 0 || p.StrokeConsistency > 1 {
		return ParaError("stroke_consistency", "0..1", p.StrokeConsistency)
	}
	if p.MinCandidates < 0 {
		return ParaError("min_candidates", ">=0", p.MinCandidates)
	}
	if p.ConfidenceThreshold < 0 || p.ConfidenceThreshold > 1 {
		return ParaError("confidence_threshold", "0..1", p.ConfidenceThreshold)
	}
	if p.DetectorMinArea < 1 {
		return ParaError("detector_min_area", ">=1", p.DetectorMinArea)
	}
	if p.CountTolerance < 0 {
		return ParaError("count_tolerance", ">=0", p.CountTolerance)
	}
	if p.Split.PaddingRatio < 0 || p.Split.PaddingRatio > 2 {
		return ParaError("split.padding_ratio", "0..2", p.Split.PaddingRatio)
	}
	if p.Split.DeskewMaxDeg < 0 || p.Split.DeskewMaxDeg > 20 {
		return ParaError("split.deskew_max_deg", "0..20", p.Split.DeskewMaxDeg)
	}
	if p.Split.DeskewStepDeg <= 0 || p.Split.DeskewStepDeg > 2 {
		return ParaError("split.deskew_step_deg", ">0 and <=2", p.Split.DeskewStepDeg)
	}
	if p.Split.TemplateWidth < 8 || p.Split.TemplateHeight < 8 {
		return ParaError("split.template_size", ">=8x8", fmt.Sprintf("%dx%d", p.Split.TemplateWidth, p.Split.TemplateHeight))
	}
	if strings.TrimSpace(p.TemplateVersion) == "" {
		return ParaError("template_version", "non-empty", "")
	}
	return nil
}

func (p Params) EffectiveBand(avgStroke float64) (int, *AppError) {
	if p.BandFactor <= 0 {
		return 0, ParaError("band_factor", ">0", p.BandFactor)
	}
	band := int(math.Round(avgStroke * p.BandFactor))
	if band < p.MinBand {
		band = p.MinBand
	}
	if band <= 0 {
		return 0, ParaError("band_width", ">0", band)
	}
	if band >= p.TileSize/2 {
		return 0, ParaError("band_width", fmt.Sprintf("< tile_size/2 (%d)", p.TileSize/2), band)
	}
	return band, nil
}

type SnapshotIdentity struct {
	SampleID         string `json:"sample_id"`
	ImageHash        string `json:"image_hash"`
	TemplateVersion  string `json:"template_version"`
	ParamVersion     string `json:"param_version"`
	AlgorithmVersion string `json:"algorithm_version"`
	BandWidth        int    `json:"band_width"`
}

func (p Params) StructuralVersion(band int) string {
	copy := p
	copy.MergeIoU = -1
	copy.StrokeConsistency = -1
	copy.ConfidenceThreshold = -1
	copy.MinCandidates = -1
	payload := struct {
		Params Params `json:"params"`
		Band   int    `json:"effective_band"`
	}{Params: copy, Band: band}
	return HashJSON(payload)
}

func (p Params) TuningVersion() string {
	payload := struct {
		MergeIoU            float64 `json:"merge_iou"`
		StrokeConsistency   float64 `json:"stroke_consistency"`
		MinCandidates       int     `json:"min_candidates"`
		ConfidenceThreshold float64 `json:"confidence_threshold"`
	}{
		MergeIoU:            p.MergeIoU,
		StrokeConsistency:   p.StrokeConsistency,
		MinCandidates:       p.MinCandidates,
		ConfidenceThreshold: p.ConfidenceThreshold,
	}
	return HashJSON(payload)
}

func (p Params) SplitVersion() string {
	return HashJSON(p.Split)
}

func HashJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type Tile struct {
	Index     int `json:"index"`
	Row       int `json:"row"`
	Col       int `json:"col"`
	X         int `json:"x"`
	Y         int `json:"y"`
	W         int `json:"w"`
	H         int `json:"h"`
	CoreX     int `json:"core_x"`
	CoreY     int `json:"core_y"`
	CoreW     int `json:"core_w"`
	CoreH     int `json:"core_h"`
	BandWidth int `json:"band_width"`
}

func (t Tile) Bounds() Rect {
	return Rect{X: t.X, Y: t.Y, W: t.W, H: t.H}
}

func (t Tile) Core() Rect {
	return Rect{X: t.CoreX, Y: t.CoreY, W: t.CoreW, H: t.CoreH}
}

type Candidate struct {
	ID            string  `json:"id"`
	TileIndex     int     `json:"tile_index"`
	BBox          Rect    `json:"bbox"`
	Area          int     `json:"area"`
	StrokeWidth   float64 `json:"stroke_width"`
	GradientScore float64 `json:"gradient_score"`
	PixelHash     string  `json:"pixel_hash"`
}

type DetectedLine struct {
	ReadingIndex      int      `json:"reading_index"`
	BBox              Rect     `json:"bbox"`
	Quad              [4]Point `json:"quad"`
	CandidateIDs      []string `json:"candidate_ids"`
	CandidateCount    int      `json:"candidate_count"`
	AvgStrokeWidth    float64  `json:"avg_stroke_width"`
	PrimaryTileIndex  int      `json:"primary_tile_index"`
	TileSpan          []int    `json:"tile_span"`
	MergeIoU          float64  `json:"merge_iou"`
	StrokeConsistency float64  `json:"stroke_consistency"`
}

type CharResult struct {
	Index         int     `json:"index"`
	Rune          string  `json:"rune"`
	BBox          Rect    `json:"bbox"`
	Confidence    float64 `json:"confidence"`
	MatchScore    float64 `json:"match_score"`
	RunnerUp      string  `json:"runner_up,omitempty"`
	RunnerUpScore float64 `json:"runner_up_score,omitempty"`
}

type LineRecognition struct {
	Text                string       `json:"text"`
	Empty               bool         `json:"empty"`
	Confidence          float64      `json:"confidence"`
	Characters          []CharResult `json:"characters"`
	DeskewDegrees       float64      `json:"deskew_degrees"`
	TemplateVersion     string       `json:"template_version"`
	SplitParams         SplitParams  `json:"split_params"`
	SplitVersion        string       `json:"split_version"`
	Accepted            bool         `json:"accepted"`
	ConfidenceThreshold float64      `json:"confidence_threshold"`
}

type LineCommitSnapshot struct {
	Line              DetectedLine    `json:"line"`
	Recognition       LineRecognition `json:"recognition"`
	Params            Params          `json:"params"`
	ImageHash         string          `json:"image_hash"`
	ParamVersion      string          `json:"param_version"`
	TuningVersion     string          `json:"tuning_version"`
	TemplateVersion   string          `json:"template_version"`
	AlgorithmVersion  string          `json:"algorithm_version"`
	CommittedUnixNano int64           `json:"committed_unix_nano"`
}

type TileCommitPayload struct {
	Tile              Tile        `json:"tile"`
	Candidates        []Candidate `json:"candidates"`
	ImageHash         string      `json:"image_hash"`
	TemplateVersion   string      `json:"template_version"`
	ParamVersion      string      `json:"param_version"`
	TuningVersion     string      `json:"tuning_version"`
	AlgorithmVersion  string      `json:"algorithm_version"`
	CommittedUnixNano int64       `json:"committed_unix_nano"`
}

type State string

const (
	TilePending     State = "pending"
	TileDetecting   State = "detecting"
	TileCommitted   State = "committed"
	TileFailed      State = "failed"
	LinePending     State = "pending"
	LineRecognizing State = "recognizing"
	LineCommitted   State = "committed"
	LineFailed      State = "failed"
)

func (s State) Terminal() bool { return s == TileCommitted || s == LineCommitted }

func CanTileTransition(from, to State) bool {
	switch from {
	case TilePending:
		return to == TileDetecting || to == TileFailed
	case TileDetecting:
		return to == TileCommitted || to == TileFailed
	case TileFailed:
		return to == TileDetecting
	default:
		return false
	}
}

func CanLineTransition(from, to State) bool {
	switch from {
	case LinePending:
		return to == LineRecognizing || to == LineFailed
	case LineRecognizing:
		return to == LineCommitted || to == LineFailed
	case LineFailed:
		return to == LineRecognizing
	default:
		return false
	}
}

type TileView struct {
	Tile
	State          State       `json:"state"`
	CandidateCount int         `json:"candidate_count"`
	Candidates     []Candidate `json:"candidates,omitempty"`
	Error          *AppError   `json:"error,omitempty"`
	CommitSeq      int64       `json:"commit_seq,omitempty"`
	PayloadHash    string      `json:"payload_hash,omitempty"`
}

type LineView struct {
	ReadingIndex int                 `json:"reading_index"`
	BBox         Rect                `json:"bbox"`
	Quad         [4]Point            `json:"quad"`
	State        State               `json:"state"`
	Text         string              `json:"text"`
	Empty        bool                `json:"empty"`
	Confidence   float64             `json:"confidence"`
	Accepted     bool                `json:"accepted"`
	Characters   []CharResult        `json:"characters"`
	Snapshot     *LineCommitSnapshot `json:"snapshot,omitempty"`
	Error        *AppError           `json:"error,omitempty"`
	CommitSeq    int64               `json:"commit_seq,omitempty"`
	PayloadHash  string              `json:"payload_hash,omitempty"`
}

type CommitEntry struct {
	Seq         int64  `json:"seq"`
	Kind        string `json:"kind"`
	UnitKey     string `json:"unit_key"`
	PayloadHash string `json:"payload_hash"`
	PayloadJSON string `json:"payload_json,omitempty"`
}

type QualityStats struct {
	TileCount          int     `json:"tile_count"`
	CommittedTiles     int     `json:"committed_tiles"`
	DetectedLines      int     `json:"detected_lines"`
	CommittedLines     int     `json:"committed_lines"`
	EmptyLines         int     `json:"empty_lines"`
	CharCount          int     `json:"char_count"`
	AvgConfidence      float64 `json:"avg_confidence"`
	MinConfidence      float64 `json:"min_confidence"`
	LowConfidenceLines int     `json:"low_confidence_lines"`
	CandidateCount     int     `json:"candidate_count"`
	TruthRegionCount   int     `json:"truth_region_count"`
	CountDeviation     int     `json:"count_deviation"`
	AvgTruthIoU        float64 `json:"avg_truth_iou"`
	TextMatchLines     int     `json:"text_match_lines"`
	TextAccuracy       float64 `json:"text_accuracy"`
}

type RunView struct {
	RunID            string        `json:"run_id"`
	SampleID         string        `json:"sample_id"`
	SampleTitle      string        `json:"sample_title"`
	Status           string        `json:"status"`
	Mode             string        `json:"mode"`
	ImageWidth       int           `json:"image_width"`
	ImageHeight      int           `json:"image_height"`
	ImageHash        string        `json:"image_hash"`
	TemplateVersion  string        `json:"template_version"`
	ParamVersion     string        `json:"param_version"`
	AlgorithmVersion string        `json:"algorithm_version"`
	BandWidth        int           `json:"band_width"`
	AvgStrokeWidth   float64       `json:"avg_stroke_width"`
	Params           Params        `json:"params"`
	Tiles            []TileView    `json:"tiles"`
	Lines            []LineView    `json:"lines"`
	Stats            QualityStats  `json:"stats"`
	Commits          []CommitEntry `json:"commits"`
	TruthRegions     []TruthRegion `json:"truth_regions"`
	Invalidated      []string      `json:"invalidated,omitempty"`
	Error            *AppError     `json:"error,omitempty"`
	ProgressText     string        `json:"progress_text"`
}

type ComparisonPath struct {
	Name        string       `json:"name"`
	RunID       string       `json:"run_id"`
	Status      string       `json:"status"`
	Stats       QualityStats `json:"stats"`
	CommitCount int          `json:"commit_count"`
	DurationMS  int64        `json:"duration_ms"`
	Error       *AppError    `json:"error,omitempty"`
}

type ComparisonResult struct {
	Equal           bool             `json:"equal"`
	Code            string           `json:"code,omitempty"`
	Message         string           `json:"message"`
	Paths           []ComparisonPath `json:"paths"`
	FirstDiff       *AppError        `json:"first_diff,omitempty"`
	TilesEqual      bool             `json:"tiles_equal"`
	LinesEqual      bool             `json:"lines_equal"`
	CommitsEqual    bool             `json:"commits_equal"`
	ComparedLines   int              `json:"compared_lines"`
	ComparedCommits int              `json:"compared_commits"`
}

type Bootstrap struct {
	Manifest         Manifest `json:"manifest"`
	Defaults         Params   `json:"defaults"`
	AlgorithmVersion string   `json:"algorithm_version"`
	TemplateVersion  string   `json:"template_version"`
	DBPath           string   `json:"db_path"`
	SampleDirectory  string   `json:"sample_directory"`
	Desktop          bool     `json:"desktop"`
	LatestRun        *RunView `json:"latest_run,omitempty"`
}

func SortCandidates(candidates []Candidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.BBox.Y != b.BBox.Y {
			return a.BBox.Y < b.BBox.Y
		}
		if a.BBox.X != b.BBox.X {
			return a.BBox.X < b.BBox.X
		}
		if a.BBox.H != b.BBox.H {
			return a.BBox.H < b.BBox.H
		}
		return a.BBox.W < b.BBox.W
	})
}

func Clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func Round4(v float64) float64 {
	return math.Round(v*10000) / 10000
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
