package pipeline

import (
	"crypto/sha256"
	"encoding/hex"

	"iconbench/internal/imaging"
	"iconbench/internal/model"
	"iconbench/internal/segment"
)

type Block struct {
	Index  int `json:"index"`
	X      int `json:"x"`
	Y      int `json:"y"`
	X0     int `json:"x0"`
	Y0     int `json:"y0"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

type BlockStats struct {
	CleanMask  []byte             `json:"clean_mask"`
	PixelCount int                `json:"pixel_count"`
	MaskHash   string             `json:"mask_hash"`
	LocalStats []model.RegionStat `json:"local_stats,omitempty"`
}

type Artifact struct {
	SchemaVersion    string       `json:"schema_version"`
	AlgorithmVersion string       `json:"algorithm_version"`
	ImageID          string       `json:"image_id"`
	Block            Block        `json:"block"`
	Layer            string       `json:"layer"`
	Params           model.Params `json:"params"`

	Gradient []float64             `json:"gradient,omitempty"`
	Markers  *segment.MarkerResult `json:"markers,omitempty"`

	FloodSamples     int                  `json:"flood_samples,omitempty"`
	QueueSampleRatio float64              `json:"queue_sample_ratio,omitempty"`
	InitialLabels    []int32              `json:"initial_labels,omitempty"`
	Labels           []int32              `json:"labels,omitempty"`
	Regions          []segment.RegionInfo `json:"regions,omitempty"`
	Edges            []segment.RAGEdge    `json:"edges,omitempty"`
	MergeLog         []segment.MergeEvent `json:"merge_log,omitempty"`
	RawMask          []byte               `json:"raw_mask,omitempty"`

	Stats *BlockStats `json:"stats,omitempty"`
}

func BuildBlocks(width, height, tileSize int) []Block {
	if tileSize <= 0 {
		tileSize = 64
	}
	cols := (width + tileSize - 1) / tileSize
	var blocks []Block
	index := 0
	for by := 0; by*tileSize < height; by++ {
		for bx := 0; bx*tileSize < width; bx++ {
			x0 := bx * tileSize
			y0 := by * tileSize
			x1 := minInt(x0+tileSize, width)
			y1 := minInt(y0+tileSize, height)
			blocks = append(blocks, Block{
				Index: index, X: bx, Y: by, X0: x0, Y0: y0,
				Width: x1 - x0, Height: y1 - y0,
			})
			index++
		}
	}
	_ = cols
	return blocks
}

func CropRGB(src *imaging.RGBImage, b Block) *imaging.RGBImage {
	out := imaging.NewRGBImage(b.Width, b.Height)
	for y := 0; y < b.Height; y++ {
		for x := 0; x < b.Width; x++ {
			si := (b.Y0+y)*src.Width + (b.X0 + x)
			di := y*b.Width + x
			out.R[di] = src.R[si]
			out.G[di] = src.G[si]
			out.B[di] = src.B[si]
		}
	}
	return out
}

func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
