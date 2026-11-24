package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"gameintegrity/internal/apperr"
)

const (
	AlgorithmVersion = "content-integrity/1.0.0"
	FormatVersion    = 1
)

type Interval struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

func (i Interval) Empty() bool { return i.Start < 0 || i.End < i.Start }
func (i Interval) Len() int {
	if i.Empty() {
		return 0
	}
	return i.End - i.Start + 1
}

type BlockRef struct {
	Col int `json:"col"`
	Row int `json:"row"`
	X   int `json:"x"`
	Y   int `json:"y"`
	W   int `json:"w"`
	H   int `json:"h"`
}

func (b BlockRef) AppErr() apperr.Block {
	return apperr.Block{Col: b.Col, Row: b.Row, X: b.X, Y: b.Y, W: b.W, H: b.H}
}

func AllBlocks(width, height, blockSize int) []BlockRef {
	if blockSize <= 0 {
		return nil
	}
	cols := width / blockSize
	rows := height / blockSize
	out := make([]BlockRef, 0, cols*rows)
	for row := 0; row < rows; row++ {
		for col := 0; col < cols; col++ {
			out = append(out, BlockRef{
				Col: col, Row: row,
				X: col * blockSize, Y: row * blockSize,
				W: blockSize, H: blockSize,
			})
		}
	}
	return out
}

func BlockAtPixel(x, y, width, height, blockSize int) BlockRef {
	if blockSize <= 0 {
		return BlockRef{}
	}
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if x >= width {
		x = width - 1
	}
	if y >= height {
		y = height - 1
	}
	col, row := x/blockSize, y/blockSize
	return BlockRef{
		Col: col, Row: row,
		X: col * blockSize, Y: row * blockSize,
		W: blockSize, H: blockSize,
	}
}

type Config struct {
	BlockSize           int     `json:"block_size"`
	ResidualThreshold   float64 `json:"residual_threshold"`
	NoiseThreshold      float64 `json:"noise_threshold"`
	BBoxShiftThreshold  float64 `json:"bbox_shift_threshold"`
	HashJumpRatio       float64 `json:"hash_jump_ratio"`
	History             int     `json:"history"`
	SamplingBudget      int     `json:"sampling_budget"`
	SegmentSeconds      float64 `json:"segment_seconds"`
	MaxWorkers          int     `json:"max_workers"`
	MaxPixelBufferBytes int64   `json:"max_pixel_buffer_bytes"`
}

func DefaultConfig(width, height, channels, frameCount int, blockSize, budget int) Config {
	if blockSize <= 0 {
		blockSize = 8
	}
	if budget <= 0 {
		budget = 24
	}
	return Config{
		BlockSize:           blockSize,
		ResidualThreshold:   0.03,
		NoiseThreshold:      0.03,
		BBoxShiftThreshold:  2,
		HashJumpRatio:       0.5,
		History:             3,
		SamplingBudget:      budget,
		SegmentSeconds:      1,
		MaxWorkers:          3,
		MaxPixelBufferBytes: int64(64 << 20),
	}
}

func (c Config) Validate(width, height, channels, frameCount int) error {
	if width <= 0 {
		return fieldErr(apperr.Meta, "width", "manifest width must be positive", ">0", width)
	}
	if height <= 0 {
		return fieldErr(apperr.Meta, "height", "manifest height must be positive", ">0", height)
	}
	if channels <= 0 {
		return fieldErr(apperr.Meta, "channels", "manifest channels must be positive", ">0", channels)
	}
	if frameCount <= 0 {
		return fieldErr(apperr.Meta, "frame_count", "manifest frame count must be positive", ">0", frameCount)
	}
	if c.BlockSize <= 0 || width%c.BlockSize != 0 || height%c.BlockSize != 0 {
		divisors := commonDivisors(width, height)
		lo, hi := 0, 0
		if len(divisors) > 0 {
			lo, hi = divisors[0], divisors[len(divisors)-1]
		}
		e := apperr.New(apperr.Para, "block size must be positive and divide both frame dimensions")
		e.Field = "block_size"
		e.Expected = "divisor of width and height"
		e.Actual = c.BlockSize
		e.AvailableRange = fmt.Sprintf("%d..%d; valid divisors: %v", lo, hi, divisors)
		return e
	}
	if !unitRange(c.ResidualThreshold) {
		return rangeErr("residual_threshold", 0, 1, c.ResidualThreshold)
	}
	if !unitRange(c.NoiseThreshold) {
		return rangeErr("noise_threshold", 0, 1, c.NoiseThreshold)
	}
	if c.HashJumpRatio <= 0 || c.HashJumpRatio > 1 {
		return rangeErr("hash_jump_ratio", math.SmallestNonzeroFloat64, 1, c.HashJumpRatio)
	}
	if c.BBoxShiftThreshold < 0 {
		return rangeErr("bbox_shift_threshold", 0, math.MaxFloat64, c.BBoxShiftThreshold)
	}
	if c.History < 1 || c.History > 64 {
		return rangeErr("history", 1, 64, c.History)
	}
	if c.SamplingBudget < 1 {
		return rangeErr("sampling_budget", 1, math.MaxInt32, c.SamplingBudget)
	}
	if c.SegmentSeconds <= 0 {
		return rangeErr("segment_seconds", math.SmallestNonzeroFloat64, math.MaxFloat64, c.SegmentSeconds)
	}
	if c.MaxWorkers < 1 || c.MaxWorkers > 64 {
		return rangeErr("max_workers", 1, 64, c.MaxWorkers)
	}
	frameBytes := int64(width) * int64(height) * int64(channels)
	workingSet := frameBytes * int64(2*c.History+1)
	if c.MaxPixelBufferBytes < workingSet {
		e := rangeErr("max_pixel_buffer_bytes", workingSet, int64(math.MaxInt64), c.MaxPixelBufferBytes)
		e.AvailableRange = fmt.Sprintf("%d..%d (one frame plus %d history/next buffers)", workingSet, int64(math.MaxInt64), 2*c.History)
		return e
	}
	return nil
}

func rangeErr(field string, min, max any, actual any) *apperr.Error {
	e := apperr.New(apperr.Para, "configuration value is outside the supported range")
	e.Field = field
	e.Expected = fmt.Sprintf("[%v,%v]", min, max)
	e.Actual = actual
	if s, ok := min.(string); ok {
		e.AvailableRange = s
	} else {
		e.AvailableRange = fmt.Sprintf("%v..%v", min, max)
	}
	return e
}

func fieldErr(code, field, message, expected string, actual any) *apperr.Error {
	e := apperr.New(code, message)
	e.Field = field
	e.Expected = expected
	e.Actual = actual
	return e
}

func unitRange(v float64) bool { return v >= 0 && v <= 1 }

func commonDivisors(a, b int) []int {
	max := a
	if b < max {
		max = b
	}
	out := make([]int, 0, 8)
	for d := 1; d <= max; d++ {
		if a%d == 0 && b%d == 0 {
			out = append(out, d)
		}
	}
	return out
}

func (c Config) ParamVersion() string {
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type Injection struct {
	Type  string   `json:"type"`
	Frame int      `json:"frame"`
	Block BlockRef `json:"block"`
}

type Manifest struct {
	FormatVersion  int         `json:"format_version"`
	SampleID       string      `json:"sample_id"`
	Seed           int64       `json:"seed"`
	Width          int         `json:"width"`
	Height         int         `json:"height"`
	Channels       int         `json:"channels"`
	FrameCount     int         `json:"frame_count"`
	FrameDuration  int64       `json:"frame_duration_us"`
	TimestampsUS   []int64     `json:"timestamps_us"`
	SegmentSeconds float64     `json:"segment_seconds"`
	BlockSize      int         `json:"block_size"`
	SamplingBudget int         `json:"sampling_budget"`
	Tolerance      int         `json:"tolerance_frames"`
	FramePattern   string      `json:"frame_pattern"`
	BaselineDir    string      `json:"baseline_dir"`
	Injections     []Injection `json:"injections,omitempty"`
}

type ShapeBox struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	X     int    `json:"x"`
	Y     int    `json:"y"`
	W     int    `json:"w"`
	H     int    `json:"h"`
	Color [3]int `json:"color"`
}

type FrameShapes struct {
	Frame int        `json:"frame"`
	Boxes []ShapeBox `json:"boxes"`
}

type Watermark struct {
	Frame  int `json:"frame"`
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
	Value  int `json:"value"`
}

type Truth struct {
	Width         int           `json:"width"`
	Height        int           `json:"height"`
	Channels      int           `json:"channels"`
	FrameCount    int           `json:"frame_count"`
	Watermarks    []Watermark   `json:"watermarks"`
	Shapes        []FrameShapes `json:"shapes"`
	Replacements  []Interval    `json:"replacements"`
	Tolerance     int           `json:"tolerance_frames"`
	Baseline      []string      `json:"baseline_frames"`
	BaselineBytes int64         `json:"baseline_frame_bytes"`
}

type BlockMetric struct {
	Col           int     `json:"col"`
	Row           int     `json:"row"`
	X             int     `json:"x"`
	Y             int     `json:"y"`
	W             int     `json:"w"`
	H             int     `json:"h"`
	Hash          string  `json:"hash"`
	Residual      float64 `json:"residual"`
	NoiseVariance float64 `json:"noise_variance"`
	Changed       bool    `json:"changed"`
}

type FrameResult struct {
	Frame              int           `json:"frame"`
	TimestampUS        int64         `json:"timestamp_us"`
	Summary            string        `json:"summary"`
	ContentHash        string        `json:"content_hash"`
	WatermarkOK        bool          `json:"watermark_ok"`
	WatermarkDecoded   int           `json:"watermark_decoded"`
	SummaryJump        bool          `json:"summary_jump"`
	ChangedBlockCount  int           `json:"changed_block_count"`
	ChangedBlocks      []BlockRef    `json:"changed_blocks,omitempty"`
	AbnormalBlocks     []BlockRef    `json:"abnormal_blocks,omitempty"`
	Blocks             []BlockMetric `json:"blocks,omitempty"`
	ResidualEnergy     float64       `json:"residual_energy"`
	ResidualEnergyNext float64       `json:"residual_energy_next"`
	ResidualScore      float64       `json:"residual_score"`
	NoiseMean          float64       `json:"noise_mean"`
	NoiseDispersion    float64       `json:"noise_dispersion"`
	BBoxShiftPixels    float64       `json:"bbox_shift_pixels"`
	Verdict            string        `json:"verdict"`
	Confidence         float64       `json:"confidence"`
	Candidate          bool          `json:"candidate"`
	Screened           bool          `json:"screened"`
	Refined            bool          `json:"refined"`
	Reasons            []string      `json:"reasons,omitempty"`
}

const (
	VerdictIntact   = "intact"
	VerdictSuspect  = "suspect"
	VerdictReplaced = "replaced"
	VerdictInvalid  = "invalid"
)

type ScaleDecision struct {
	Mode             string     `json:"mode"`
	FrameCount       int        `json:"frame_count"`
	FramePixels      int        `json:"frame_pixels"`
	TotalPixels      int64      `json:"total_pixels"`
	FrameUnits       int        `json:"frame_units"`
	EffectiveFrames  int        `json:"effective_frames"`
	SamplingBudget   int        `json:"sampling_budget"`
	Stride           int        `json:"stride"`
	BlockDownsample  int        `json:"block_downsample"`
	ScreenFrameCount int        `json:"screen_frame_count"`
	SkippedRanges    []Interval `json:"skipped_ranges"`
	Basis            string     `json:"basis"`
	ScreenedFrames   []int      `json:"screened_frames,omitempty"`
	CandidateFrames  []int      `json:"candidate_frames,omitempty"`
	RefinedFrames    []int      `json:"refined_frames,omitempty"`
	PoolWorkers      int        `json:"pool_workers"`
	PixelBufferBytes int64      `json:"pixel_buffer_bytes"`
	PixelBufferSlots int        `json:"pixel_buffer_slots"`
	PixelBufferWaits int64      `json:"pixel_buffer_waits"`
	ExactRefinement  bool       `json:"exact_refinement"`
}

type ReplacementResult struct {
	Detected        Interval `json:"detected"`
	Truth           Interval `json:"truth"`
	StartDelta      int      `json:"start_delta"`
	EndDelta        int      `json:"end_delta"`
	WithinTolerance bool     `json:"within_tolerance"`
	ToleranceFrames int      `json:"tolerance_frames"`
}

type SegmentResult struct {
	Index      int           `json:"index"`
	StartFrame int           `json:"start_frame"`
	EndFrame   int           `json:"end_frame"`
	State      string        `json:"state"`
	Frames     []FrameResult `json:"frames"`
	Summary    string        `json:"summary"`
}

type SegmentState struct {
	Index      int    `json:"index"`
	StartFrame int    `json:"start_frame"`
	EndFrame   int    `json:"end_frame"`
	State      string `json:"state"`
}

type Counts struct {
	Total    int `json:"total"`
	Intact   int `json:"intact"`
	Suspect  int `json:"suspect"`
	Replaced int `json:"replaced"`
	Invalid  int `json:"invalid"`
}

type Invalidation struct {
	Reasons  []string          `json:"reasons"`
	Fields   []string          `json:"fields"`
	Expected map[string]string `json:"expected"`
	Actual   map[string]string `json:"actual"`
}

type RunReport struct {
	SampleID          string            `json:"sample_id"`
	Path              string            `json:"path"`
	Mode              string            `json:"mode"`
	AlgorithmVersion  string            `json:"algorithm_version"`
	ParamVersion      string            `json:"param_version"`
	ContentHash       string            `json:"content_hash"`
	Config            Config            `json:"config"`
	Scale             ScaleDecision     `json:"scale"`
	Frames            []FrameResult     `json:"frames"`
	Segments          []SegmentResult   `json:"segments"`
	SegmentStates     []SegmentState    `json:"segment_states"`
	CommittedSegments int               `json:"committed_segments"`
	TotalSegments     int               `json:"total_segments"`
	Replacement       ReplacementResult `json:"replacement"`
	Counts            Counts            `json:"counts"`
	Invalidation      *Invalidation     `json:"checkpoint_invalidation,omitempty"`
	DurationMS        int64             `json:"duration_ms"`
	Paused            bool              `json:"paused,omitempty"`
	Error             *apperr.Error     `json:"error,omitempty"`
}

func (r *RunReport) Frame(i int) (*FrameResult, bool) {
	if i < 0 || i >= len(r.Frames) {
		return nil, false
	}
	return &r.Frames[i], true
}

func EmptyInterval() Interval { return Interval{Start: -1, End: -1} }

func NormalizeInterval(start, end int) Interval {
	if start < 0 || end < start {
		return EmptyInterval()
	}
	return Interval{Start: start, End: end}
}

func DetectReplacement(frames []FrameResult) Interval {
	start, end := -1, -1
	for i := range frames {
		if frames[i].Verdict == VerdictReplaced || frames[i].Verdict == VerdictInvalid {
			if start < 0 {
				start = i
			}
			end = i
		}
	}
	return NormalizeInterval(start, end)
}

func TruthInterval(truth *Truth) Interval {
	if truth == nil || len(truth.Replacements) == 0 {
		return EmptyInterval()
	}
	start, end := truth.Replacements[0].Start, truth.Replacements[0].End
	for _, r := range truth.Replacements[1:] {
		if r.Start < start {
			start = r.Start
		}
		if r.End > end {
			end = r.End
		}
	}
	return NormalizeInterval(start, end)
}

func IntervalDelta(a, b Interval) (int, int) {
	if a.Empty() && b.Empty() {
		return 0, 0
	}
	if a.Empty() {
		return 1 << 20, 1 << 20
	}
	if b.Empty() {
		return 1 << 20, 1 << 20
	}
	return abs(a.Start - b.Start), abs(a.End - b.End)
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func CountVerdicts(frames []FrameResult) Counts {
	c := Counts{Total: len(frames)}
	for i := range frames {
		switch frames[i].Verdict {
		case VerdictIntact:
			c.Intact++
		case VerdictSuspect:
			c.Suspect++
		case VerdictReplaced:
			c.Replaced++
		default:
			c.Invalid++
		}
	}
	return c
}

func SortedInts(in []int) []int {
	out := append([]int(nil), in...)
	sort.Ints(out)
	return out
}

func UniqueInts(in []int) []int {
	if len(in) == 0 {
		return nil
	}
	sorted := SortedInts(in)
	out := sorted[:1]
	for _, v := range sorted[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}

func RangesFromIndices(indices []int, total int, inverted bool) []Interval {
	inSet := make(map[int]bool, len(indices))
	for _, i := range indices {
		inSet[i] = true
	}
	var out []Interval
	start := -1
	for i := 0; i < total; i++ {
		selected := inSet[i]
		if inverted {
			selected = !selected
		}
		if selected && start < 0 {
			start = i
		}
		if !selected && start >= 0 {
			out = append(out, Interval{Start: start, End: i - 1})
			start = -1
		}
	}
	if start >= 0 {
		out = append(out, Interval{Start: start, End: total - 1})
	}
	return out
}

func VersionSummary(content, params, algorithm string) string {
	return strings.Join([]string{content, params, algorithm}, "/")
}
