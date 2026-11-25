package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"math"
	"sort"
	"strconv"
)

const (
	AlgorithmVersion = "icon-seg/1.0.0"
	SchemaVersion    = "1"
)

type Rect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

type SeedKind string

const (
	SeedForeground SeedKind = "foreground"
	SeedBackground SeedKind = "background"
)

type Seed struct {
	X         int      `json:"x"`
	Y         int      `json:"y"`
	Kind      SeedKind `json:"kind"`
	Component int      `json:"component,omitempty"`
}

type ComponentTruth struct {
	ID          int     `json:"id"`
	CentroidX   float64 `json:"centroid_x"`
	CentroidY   float64 `json:"centroid_y"`
	Area        int     `json:"area"`
	BBox        Rect    `json:"bbox"`
	HoleCount   int     `json:"hole_count"`
	Fingerprint string  `json:"fingerprint,omitempty"`
}

type MaskComponent struct {
	ID         int     `json:"id"`
	CentroidX  float64 `json:"centroid_x"`
	CentroidY  float64 `json:"centroid_y"`
	Area       int     `json:"area"`
	BBox       Rect    `json:"bbox"`
	HoleCount  int     `json:"hole_count"`
	PixelCount int     `json:"pixel_count"`
}

type RegionRecord struct {
	ID        int32    `json:"id"`
	Kind      SeedKind `json:"kind"`
	Component int      `json:"component,omitempty"`
	Area      int      `json:"area"`
}

type TruthManifest struct {
	SchemaVersion             string           `json:"schema_version"`
	AlgorithmVersion          string           `json:"algorithm_version"`
	ImageID                   string           `json:"image_id"`
	File                      string           `json:"file"`
	MaskFile                  string           `json:"mask_file"`
	Seed                      int64            `json:"seed"`
	Width                     int              `json:"width"`
	Height                    int              `json:"height"`
	Channels                  int              `json:"channels"`
	BlockSize                 int              `json:"block_size"`
	WorkerBudget              int              `json:"worker_budget"`
	PerThreadPixelBuffer      int              `json:"per_thread_pixel_buffer"`
	PreviewLatencyMS          int              `json:"preview_latency_ms"`
	MaskMismatchTolerance     float64          `json:"mask_mismatch_tolerance"`
	HoleCountTolerance        int              `json:"hole_count_tolerance"`
	PreviewCentroidTolerance  float64          `json:"preview_centroid_tolerance"`
	PreviewAreaRelTolerance   float64          `json:"preview_area_rel_tolerance"`
	PreviewDiffPixelTolerance float64          `json:"preview_diff_pixel_tolerance"`
	Regions                   []ComponentTruth `json:"regions"`
	Seeds                     []Seed           `json:"seeds"`
	ReferenceMask             []byte           `json:"reference_mask"`
	ReferenceMaskHash         string           `json:"reference_mask_hash"`
	ReferenceFingerprint      string           `json:"reference_fingerprint"`
}

type Icon struct {
	Manifest TruthManifest
	Image    *image.NRGBA
}

type SegmentParams struct {
	GradientOperator string  `json:"gradient_operator"`
	SmoothKernel     int     `json:"smooth_kernel"`
	FloodThreshold   float64 `json:"flood_threshold"`
	MinRegionSize    int     `json:"min_region_size"`
	MergeCostLimit   float64 `json:"merge_cost_limit"`
	MergeAreaDiff    int     `json:"merge_area_diff_limit"`
	CleanupRadius    int     `json:"cleanup_radius"`
}

func DefaultParams() SegmentParams {
	return SegmentParams{
		GradientOperator: "sobel",
		SmoothKernel:     3,
		FloodThreshold:   140,
		MinRegionSize:    12,
		MergeCostLimit:   64,
		MergeAreaDiff:    5000,
		CleanupRadius:    1,
	}
}

func (p SegmentParams) Validate() error {
	switch p.GradientOperator {
	case "sobel", "prewitt":
	default:
		return ParaError("gradient_operator", "{sobel,prewitt}", "unsupported gradient operator")
	}
	switch p.SmoothKernel {
	case 1, 3, 5:
	default:
		return ParaError("smooth_kernel", "{1,3,5}", "smooth kernel must be an odd Gaussian radius")
	}
	if math.IsNaN(p.FloodThreshold) || math.IsInf(p.FloodThreshold, 0) ||
		p.FloodThreshold < 0 || p.FloodThreshold > 2048 {
		return ParaError("flood_threshold", "[0,2048]", "flood threshold is outside the legal interval")
	}
	if p.MinRegionSize < 1 || p.MinRegionSize > 1_000_000 {
		return ParaError("min_region_size", "[1,1000000]", "minimum region size is outside the legal interval")
	}
	if math.IsNaN(p.MergeCostLimit) || math.IsInf(p.MergeCostLimit, 0) ||
		p.MergeCostLimit < 0 || p.MergeCostLimit > 4096 {
		return ParaError("merge_cost_limit", "[0,4096]", "merge cost limit is outside the legal interval")
	}
	if p.MergeAreaDiff < 0 || p.MergeAreaDiff > 100_000_000 {
		return ParaError("merge_area_diff_limit", "[0,100000000]", "merge area difference limit is outside the legal interval")
	}
	if p.CleanupRadius < 0 || p.CleanupRadius > 8 {
		return ParaError("cleanup_radius", "[0,8]", "cleanup radius is outside the legal interval")
	}
	return nil
}

type Snapshot struct {
	ParamVersion     string        `json:"param_version"`
	AlgorithmVersion string        `json:"algorithm_version"`
	Seed             int64         `json:"seed"`
	ImageID          string        `json:"image_id"`
	Params           SegmentParams `json:"params"`
}

func NewSnapshot(seed int64, imageID string, params SegmentParams) (Snapshot, error) {
	if err := params.Validate(); err != nil {
		return Snapshot{}, err
	}
	body, err := json.Marshal(struct {
		Algorithm string        `json:"algorithm"`
		Seed      int64         `json:"seed"`
		ImageID   string        `json:"image_id"`
		Params    SegmentParams `json:"params"`
	}{AlgorithmVersion, seed, imageID, params})
	if err != nil {
		return Snapshot{}, err
	}
	sum := sha256.Sum256(body)
	return Snapshot{
		ParamVersion:     hex.EncodeToString(sum[:16]),
		AlgorithmVersion: AlgorithmVersion,
		Seed:             seed,
		ImageID:          imageID,
		Params:           params,
	}, nil
}

func (s Snapshot) ValidateFor(imageID string, seed int64) error {
	if s.AlgorithmVersion != AlgorithmVersion {
		return VersionError(s.ParamVersion, AlgorithmVersion, s.AlgorithmVersion)
	}
	if s.ImageID != imageID || s.Seed != seed {
		return VersionError(s.ParamVersion, imageID+"/"+formatSeed(seed), s.ImageID+"/"+formatSeed(s.Seed))
	}
	return s.Params.Validate()
}

func formatSeed(seed int64) string {
	return strconv.FormatInt(seed, 10)
}

func ValidateManifest(m TruthManifest) error {
	if m.SchemaVersion == "" {
		return ParaError("schema_version", "non-empty", "manifest schema version is required")
	}
	if m.Width < 16 || m.Width > 8192 {
		return ParaError("width", "[16,8192]", "image width is outside the legal interval")
	}
	if m.Height < 16 || m.Height > 8192 {
		return ParaError("height", "[16,8192]", "image height is outside the legal interval")
	}
	if m.Channels != 4 {
		return ParaError("channels", "[4,4]", "only RGBA icons are supported")
	}
	if m.BlockSize < 8 || m.BlockSize > 512 {
		return ParaError("block_size", "[8,512]", "block size is outside the legal interval")
	}
	if m.WorkerBudget < 1 || m.WorkerBudget > 32 {
		return ParaError("worker_budget", "[1,32]", "worker budget is outside the legal interval")
	}
	if m.PerThreadPixelBuffer < m.BlockSize*m.BlockSize || m.PerThreadPixelBuffer > 16_777_216 {
		return ParaError("per_thread_pixel_buffer", "[blocksize^2,16777216]", "per-thread pixel buffer cannot hold one block")
	}
	if m.PreviewLatencyMS < 1 || m.PreviewLatencyMS > 60_000 {
		return ParaError("preview_latency_ms", "[1,60000]", "preview latency limit is outside the legal interval")
	}
	if m.MaskMismatchTolerance < 0 || m.MaskMismatchTolerance > 0.25 {
		return ParaError("mask_mismatch_tolerance", "[0,0.25]", "mask mismatch tolerance is outside the legal interval")
	}
	if m.HoleCountTolerance < 0 || m.HoleCountTolerance > 4 {
		return ParaError("hole_count_tolerance", "[0,4]", "hole count tolerance is outside the legal interval")
	}
	if m.PreviewCentroidTolerance < 0 || m.PreviewCentroidTolerance > 32 {
		return ParaError("preview_centroid_tolerance", "[0,32]", "preview centroid tolerance is outside the legal interval")
	}
	if m.PreviewAreaRelTolerance < 0 || m.PreviewAreaRelTolerance > 1 {
		return ParaError("preview_area_rel_tolerance", "[0,1]", "preview area relative tolerance is outside the legal interval")
	}
	if m.PreviewDiffPixelTolerance < 0 || m.PreviewDiffPixelTolerance > 1 {
		return ParaError("preview_diff_pixel_tolerance", "[0,1]", "preview diff-pixel tolerance is outside the legal interval")
	}
	if len(m.ReferenceMask) != m.Width*m.Height {
		return ParaError("reference_mask", "width*height bytes", "reference mask length does not match image dimensions")
	}
	return nil
}

func FingerprintTruth(m TruthManifest) string {
	type truthFingerprint struct {
		ImageID string           `json:"image_id"`
		Seed    int64            `json:"seed"`
		Width   int              `json:"width"`
		Height  int              `json:"height"`
		Mask    string           `json:"mask"`
		Regions []ComponentTruth `json:"regions"`
		Seeds   []Seed           `json:"seeds"`
	}
	regions := append([]ComponentTruth(nil), m.Regions...)
	sort.Slice(regions, func(i, j int) bool { return regions[i].ID < regions[j].ID })
	seeds := append([]Seed(nil), m.Seeds...)
	sort.Slice(seeds, func(i, j int) bool {
		if seeds[i].Y != seeds[j].Y {
			return seeds[i].Y < seeds[j].Y
		}
		if seeds[i].X != seeds[j].X {
			return seeds[i].X < seeds[j].X
		}
		return seeds[i].Kind < seeds[j].Kind
	})
	body, _ := json.Marshal(truthFingerprint{
		ImageID: m.ImageID, Seed: m.Seed, Width: m.Width, Height: m.Height,
		Mask: m.ReferenceMaskHash, Regions: regions, Seeds: seeds,
	})
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func (m TruthManifest) ReferenceFingerprintCheck() error {
	if m.ReferenceFingerprint == "" {
		return DecodeError("manifest is missing the reference fingerprint")
	}
	actual := FingerprintTruth(m)
	if actual != m.ReferenceFingerprint {
		return DecodeError("reference fingerprint does not match the embedded truth")
	}
	return nil
}

func MaskHash(mask []byte) string {
	sum := sha256.Sum256(mask)
	return hex.EncodeToString(sum[:])
}

func BytesHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func CloneBytes(src []byte) []byte {
	dst := make([]byte, len(src))
	copy(dst, src)
	return dst
}

func MaskAt(mask []byte, w, x, y int) bool {
	return mask[y*w+x] != 0
}

func SetMask(mask []byte, w, x, y int, value bool) {
	if value {
		mask[y*w+x] = 1
	} else {
		mask[y*w+x] = 0
	}
}

func NRGBAToRGBA(img *image.NRGBA) []byte {
	b := img.Bounds()
	out := make([]byte, b.Dx()*b.Dy()*4)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := img.NRGBAAt(x, y)
			i := ((y-b.Min.Y)*b.Dx() + (x - b.Min.X)) * 4
			out[i+0] = c.R
			out[i+1] = c.G
			out[i+2] = c.B
			out[i+3] = c.A
		}
	}
	return out
}

func RGBAAt(img *image.NRGBA, x, y int) color.NRGBA {
	return img.NRGBAAt(x, y)
}
