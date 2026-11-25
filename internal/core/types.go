package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

const (
	GeneratorVersion  = "deterministic-icons@1.0.0"
	AlgorithmVersion  = "seeded-region-grow@1.0.0"
	CheckpointVersion = "sqlite-checkpoint@1.0.0"

	MinColorTolerance     = 0
	MaxColorTolerance     = 255
	MinGradientThreshold  = 0
	MaxGradientThreshold  = 255
	MinKernelSize         = 3
	MaxKernelSize         = 15
	MinGrowthIterations   = 1
	MaxGrowthIterations   = 100000
	MinQuantizationBits   = 4
	MaxQuantizationBits   = 8
	DefaultPreviewDelayMS = 60
)

type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type PointF struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type BBox struct {
	X0 int `json:"x0"`
	Y0 int `json:"y0"`
	X1 int `json:"x1"`
	Y1 int `json:"y1"`
}

func (b BBox) Width() int  { return b.X1 - b.X0 + 1 }
func (b BBox) Height() int { return b.Y1 - b.Y0 + 1 }

type RGB struct {
	R uint8 `json:"r"`
	G uint8 `json:"g"`
	B uint8 `json:"b"`
}

func (c RGB) Distance(other RGB) int {
	dr := int(c.R) - int(other.R)
	dg := int(c.G) - int(other.G)
	db := int(c.B) - int(other.B)
	return int(math.Sqrt(float64(dr*dr + dg*dg + db*db)))
}

type Image struct {
	ID       string `json:"id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Channels int    `json:"channels"`
	// Pix stores 8-bit channels in row-major order. Generated and loaded
	// samples are normalized to RGBA, so At remains independent of PNG subtypes.
	Pix []uint8 `json:"-"`
}

func (im *Image) Validate() error {
	if im == nil {
		return NewError(CodeDecode, "image.validate", "nil image")
	}
	if im.Width <= 0 || im.Height <= 0 {
		return NewError(CodeDecode, "image.validate", "non-positive image dimensions")
	}
	if im.Channels != 4 {
		return NewError(CodeDecode, "image.validate", fmt.Sprintf("unsupported channel count %d", im.Channels))
	}
	if len(im.Pix) != im.Width*im.Height*im.Channels {
		return NewError(CodeDecode, "image.validate", "pixel buffer length does not match dimensions")
	}
	return nil
}

func (im *Image) At(x, y int) RGB {
	i := (y*im.Width + x) * im.Channels
	return RGB{R: im.Pix[i], G: im.Pix[i+1], B: im.Pix[i+2]}
}

func (im *Image) Clone() *Image {
	cp := *im
	cp.Pix = append([]uint8(nil), im.Pix...)
	return &cp
}

// Params are user-editable segmentation parameters. Runtime reductions used
// by preview never mutate this struct; they are recorded separately.
type Params struct {
	ColorTolerance    int `json:"color_tolerance"`
	GradientThreshold int `json:"gradient_threshold"`
	KernelSize        int `json:"kernel_size"`
	GrowthIterations  int `json:"growth_iterations"`
}

func DefaultParams() Params {
	return Params{
		ColorTolerance:    30,
		GradientThreshold: 18,
		KernelSize:        3,
		GrowthIterations:  2048,
	}
}

func (p Params) Validate() error {
	if p.ColorTolerance < MinColorTolerance || p.ColorTolerance > MaxColorTolerance {
		return ParamError("params.validate", "color_tolerance", p.ColorTolerance, MinColorTolerance, MaxColorTolerance)
	}
	if p.GradientThreshold < MinGradientThreshold || p.GradientThreshold > MaxGradientThreshold {
		return ParamError("params.validate", "gradient_threshold", p.GradientThreshold, MinGradientThreshold, MaxGradientThreshold)
	}
	if p.KernelSize < MinKernelSize || p.KernelSize > MaxKernelSize {
		return ParamError("params.validate", "kernel_size", p.KernelSize, MinKernelSize, MaxKernelSize)
	}
	if p.KernelSize%2 == 0 {
		return &Error{
			Code:    CodeParam,
			Op:      "params.validate",
			Field:   "kernel_size",
			Got:     p.KernelSize,
			Want:    "odd integer in [3,15]",
			Message: "structuring element must have an odd size",
			Data:    map[string]any{"min": MinKernelSize, "max": MaxKernelSize, "odd": true},
		}
	}
	if p.GrowthIterations < MinGrowthIterations || p.GrowthIterations > MaxGrowthIterations {
		return ParamError("params.validate", "growth_iterations", p.GrowthIterations, MinGrowthIterations, MaxGrowthIterations)
	}
	return nil
}

type QuantizationRule struct {
	ChannelBits int `json:"channel_bits"`
}

func DefaultQuantization() QuantizationRule {
	return QuantizationRule{ChannelBits: 6}
}

func (q QuantizationRule) Validate() error {
	if q.ChannelBits < MinQuantizationBits || q.ChannelBits > MaxQuantizationBits {
		return ParamError("quantization.validate", "quantization.channel_bits", q.ChannelBits, MinQuantizationBits, MaxQuantizationBits)
	}
	return nil
}

type PreviewTolerance struct {
	CentroidPx   float64 `json:"centroid_px"`
	AreaRelative float64 `json:"area_relative"`
}

type TruthRegion struct {
	ID       int    `json:"id"`
	Label    int    `json:"label"`
	Color    RGB    `json:"color"`
	Area     int    `json:"area"`
	Centroid PointF `json:"centroid"`
	BBox     BBox   `json:"bbox"`
}

type SampleSpec struct {
	ID                string           `json:"id"`
	ImageFile         string           `json:"image_file"`
	ReferenceMaskFile string           `json:"reference_mask_file"`
	Width             int              `json:"width"`
	Height            int              `json:"height"`
	Channels          int              `json:"channels"`
	ImageSHA256       string           `json:"image_sha256"`
	TruthFingerprint  string           `json:"truth_fingerprint"`
	MaskHash          string           `json:"mask_hash"`
	RegionCount       int              `json:"region_count"`
	Regions           []TruthRegion    `json:"regions"`
	Seeds             []Point          `json:"seeds"`
	DefaultParams     Params           `json:"default_params"`
	Quantization      QuantizationRule `json:"quantization"`
	PreviewDelayMS    int              `json:"preview_delay_ms"`
	PreviewTolerance  PreviewTolerance `json:"preview_tolerance"`
}

type Manifest struct {
	Schema           string       `json:"schema"`
	GeneratorVersion string       `json:"generator_version"`
	Seed             int64        `json:"seed"`
	Samples          []SampleSpec `json:"samples"`
}

type Snapshot struct {
	Version          string           `json:"version"`
	ParamVersion     string           `json:"param_version"`
	ImageID          string           `json:"image_id"`
	ImageHash        string           `json:"image_hash"`
	Params           Params           `json:"params"`
	Seeds            []Point          `json:"seeds"`
	Quantization     QuantizationRule `json:"quantization"`
	AlgorithmVersion string           `json:"algorithm_version"`
}

type snapshotPayload struct {
	ImageID          string           `json:"image_id"`
	ImageHash        string           `json:"image_hash"`
	Params           Params           `json:"params"`
	Seeds            []Point          `json:"seeds"`
	Quantization     QuantizationRule `json:"quantization"`
	AlgorithmVersion string           `json:"algorithm_version"`
}

func NewSnapshot(imageID, imageHash string, params Params, seeds []Point, quant QuantizationRule) (Snapshot, error) {
	if err := params.Validate(); err != nil {
		return Snapshot{}, err
	}
	if err := quant.Validate(); err != nil {
		return Snapshot{}, err
	}
	if len(seeds) == 0 {
		return Snapshot{}, ParamError("snapshot.new", "seeds", len(seeds), 1, "unbounded")
	}
	canonicalSeeds := append([]Point(nil), seeds...)
	sort.Slice(canonicalSeeds, func(i, j int) bool {
		if canonicalSeeds[i].Y != canonicalSeeds[j].Y {
			return canonicalSeeds[i].Y < canonicalSeeds[j].Y
		}
		if canonicalSeeds[i].X != canonicalSeeds[j].X {
			return canonicalSeeds[i].X < canonicalSeeds[j].X
		}
		return false
	})
	payload := snapshotPayload{
		ImageID:          imageID,
		ImageHash:        imageHash,
		Params:           params,
		Seeds:            canonicalSeeds,
		Quantization:     quant,
		AlgorithmVersion: AlgorithmVersion,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return Snapshot{}, fmt.Errorf("marshal snapshot: %w", err)
	}
	sum := sha256.Sum256(raw)
	version := hex.EncodeToString(sum[:])
	return Snapshot{
		Version:          version,
		ParamVersion:     version,
		ImageID:          imageID,
		ImageHash:        imageHash,
		Params:           params,
		Seeds:            canonicalSeeds,
		Quantization:     quant,
		AlgorithmVersion: AlgorithmVersion,
	}, nil
}

func (s Snapshot) Clone() Snapshot {
	s.Seeds = append([]Point(nil), s.Seeds...)
	return s
}

func (s Snapshot) Validate() error {
	if s.Version == "" || s.ParamVersion == "" {
		return NewError(CodeVersion, "snapshot.validate", "empty snapshot version")
	}
	if s.AlgorithmVersion != AlgorithmVersion {
		return &Error{
			Code:    CodeVersion,
			Op:      "snapshot.validate",
			Left:    s.AlgorithmVersion,
			Right:   AlgorithmVersion,
			Message: "algorithm version mismatch",
		}
	}
	if err := s.Params.Validate(); err != nil {
		return err
	}
	return s.Quantization.Validate()
}

func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
