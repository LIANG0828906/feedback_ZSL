package engine

import (
	"fmt"
	"math"
	"sort"

	"ms-workbench/internal/pipeline"
	"ms-workbench/internal/scene"
)

type Config struct {
	ChunkWidth         int
	ChunkHeight        int
	MaxWorkers         int
	MaxPixelsPerWorker int
	AlgorithmVersion   string
	ChunkDelayMS       int
}

func DefaultConfig() Config {
	return Config{
		ChunkWidth:         16,
		ChunkHeight:        16,
		MaxWorkers:         4,
		MaxPixelsPerWorker: 1024,
		AlgorithmVersion:   "spectra-pipeline/1.0.0",
	}
}

func (c Config) Validate() error {
	if c.ChunkWidth <= 0 || c.ChunkHeight <= 0 {
		return fmt.Errorf("chunk dimensions must be positive")
	}
	if c.MaxWorkers <= 0 {
		return fmt.Errorf("max workers must be positive")
	}
	if c.MaxPixelsPerWorker < c.ChunkWidth*c.ChunkHeight {
		return fmt.Errorf("pixel buffer %d is smaller than one chunk (%d)", c.MaxPixelsPerWorker, c.ChunkWidth*c.ChunkHeight)
	}
	if c.AlgorithmVersion == "" {
		return fmt.Errorf("algorithm version is required")
	}
	if c.ChunkDelayMS < 0 {
		return fmt.Errorf("chunk delay cannot be negative")
	}
	return nil
}

type Segment struct {
	ID          int
	Rect        scene.Rect
	State       SegmentState
	Result      *pipeline.ChunkResult
	Ready       bool
	FusionDirty bool
	ChromaDirty bool
	Reasons     []string
	LastError   string
	Reused      bool
}

func (s *Segment) clone() *Segment {
	if s == nil {
		return nil
	}
	out := *s
	out.Reasons = append([]string(nil), s.Reasons...)
	out.Result = s.Result.Clone()
	return &out
}

type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type Composite struct {
	Width          int                      `json:"width"`
	Height         int                      `json:"height"`
	BandIDs        []string                 `json:"bandIDs"`
	Fused          []float64                `json:"-"`
	LinearRGB      []float64                `json:"-"`
	OutOfGamut     []Point                  `json:"outOfGamut"`
	GamutBoundary  []pipeline.BoundaryPoint `json:"gamutBoundary"`
	PeakRadiance   float64                  `json:"peakRadiance"`
	SegmentPeaks   []float64                `json:"segmentPeaks"`
	Committed      int                      `json:"committedSegments"`
	Total          int                      `json:"totalSegments"`
	Complete       bool                     `json:"complete"`
	CommitSequence []int                    `json:"commitSequence"`
}

type RunMode string

const (
	ModeLive   RunMode = "live"
	ModeResume RunMode = "resume"
	ModeFull   RunMode = "full"
)

type RunOptions struct {
	Mode          RunMode
	MaxNewCommits int
}

type RunReport struct {
	Mode           RunMode `json:"mode"`
	Complete       bool    `json:"complete"`
	NewCommits     int     `json:"newCommits"`
	Committed      int     `json:"committedSegments"`
	Total          int     `json:"totalSegments"`
	CommitSequence []int   `json:"commitSequence"`
	SnapshotReset  string  `json:"snapshotReset,omitempty"`
	Paused         bool    `json:"paused"`
}

type SegmentView struct {
	ID           int          `json:"id"`
	State        SegmentState `json:"state"`
	X            int          `json:"x"`
	Y            int          `json:"y"`
	W            int          `json:"w"`
	H            int          `json:"h"`
	Error        string       `json:"error,omitempty"`
	Reasons      []string     `json:"reasons,omitempty"`
	Reused       bool         `json:"reused"`
	OutOfGamut   int          `json:"outOfGamut"`
	PeakRadiance float64      `json:"peakRadiance"`
}

type View struct {
	SceneID          string               `json:"sceneID"`
	SceneName        string               `json:"sceneName"`
	SourceVersion    int                  `json:"sourceVersion"`
	Width            int                  `json:"width"`
	Height           int                  `json:"height"`
	BandIDs          []string             `json:"bandIDs"`
	Settings         scene.RenderSettings `json:"settings"`
	ParamVersion     string               `json:"paramVersion"`
	BandVersion      string               `json:"bandVersion"`
	AlgorithmVersion string               `json:"algorithmVersion"`
	Segments         []SegmentView        `json:"segments"`
	Committed        int                  `json:"committedSegments"`
	Total            int                  `json:"totalSegments"`
	Running          bool                 `json:"running"`
	Paused           bool                 `json:"paused"`
	SnapshotReset    string               `json:"snapshotReset,omitempty"`
	CommitSequence   []int                `json:"commitSequence"`
	LastInvalidation *InvalidationReport  `json:"lastInvalidation,omitempty"`
	Fingerprint      string               `json:"sceneFingerprint"`
}

type SegmentInvalidation struct {
	SegmentID int      `json:"segmentID"`
	Stage     string   `json:"stage"`
	Reasons   []string `json:"reasons"`
	Reused    bool     `json:"reused"`
}

type InvalidationReport struct {
	ChangedFields    []string              `json:"changedFields"`
	Segments         []SegmentInvalidation `json:"segments"`
	ParamVersion     string                `json:"paramVersion"`
	BandVersion      string                `json:"bandVersion"`
	SceneFingerprint string                `json:"sceneFingerprint"`
}

type Dependency struct {
	Parameter   string `json:"parameter"`
	Stage       string `json:"stage"`
	SegmentID   int    `json:"segmentID"`
	Description string `json:"description"`
}

type PixelDifference struct {
	X       int     `json:"x"`
	Y       int     `json:"y"`
	Segment int     `json:"segment"`
	Field   string  `json:"field"`
	Index   int     `json:"index"`
	A       float64 `json:"a"`
	B       float64 `json:"b"`
	Delta   float64 `json:"delta"`
}

type DifferenceReport struct {
	Equal               bool             `json:"equal"`
	PixelsCompared      int              `json:"pixelsCompared"`
	FirstDifference     *PixelDifference `json:"firstDifference,omitempty"`
	MaxAbsDelta         float64          `json:"maxAbsDelta"`
	PeakEqual           bool             `json:"peakEqual"`
	OutOfGamutEqual     bool             `json:"outOfGamutEqual"`
	CommitSequenceEqual bool             `json:"commitSequenceEqual"`
	Heatmap             []float64        `json:"heatmap,omitempty"`
}

func segmentIDForPixel(x, y, width, height int, cfg Config) int {
	cols := (width + cfg.ChunkWidth - 1) / cfg.ChunkWidth
	cx := x / cfg.ChunkWidth
	cy := y / cfg.ChunkHeight
	return cy*cols + cx
}

func sortInts(v []int) {
	sort.Ints(v)
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func samePoints(a, b []Point) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func CompareComposites(a, b *Composite, cfg Config) DifferenceReport {
	report := DifferenceReport{
		Equal:     true,
		PeakEqual: math.Float64bits(a.PeakRadiance) == math.Float64bits(b.PeakRadiance),
	}
	if a.Width != b.Width || a.Height != b.Height {
		report.FirstDifference = &PixelDifference{Field: "dimensions", A: float64(a.Width), B: float64(b.Width), Delta: math.Abs(float64(a.Width - b.Width))}
		return report
	}
	report.PixelsCompared = a.Width * a.Height
	report.Heatmap = make([]float64, a.Width*a.Height)
	bands := len(a.BandIDs)
	firstSet := false
	check := func(x, y int, field string, index int, av, bv float64) {
		if math.Float64bits(av) == math.Float64bits(bv) {
			return
		}
		report.Equal = false
		delta := math.Abs(av - bv)
		if math.IsNaN(delta) {
			delta = math.Inf(1)
		}
		heat := y*a.Width + x
		if delta > report.Heatmap[heat] {
			report.Heatmap[heat] = delta
		}
		if delta > report.MaxAbsDelta {
			report.MaxAbsDelta = delta
		}
		if !firstSet {
			firstSet = true
			report.FirstDifference = &PixelDifference{
				X: x, Y: y, Segment: segmentIDForPixel(x, y, a.Width, a.Height, cfg),
				Field: field, Index: index, A: av, B: bv, Delta: delta,
			}
		}
	}
	for y := 0; y < a.Height; y++ {
		for x := 0; x < a.Width; x++ {
			for band := 0; band < bands; band++ {
				idx := (y*a.Width+x)*bands + band
				check(x, y, "fused", band, a.Fused[idx], b.Fused[idx])
			}
			for c := 0; c < 3; c++ {
				idx := (y*a.Width+x)*3 + c
				check(x, y, "rgb", c, a.LinearRGB[idx], b.LinearRGB[idx])
			}
		}
	}
	report.OutOfGamutEqual = samePoints(a.OutOfGamut, b.OutOfGamut)
	if !report.OutOfGamutEqual {
		report.Equal = false
		if !firstSet {
			report.FirstDifference = &PixelDifference{Field: "outOfGamut", A: float64(len(a.OutOfGamut)), B: float64(len(b.OutOfGamut)), Delta: math.Abs(float64(len(a.OutOfGamut) - len(b.OutOfGamut)))}
		}
	}
	report.CommitSequenceEqual = sameInts(a.CommitSequence, b.CommitSequence)
	if !report.CommitSequenceEqual || !report.PeakEqual {
		report.Equal = false
	}
	if firstSet {
		report.Equal = false
	}
	return report
}
