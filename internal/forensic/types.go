package forensic

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

const (
	ManifestFormat   = "forensiclab.samples/v1"
	AlgorithmVersion = "forensiclab-algo-v1"
	SchemaVersion    = "forensiclab-schema-v2"
)

type Rect struct {
	X0 int `json:"x0"`
	Y0 int `json:"y0"`
	X1 int `json:"x1"`
	Y1 int `json:"y1"`
}

func (r Rect) Width() int  { return r.X1 - r.X0 }
func (r Rect) Height() int { return r.Y1 - r.Y0 }

func (r Rect) Intersects(other Rect) bool {
	return r.X0 < other.X1 && other.X0 < r.X1 && r.Y0 < other.Y1 && other.Y0 < r.Y1
}

type Primitive struct {
	Kind  string `json:"kind"`
	Rect  Rect   `json:"rect"`
	Color int    `json:"color"`
}

type TamperRegion struct {
	ID   string `json:"id"`
	Rect Rect   `json:"rect"`
}

type SampleEntry struct {
	ID               string         `json:"id"`
	ImagePath        string         `json:"imagePath"`
	BaselinePath     string         `json:"baselinePath"`
	MaskPath         string         `json:"maskPath"`
	Width            int            `json:"width"`
	Height           int            `json:"height"`
	Channels         int            `json:"channels"`
	Seed             uint64         `json:"seed"`
	PrimitiveCount   int            `json:"primitiveCount"`
	Primitives       []Primitive    `json:"primitives"`
	TamperRegions    []TamperRegion `json:"tamperRegions"`
	MaskWidth        int            `json:"maskWidth"`
	MaskHeight       int            `json:"maskHeight"`
	MaskTolerance    float64        `json:"maskTolerance"`
	ImageSHA256      string         `json:"imageSha256"`
	BaselineSHA256   string         `json:"baselineSha256"`
	ManifestChecksum string         `json:"manifestChecksum,omitempty"`
}

type Manifest struct {
	Format string        `json:"format"`
	Seed   uint64        `json:"seed"`
	Images []SampleEntry `json:"images"`
}

type Image struct {
	ID           string
	Width        int
	Height       int
	Channels     int
	Pix          []uint8
	Baseline     []uint8
	Mask         []uint8
	ImagePNG     []byte
	Entry        SampleEntry
	ManifestPath string
}

func (img *Image) Pixel(x, y, c int) uint8 {
	return img.Pix[(y*img.Width+x)*img.Channels+c]
}

func (img *Image) BlockBytes(b Chunk) []byte {
	out := make([]byte, 0, b.Width()*b.Height()*img.Channels)
	for y := b.Y0; y < b.Y1; y++ {
		start := (y*img.Width + b.X0) * img.Channels
		end := start + b.Width()*img.Channels
		out = append(out, img.Pix[start:end]...)
	}
	return out
}

func (img *Image) BlockDigest(b Chunk) string {
	h := sha256.New()
	var header [12]byte
	putInt(header[0:4], b.Width())
	putInt(header[4:8], b.Height())
	putInt(header[8:12], img.Channels)
	_, _ = h.Write(header[:])
	_, _ = h.Write(img.BlockBytes(b))
	return hex.EncodeToString(h.Sum(nil))
}

type Chunk struct {
	Index int `json:"index"`
	Row   int `json:"row"`
	Col   int `json:"col"`
	Rect
}

func (c Chunk) Width() int  { return c.X1 - c.X0 }
func (c Chunk) Height() int { return c.Y1 - c.Y0 }

type Params struct {
	HashAlgorithm    string  `json:"hashAlgorithm"`
	FingerprintGrid  int     `json:"fingerprintGrid"`
	Threshold        float64 `json:"threshold"`
	Window           int     `json:"window"`
	BlockSize        int     `json:"blockSize"`
	WorkerCount      int     `json:"workerCount"`
	PixelBufferLimit int     `json:"pixelBufferLimit"`
	QueueCapacity    int     `json:"queueCapacity"`
	MaskTolerance    float64 `json:"maskTolerance"`
}

func DefaultParams() Params {
	return Params{
		HashAlgorithm:    "sha256",
		FingerprintGrid:  4,
		Threshold:        0.45,
		Window:           3,
		BlockSize:        16,
		WorkerCount:      4,
		PixelBufferLimit: 2 * 1024 * 1024,
		QueueCapacity:    8,
		MaskTolerance:    0.08,
	}
}

func (p Params) Clone() Params { return p }

func (p Params) Validate(img *Image) error {
	if p.BlockSize <= 0 {
		return ParamError("blockSize", "must be a positive integer")
	}
	if img != nil {
		if grid, err := MakeChunks(img.Width, img.Height, p.BlockSize); err != nil {
			return err
		} else if !gridCovers(img.Width, img.Height, grid, p.BlockSize) {
			return ParamError("blockSize", "block grid does not cover the complete image")
		}
	}
	if p.Window < 3 {
		return ParamError("window", "must be at least 3")
	}
	if p.Window%2 == 0 {
		return ParamError("window", "must be odd so that it has a centered pixel")
	}
	if p.Threshold < 0 || p.Threshold > 1 {
		return ParamError("threshold", "must be within [0,1]")
	}
	if p.FingerprintGrid < 1 || p.FingerprintGrid > 32 {
		return ParamError("fingerprintGrid", "must be within [1,32]")
	}
	switch p.HashAlgorithm {
	case "sha256", "sha512", "sha1", "fnv64a":
	default:
		return ParamError("hashAlgorithm", "supported values: sha256, sha512, sha1, fnv64a")
	}
	if p.WorkerCount < 1 || p.WorkerCount > 64 {
		return ParamError("workerCount", "must be within [1,64]")
	}
	if p.QueueCapacity < 1 || p.QueueCapacity > 4096 {
		return ParamError("queueCapacity", "must be within [1,4096]")
	}
	if p.PixelBufferLimit < 1 {
		return ParamError("pixelBufferLimit", "must be a positive byte count")
	}
	if p.MaskTolerance < 0 || p.MaskTolerance > 1 {
		return ParamError("maskTolerance", "must be within [0,1]")
	}
	if img != nil {
		maxChunkBytes := maxBlockPixelBytes(img, p.BlockSize)
		if maxChunkBytes > p.PixelBufferLimit {
			return ParamError("pixelBufferLimit", fmt.Sprintf("must be at least %d bytes for one block", maxChunkBytes))
		}
	}
	return nil
}

func gridCovers(width, height int, chunks []Chunk, blockSize int) bool {
	if len(chunks) == 0 {
		return width == 0 && height == 0
	}
	var covered int64
	for _, c := range chunks {
		if c.Width() <= 0 || c.Height() <= 0 || c.Width() > blockSize || c.Height() > blockSize {
			return false
		}
		covered += int64(c.Width() * c.Height())
	}
	return covered == int64(width)*int64(height)
}

func maxBlockPixelBytes(img *Image, blockSize int) int {
	w := minInt(blockSize, img.Width)
	h := minInt(blockSize, img.Height)
	maxPixels := w * h
	return maxPixels * img.Channels
}

func MakeChunks(width, height, blockSize int) ([]Chunk, error) {
	if width <= 0 || height <= 0 {
		return nil, ParamError("blockSize", "image dimensions must be positive")
	}
	if blockSize <= 0 {
		return nil, ParamError("blockSize", "must be a positive integer")
	}
	cols := 1 + (width-1)/blockSize
	rows := 1 + (height-1)/blockSize
	if rows > 10_000_000 || cols > 10_000_000/rows {
		return nil, ParamError("blockSize", "creates too many blocks")
	}
	count := rows * cols
	out := make([]Chunk, 0, count)
	for row := 0; row < rows; row++ {
		for col := 0; col < cols; col++ {
			x0, y0 := col*blockSize, row*blockSize
			w := blockSize
			if width-x0 < w {
				w = width - x0
			}
			h := blockSize
			if height-y0 < h {
				h = height - y0
			}
			x1, y1 := x0+w, y0+h
			out = append(out, Chunk{
				Index: len(out), Row: row, Col: col,
				Rect: Rect{X0: x0, Y0: y0, X1: x1, Y1: y1},
			})
		}
	}
	return out, nil
}

type Stats struct {
	NoiseVariance    float64 `json:"noiseVariance"`
	NoiseRMS         float64 `json:"noiseRms"`
	ResidualCoverage float64 `json:"residualCoverage"`
	EdgeConsistency  float64 `json:"edgeConsistency"`
	Contrast         float64 `json:"contrast"`
	TamperScore      float64 `json:"tamperScore"`
}

type BlockState struct {
	ImageID          string  `json:"imageId,omitempty"`
	BlockIndex       int     `json:"blockIndex"`
	X0               int     `json:"x0"`
	Y0               int     `json:"y0"`
	X1               int     `json:"x1"`
	Y1               int     `json:"y1"`
	ContentDigest    string  `json:"contentDigest"`
	ContentHash      string  `json:"contentHash"`
	Fingerprint      string  `json:"fingerprint"`
	Stats            Stats   `json:"stats"`
	Tampered         bool    `json:"tampered"`
	Threshold        float64 `json:"threshold"`
	Window           int     `json:"window"`
	HashKey          string  `json:"hashKey"`
	StatsKey         string  `json:"statsKey"`
	VerdictKey       string  `json:"verdictKey"`
	SummaryKey       string  `json:"summaryKey"`
	ParamsVersion    string  `json:"paramsVersion"`
	AlgorithmVersion string  `json:"algorithmVersion"`
	CommitSeq        int64   `json:"commitSeq"`
	StateDigest      string  `json:"stateDigest"`
}

func (s BlockState) Rect() Rect { return Rect{X0: s.X0, Y0: s.Y0, X1: s.X1, Y1: s.Y1} }

func (s *BlockState) Finalize() {
	s.StateDigest = hashString(
		s.ContentDigest, s.ContentHash, s.Fingerprint,
		formatFloat(s.Stats.NoiseVariance),
		formatFloat(s.Stats.NoiseRMS),
		formatFloat(s.Stats.ResidualCoverage),
		formatFloat(s.Stats.EdgeConsistency),
		formatFloat(s.Stats.Contrast),
		formatFloat(s.Stats.TamperScore),
		fmt.Sprint(s.Tampered),
	)
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

type BlockTruth struct {
	BlockIndex int  `json:"blockIndex"`
	Tampered   bool `json:"tampered"`
	Pixels     int  `json:"pixels"`
}

type Summary struct {
	TotalBlocks      int            `json:"totalBlocks"`
	TamperedBlocks   int            `json:"tamperedBlocks"`
	ReusedBlocks     int            `json:"reusedBlocks"`
	RecomputedBlocks int            `json:"recomputedBlocks"`
	MeanScore        float64        `json:"meanScore"`
	TruthAgreement   float64        `json:"truthAgreement"`
	TruthConfusion   TruthConfusion `json:"truthConfusion"`
	Blocks           []BlockState   `json:"blocks"`
	CommitSequence   []int          `json:"commitSequence"`
	SummaryDigest    string         `json:"summaryDigest"`
}

type TruthConfusion struct {
	TruePositive  int `json:"truePositive"`
	TrueNegative  int `json:"trueNegative"`
	FalsePositive int `json:"falsePositive"`
	FalseNegative int `json:"falseNegative"`
}

func BuildSummary(states []BlockState, reused map[int]bool) Summary {
	sort.Slice(states, func(i, j int) bool { return states[i].BlockIndex < states[j].BlockIndex })
	s := Summary{TotalBlocks: len(states), Blocks: states}
	var sum float64
	for i := range states {
		sum += states[i].Stats.TamperScore
		if states[i].Tampered {
			s.TamperedBlocks++
		}
		if reused != nil && reused[states[i].BlockIndex] {
			s.ReusedBlocks++
		} else {
			s.RecomputedBlocks++
		}
		s.CommitSequence = append(s.CommitSequence, states[i].BlockIndex)
	}
	if len(states) > 0 {
		s.MeanScore = sum / float64(len(states))
	}
	h := sha256.New()
	enc := json.NewEncoder(h)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(states)
	s.SummaryDigest = hex.EncodeToString(h.Sum(nil))
	return s
}

func hashString(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		var n [8]byte
		putInt(n[:], len(part))
		_, _ = h.Write(n[:])
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func putInt(dst []byte, n int) {
	_ = dst[3]
	dst[0] = byte(n >> 24)
	dst[1] = byte(n >> 16)
	dst[2] = byte(n >> 8)
	dst[3] = byte(n)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func canonicalJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimSpace(buf.Bytes())
}
