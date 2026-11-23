// Package sample defines the deterministic sample sequences, their on-disk
// binary frame format, the truth manifest and the block frequency codec that
// produces the lossy reconstructions.
package sample

// Frame is one raw image in planar-interleaved channel order. Pixel (x,y,c)
// lives at index (y*Width+x)*Channels+c.
type Frame struct {
	Width    int
	Height   int
	Channels int
	Pix      []uint8
}

// At returns one channel value.
func (f *Frame) At(x, y, c int) int {
	return int(f.Pix[(y*f.Width+x)*f.Channels+c])
}

// Set writes one channel value.
func (f *Frame) Set(x, y, c, v int) {
	f.Pix[(y*f.Width+x)*f.Channels+c] = uint8(clamp255(v))
}

func clamp255(v int) int {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return v
}

// EdgePixel is an analytic geometry boundary pixel whose position is known at
// generation time and therefore part of the truth fingerprint.
type EdgePixel struct {
	X           int    `json:"x"`
	Y           int    `json:"y"`
	Channel     int    `json:"channel"`
	Description string `json:"description"`
}

// BlockQuantile is the truth per-block absolute pixel-error quantiles. All
// values are integer pixel levels; quantile rank is nearest-rank ceil(p*n).
type BlockQuantile struct {
	BX  int `json:"bx"`
	BY  int `json:"by"`
	Q10 int `json:"q10"`
	Q50 int `json:"q50"`
	Q90 int `json:"q90"`
}

// FrameFingerprint is the per-frame truth section of the manifest.
type FrameFingerprint struct {
	Frame          int             `json:"frame"`
	PSNRMin        float64         `json:"psnr_min"`
	PSNRMax        float64         `json:"psnr_max"`
	EdgePixels     []EdgePixel     `json:"edge_pixels"`
	BlockQuantiles []BlockQuantile `json:"block_quantiles"`
}

// Manifest is the single truth document. Acceptance always defers to it and
// never infers expected values that are absent here.
type Manifest struct {
	SchemaVersion  string                      `json:"schema_version"`
	Seed           int64                       `json:"seed"`
	Width          int                         `json:"width"`
	Height         int                         `json:"height"`
	Channels       int                         `json:"channels"`
	Frames         int                         `json:"frames"`
	BlockSize      int                         `json:"block_size"`
	QualityLevels  []int                       `json:"quality_levels"`
	SampleFrames   []int                       `json:"sample_frames"`
	Fingerprints   map[string]FrameFingerprint `json:"fingerprints"`
	GeneratedAtUTC string                      `json:"generated_at_utc"`
}
