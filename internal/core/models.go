package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

const (
	AlgorithmVersion = "morph-edge/1.0.0"
	StageGradient    = "gradient"
	StageOpen        = "open"
	StageClose       = "close"
	StageThin        = "thin"
	StagePrune       = "prune"
)

var StageOrder = []string{StageGradient, StageOpen, StageClose, StageThin, StagePrune}

type Params struct {
	LowThreshold   int    `json:"lowThreshold"`
	HighThreshold  int    `json:"highThreshold"`
	Operator       string `json:"operator"`
	OpenRadius     int    `json:"openRadius"`
	CloseRadius    int    `json:"closeRadius"`
	ThinIterations int    `json:"thinIterations"`
	PruneLength    int    `json:"pruneLength"`
}

func DefaultParams() Params {
	return Params{
		LowThreshold:   10,
		HighThreshold:  22,
		Operator:       "sobel",
		OpenRadius:     0,
		CloseRadius:    0,
		ThinIterations: 2,
		PruneLength:    8,
	}
}

func (p Params) Validate() error {
	if p.LowThreshold < 0 || p.LowThreshold > 255 {
		return NewError(CodeParam, "lowThreshold", "low threshold must be in [0,255]")
	}
	if p.HighThreshold < 0 || p.HighThreshold > 255 {
		return NewError(CodeParam, "highThreshold", "high threshold must be in [0,255]")
	}
	if p.LowThreshold > p.HighThreshold {
		return NewError(CodeParam, "lowThreshold", "low threshold must not exceed high threshold")
	}
	switch p.Operator {
	case "sobel", "prewitt", "central":
	default:
		return NewError(CodeParam, "operator", "operator must be sobel, prewitt, or central")
	}
	if p.OpenRadius < 0 || p.OpenRadius > 16 {
		return NewError(CodeParam, "openRadius", "open radius must be in [0,16]")
	}
	if p.CloseRadius < 0 || p.CloseRadius > 16 {
		return NewError(CodeParam, "closeRadius", "close radius must be in [0,16]")
	}
	if p.ThinIterations < 0 || p.ThinIterations > 32 {
		return NewError(CodeParam, "thinIterations", "thin iterations must be in [0,32]")
	}
	if p.PruneLength < 0 || p.PruneLength > 256 {
		return NewError(CodeParam, "pruneLength", "prune length must be in [0,256]")
	}
	return nil
}

func (p Params) CanonicalJSON() string {
	b, _ := json.Marshal(p)
	return string(b)
}

func (p Params) Version() string {
	sum := sha256.Sum256([]byte(AlgorithmVersion + "|" + p.CanonicalJSON()))
	return hex.EncodeToString(sum[:8])
}

type LineTruth struct {
	Channel int     `json:"channel"`
	X1      float64 `json:"x1"`
	Y1      float64 `json:"y1"`
	X2      float64 `json:"x2"`
	Y2      float64 `json:"y2"`
}

type CircleTruth struct {
	Channel  int     `json:"channel"`
	CX       float64 `json:"cx"`
	CY       float64 `json:"cy"`
	Diameter float64 `json:"diameter"`
}

type Truth struct {
	Lines        []LineTruth   `json:"lines"`
	Circles      []CircleTruth `json:"circles"`
	EdgeCoverage float64       `json:"edgeCoverage"`
}

type ReferenceMask struct {
	Stage       string  `json:"stage"`
	File        string  `json:"file"`
	Fingerprint string  `json:"fingerprint"`
	Coverage    float64 `json:"coverage"`
}

type SampleManifest struct {
	Name        string          `json:"name"`
	Seed        int64           `json:"seed"`
	Width       int             `json:"width"`
	Height      int             `json:"height"`
	Channels    int             `json:"channels"`
	Noisy       bool            `json:"noisy"`
	LowContrast bool            `json:"lowContrast"`
	Fingerprint string          `json:"fingerprint"`
	ImageFile   string          `json:"imageFile"`
	Truth       Truth           `json:"truth"`
	References  []ReferenceMask `json:"references"`
	CreatedBy   string          `json:"createdBy"`
}

type LoadedSample struct {
	Dir      string         `json:"dir"`
	Manifest SampleManifest `json:"manifest"`
	Image    Image          `json:"-"`
}

type Geometry struct {
	Coverage float64             `json:"coverage"`
	Lines    []LineObservation   `json:"lines"`
	Circles  []CircleObservation `json:"circles"`
}

type LineObservation struct {
	Channel     int     `json:"channel"`
	X1          float64 `json:"x1"`
	Y1          float64 `json:"y1"`
	X2          float64 `json:"x2"`
	Y2          float64 `json:"y2"`
	TruthX1     float64 `json:"truthX1"`
	TruthY1     float64 `json:"truthY1"`
	TruthX2     float64 `json:"truthX2"`
	TruthY2     float64 `json:"truthY2"`
	EndpointErr float64 `json:"endpointErr"`
}

type CircleObservation struct {
	Channel       int     `json:"channel"`
	CX            float64 `json:"cx"`
	CY            float64 `json:"cy"`
	Diameter      float64 `json:"diameter"`
	TruthCX       float64 `json:"truthCx"`
	TruthCY       float64 `json:"truthCy"`
	TruthDiameter float64 `json:"truthDiameter"`
	CenterErr     float64 `json:"centerErr"`
	DiameterErr   float64 `json:"diameterErr"`
}

type BlockState string

const (
	BlockPending    BlockState = "pending"
	BlockProcessing BlockState = "processing"
	BlockCommitted  BlockState = "committed"
	BlockFailed     BlockState = "failed"
)

type Block struct {
	Index  int `json:"index"`
	Row    int `json:"row"`
	Col    int `json:"col"`
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

type Grid struct {
	Cols   int     `json:"cols"`
	Rows   int     `json:"rows"`
	BlockW int     `json:"blockW"`
	BlockH int     `json:"blockH"`
	Blocks []Block `json:"blocks"`
}

func BuildGrid(width, height, blockW, blockH int) Grid {
	if blockW <= 0 {
		blockW = 64
	}
	if blockH <= 0 {
		blockH = 64
	}
	cols := (width + blockW - 1) / blockW
	rows := (height + blockH - 1) / blockH
	g := Grid{Cols: cols, Rows: rows, BlockW: blockW, BlockH: blockH}
	idx := 0
	for row := 0; row < rows; row++ {
		for col := 0; col < cols; col++ {
			x, y := col*blockW, row*blockH
			w, h := min(blockW, width-x), min(blockH, height-y)
			g.Blocks = append(g.Blocks, Block{Index: idx, Row: row, Col: col, X: x, Y: y, Width: w, Height: h})
			idx++
		}
	}
	return g
}

func (g Grid) Block(index int) (Block, bool) {
	if index < 0 || index >= len(g.Blocks) {
		return Block{}, false
	}
	return g.Blocks[index], true
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
