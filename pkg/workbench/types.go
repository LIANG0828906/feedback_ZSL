package workbench

import (
	"fmt"
	"math"
	"strings"
)

const (
	StatePending    = "pending"
	StateProcessing = "processing"
	StateCommitted  = "committed"
	StateFailed     = "failed"

	PathAuto  = "auto"
	PathFull  = "full"
	PathTiled = "tiled"

	IntentCompress = "compress"
	IntentStrict   = "strict"

	AlgorithmVersion = "spectral-fusion-v1"
)

// PixelCoord uses image coordinates with the origin at the upper-left.
type PixelCoord struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type XY struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// WhitePoint is stored as both XYZ and a human-readable reference name. The
// validator checks that the name and tristimulus values are internally
// consistent with the truth manifest.
type WhitePoint struct {
	Name string  `json:"name"`
	X    float64 `json:"X"`
	Y    float64 `json:"Y"`
	Z    float64 `json:"Z"`
}

func D65() WhitePoint {
	return WhitePoint{Name: "D65", X: 0.95047, Y: 1.0, Z: 1.08883}
}

type BandMeta struct {
	Index            int     `json:"index"`
	CenterWavelength float64 `json:"centerWavelengthNm"`
	FWHM             float64 `json:"fwhmNm"`
}

type Metadata struct {
	Width                 int        `json:"width"`
	Height                int        `json:"height"`
	BitDepth              int        `json:"bitDepth"`
	BandCount             int        `json:"bandCount"`
	ExposureCount         int        `json:"exposureCount"`
	Bands                 []BandMeta `json:"bands"`
	ExposureTimes         []float64  `json:"exposureTimes"`
	PixelPitchMicrons     float64    `json:"pixelPitchMicrons"`
	WhitePoint            WhitePoint `json:"whitePoint"`
	SourceWhitePoint      WhitePoint `json:"sourceWhitePoint"`
	ResponseGamma         float64    `json:"responseGamma"`
	BandRadianceScale     float64    `json:"bandRadianceScale"`
	MaxRegistrationOffset float64    `json:"maxRegistrationOffsetPx"`
	TargetGamut           string     `json:"targetGamut"`
	DefaultGamutIntent    string     `json:"defaultGamutIntent"`
	DefaultDetailStrength float64    `json:"defaultDetailStrength"`
	BandMetadataVersion   string     `json:"bandMetadataVersion"`
}

type BandImage struct {
	Index            int      `json:"index"`
	CenterWavelength float64  `json:"centerWavelengthNm"`
	Width            int      `json:"width"`
	Height           int      `json:"height"`
	BitDepth         int      `json:"bitDepth"`
	Data             []uint16 `json:"data"`
}

type ExposureFrame struct {
	Index        int      `json:"index"`
	ExposureTime float64  `json:"exposureTime"`
	Width        int      `json:"width"`
	Height       int      `json:"height"`
	BitDepth     int      `json:"bitDepth"`
	Data         []uint16 `json:"data"`
}

type RegistrationOffset struct {
	FromBand int     `json:"fromBand"`
	ToBand   int     `json:"toBand"`
	DX       float64 `json:"dx"`
	DY       float64 `json:"dy"`
}

func (r RegistrationOffset) Name() string {
	return fmt.Sprintf("b%d->b%d", r.FromBand, r.ToBand)
}

type PatchTruth struct {
	ID        string     `json:"id"`
	X         int        `json:"x"`
	Y         int        `json:"y"`
	Width     int        `json:"width"`
	Height    int        `json:"height"`
	LinearRGB [3]float64 `json:"linearRGB"`
}

type Tolerance struct {
	PatchRGB        float64 `json:"patchRGB"`
	PeakRadianceRel float64 `json:"peakRadianceRel"`
	PathPixel       float64 `json:"pathPixel"`
	ScaleBlock      float64 `json:"scaleBlock"`
}

func DefaultTolerance() Tolerance {
	return Tolerance{
		PatchRGB:        0.035,
		PeakRadianceRel: 0.025,
		PathPixel:       1e-9,
		ScaleBlock:      1e-6,
	}
}

type Truth struct {
	Patches         []PatchTruth         `json:"patches"`
	PeakRadiance    float64              `json:"peakRadiance"`
	Registration    []RegistrationOffset `json:"registration"`
	WhitePoint      WhitePoint           `json:"whitePoint"`
	DetailStrength  float64              `json:"detailStrength"`
	Tolerance       Tolerance            `json:"tolerance"`
	GamutBoundaryXY []XY                 `json:"gamutBoundaryXY"`
}

type Scene struct {
	ID        string          `json:"id"`
	Seed      int64           `json:"seed"`
	Meta      Metadata        `json:"metadata"`
	Bands     []BandImage     `json:"bands"`
	Exposures []ExposureFrame `json:"exposures"`
	Detail    []float64       `json:"detail"`
	Truth     Truth           `json:"truth"`
}

func (s *Scene) PixelCount() int {
	if s == nil {
		return 0
	}
	return s.Meta.Width * s.Meta.Height
}

type GenerateConfig struct {
	Seed          int64     `json:"seed"`
	Width         int       `json:"width"`
	Height        int       `json:"height"`
	Bands         int       `json:"bands"`
	Exposures     int       `json:"exposures"`
	BitDepth      int       `json:"bitDepth"`
	BandCenters   []float64 `json:"bandCentersNm,omitempty"`
	ExposureTimes []float64 `json:"exposureTimes,omitempty"`
	TargetGamut   string    `json:"targetGamut,omitempty"`
}

func DefaultGenerateConfig() GenerateConfig {
	return GenerateConfig{
		Seed:          20261124,
		Width:         96,
		Height:        72,
		Bands:         3,
		Exposures:     3,
		BitDepth:      16,
		BandCenters:   []float64{450, 550, 650},
		ExposureTimes: []float64{1.0 / 250.0, 1.0 / 60.0, 1.0 / 15.0},
		TargetGamut:   "sRGB",
	}
}

type ProcessConfig struct {
	Path                  string  `json:"path"`
	InterpolationOrder    int     `json:"interpolationOrder"`
	MaxRegistrationOffset float64 `json:"maxRegistrationOffsetPx"`
	WeightBias            float64 `json:"weightBias"`
	WeightSharpness       float64 `json:"weightSharpness"`
	DetailStrength        float64 `json:"detailStrength"`
	GamutIntent           string  `json:"gamutIntent"`
	MaxWorkers            int     `json:"maxWorkers"`
	PixelBufferLimit      int     `json:"pixelBufferLimit"`
	MemoryBudgetMB        int     `json:"memoryBudgetMB"`
	TimeBudgetMS          int     `json:"timeBudgetMs"`
	BlockPixels           int     `json:"blockPixels"`
	DownsampleFactor      int     `json:"downsampleFactor"`
	DisplayExposure       float64 `json:"displayExposure"`
}

func DefaultProcessConfig() ProcessConfig {
	return ProcessConfig{
		Path:                  PathAuto,
		InterpolationOrder:    3,
		MaxRegistrationOffset: 8,
		WeightBias:            0,
		WeightSharpness:       1,
		DetailStrength:        0.25,
		GamutIntent:           IntentCompress,
		MaxWorkers:            4,
		PixelBufferLimit:      262144,
		MemoryBudgetMB:        256,
		TimeBudgetMS:          900,
		BlockPixels:           4096,
		DownsampleFactor:      1,
		DisplayExposure:       2.4,
	}
}

func (c ProcessConfig) normalized() ProcessConfig {
	d := DefaultProcessConfig()
	if c.Path == "" {
		c.Path = d.Path
	}
	if c.InterpolationOrder == 0 {
		c.InterpolationOrder = d.InterpolationOrder
	}
	if c.MaxRegistrationOffset == 0 {
		c.MaxRegistrationOffset = d.MaxRegistrationOffset
	}
	if c.WeightSharpness == 0 {
		c.WeightSharpness = d.WeightSharpness
	}
	if c.GamutIntent == "" {
		c.GamutIntent = d.GamutIntent
	}
	if c.MaxWorkers == 0 {
		c.MaxWorkers = d.MaxWorkers
	}
	if c.PixelBufferLimit == 0 {
		c.PixelBufferLimit = d.PixelBufferLimit
	}
	if c.MemoryBudgetMB == 0 {
		c.MemoryBudgetMB = d.MemoryBudgetMB
	}
	if c.TimeBudgetMS == 0 {
		c.TimeBudgetMS = d.TimeBudgetMS
	}
	if c.BlockPixels == 0 {
		c.BlockPixels = d.BlockPixels
	}
	if c.DownsampleFactor == 0 {
		c.DownsampleFactor = d.DownsampleFactor
	}
	if c.DisplayExposure == 0 {
		c.DisplayExposure = d.DisplayExposure
	}
	return c
}

func (c ProcessConfig) Validate() error {
	c = c.normalized()
	if !oneOf(c.Path, PathAuto, PathFull, PathTiled) {
		return fieldErr(CodePara, "path", c.Path, []string{PathAuto, PathFull, PathTiled}, "unsupported processing path")
	}
	if c.InterpolationOrder < 1 || c.InterpolationOrder > 5 {
		return fieldErr(CodePara, "interpolationOrder", c.InterpolationOrder, "1..5", "interpolation order out of range")
	}
	if c.MaxRegistrationOffset < 0.5 || c.MaxRegistrationOffset > 64 {
		return fieldErr(CodePara, "maxRegistrationOffsetPx", c.MaxRegistrationOffset, "0.5..64", "registration correction range out of range")
	}
	if c.WeightBias < -1 || c.WeightBias > 1 {
		return fieldErr(CodePara, "weightBias", c.WeightBias, "-1..1", "weight bias out of range")
	}
	if c.WeightSharpness < 0.25 || c.WeightSharpness > 8 {
		return fieldErr(CodePara, "weightSharpness", c.WeightSharpness, "0.25..8", "weight sharpness out of range")
	}
	if c.DetailStrength < 0 || c.DetailStrength > 2 {
		return fieldErr(CodePara, "detailStrength", c.DetailStrength, "0..2", "detail strength out of range")
	}
	if c.GamutIntent != IntentCompress && c.GamutIntent != IntentStrict {
		return fieldErr(CodePara, "gamutIntent", c.GamutIntent, []string{IntentCompress, IntentStrict}, "unsupported gamut intent")
	}
	if c.MaxWorkers < 1 || c.MaxWorkers > 16 {
		return fieldErr(CodePara, "maxWorkers", c.MaxWorkers, "1..16", "worker count out of range")
	}
	if c.PixelBufferLimit < 256 || c.PixelBufferLimit > 64*1024*1024 {
		return fieldErr(CodePara, "pixelBufferLimit", c.PixelBufferLimit, "256..67108864", "per-thread pixel buffer out of range")
	}
	if c.MemoryBudgetMB < 8 || c.MemoryBudgetMB > 65536 {
		return fieldErr(CodePara, "memoryBudgetMB", c.MemoryBudgetMB, "8..65536", "memory budget out of range")
	}
	if c.TimeBudgetMS < 1 || c.TimeBudgetMS > 3600000 {
		return fieldErr(CodePara, "timeBudgetMs", c.TimeBudgetMS, "1..3600000", "time budget out of range")
	}
	if c.BlockPixels < 256 || c.BlockPixels > 16*1024*1024 {
		return fieldErr(CodePara, "blockPixels", c.BlockPixels, "256..16777216", "block size out of range")
	}
	if c.DownsampleFactor < 1 || c.DownsampleFactor > 16 {
		return fieldErr(CodePara, "downsampleFactor", c.DownsampleFactor, "1..16", "downsample factor out of range")
	}
	if c.DisplayExposure < 0.1 || c.DisplayExposure > 20 {
		return fieldErr(CodePara, "displayExposure", c.DisplayExposure, "0.1..20", "display exposure out of range")
	}
	return nil
}

type ScaleDecision struct {
	RequestedPath       string   `json:"requestedPath"`
	SelectedPath        string   `json:"selectedPath"`
	IsTiled             bool     `json:"isTiled"`
	Rationale           string   `json:"rationale"`
	Reasons             []string `json:"reasons"`
	PixelCount          int      `json:"pixelCount"`
	EffectivePixels     int64    `json:"effectivePixels"`
	TileWidth           int      `json:"tileWidth"`
	TileHeight          int      `json:"tileHeight"`
	BlockPixels         int      `json:"blockPixels"`
	DownsampleFactor    int      `json:"downsampleFactor"`
	EstimatedMemoryByte int64    `json:"estimatedMemoryBytes"`
	MemoryBudgetByte    int64    `json:"memoryBudgetBytes"`
	EstimatedFullMS     float64  `json:"estimatedFullMs"`
	TimeBudgetMS        int      `json:"timeBudgetMs"`
	CoarseSamples       int      `json:"coarseSamples,omitempty"`
	CoarsePeakRadiance  float64  `json:"coarsePeakRadiance,omitempty"`
	CoarseMeanRadiance  float64  `json:"coarseMeanRadiance,omitempty"`
}

type GamutStats struct {
	OutOfGamutCount int          `json:"outOfGamutCount"`
	OutOfGamutHash  string       `json:"outOfGamutHash"`
	Samples         []PixelCoord `json:"samples"`
	BoundaryXY      []XY         `json:"boundaryXY"`
}

type RenderResult struct {
	SceneID          string        `json:"sceneId"`
	Width            int           `json:"width"`
	Height           int           `json:"height"`
	Path             string        `json:"path"`
	Scale            ScaleDecision `json:"scale"`
	FusedRadiance    []float64     `json:"-"`
	LinearRGB        []float64     `json:"-"`
	DisplayRGB       []uint8       `json:"-"`
	GamutMask        []uint8       `json:"-"`
	PeakRadiance     float64       `json:"peakRadiance"`
	MeanRadiance     float64       `json:"meanRadiance"`
	OutOfGamut       int           `json:"outOfGamut"`
	OutOfGamutHash   string        `json:"outOfGamutHash"`
	OutOfGamutSample []PixelCoord  `json:"outOfGamutSamples"`
	GamutBoundaryXY  []XY          `json:"gamutBoundaryXY"`
	ElapsedMS        float64       `json:"elapsedMs"`
}

type PathComparison struct {
	PathA              string             `json:"pathA"`
	PathB              string             `json:"pathB"`
	Consistent         bool               `json:"consistent"`
	FirstMismatch      *PixelCoord        `json:"firstMismatch,omitempty"`
	FirstMismatchBlock int                `json:"firstMismatchBlock"`
	PeakDiff           float64            `json:"peakDiff"`
	GamutSetMatch      bool               `json:"gamutSetMatch"`
	Metrics            map[string]float64 `json:"metrics"`
}

type ParamSnapshot struct {
	Version               string  `json:"version"`
	InterpolationOrder    int     `json:"interpolationOrder"`
	MaxRegistrationOffset float64 `json:"maxRegistrationOffsetPx"`
	WeightBias            float64 `json:"weightBias"`
	WeightSharpness       float64 `json:"weightSharpness"`
	DetailStrength        float64 `json:"detailStrength"`
	GamutIntent           string  `json:"gamutIntent"`
	DownsampleFactor      int     `json:"downsampleFactor"`
	DisplayExposure       float64 `json:"displayExposure"`
}

func snapshotParams(cfg ProcessConfig, version string) ParamSnapshot {
	cfg = cfg.normalized()
	return ParamSnapshot{
		Version:               version,
		InterpolationOrder:    cfg.InterpolationOrder,
		MaxRegistrationOffset: cfg.MaxRegistrationOffset,
		WeightBias:            cfg.WeightBias,
		WeightSharpness:       cfg.WeightSharpness,
		DetailStrength:        cfg.DetailStrength,
		GamutIntent:           cfg.GamutIntent,
		DownsampleFactor:      cfg.DownsampleFactor,
		DisplayExposure:       cfg.DisplayExposure,
	}
}

func (p ParamSnapshot) processConfig() ProcessConfig {
	c := DefaultProcessConfig()
	c.InterpolationOrder = p.InterpolationOrder
	c.MaxRegistrationOffset = p.MaxRegistrationOffset
	c.WeightBias = p.WeightBias
	c.WeightSharpness = p.WeightSharpness
	c.DetailStrength = p.DetailStrength
	c.GamutIntent = p.GamutIntent
	c.DownsampleFactor = p.DownsampleFactor
	c.DisplayExposure = p.DisplayExposure
	return c
}

func oneOf(v string, values ...string) bool {
	for _, candidate := range values {
		if v == candidate {
			return true
		}
	}
	return false
}

func maxCode(bitDepth int) uint32 {
	if bitDepth <= 0 {
		return 0
	}
	return uint32((uint64(1) << bitDepth) - 1)
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func finite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func normalizeGamutName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "srgb", "rec709", "rec.709":
		return "sRGB"
	case "display-p3", "displayp3", "p3":
		return "DisplayP3"
	case "rec2020", "rec.2020", "bt2020":
		return "Rec2020"
	default:
		return name
	}
}
