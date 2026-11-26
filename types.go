package maskqa

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

const (
	SchemaVersion = "maskqa-schema-1.0"
	AlgoVersion   = "maskqa-algo-1.0.0"
)

type Stage string

const (
	StageGradient Stage = "gradient"
	StageEdge     Stage = "edge_band"
	StageRadius   Stage = "radius"
	StageClose    Stage = "close"
	StageOpen     Stage = "open"
	StageCompose  Stage = "compose"
)

var Stages = []Stage{StageGradient, StageEdge, StageRadius, StageClose, StageOpen, StageCompose}

type State string

const (
	StatePending    State = "pending"
	StateProcessing State = "processing"
	StateCommitted  State = "committed"
	StateFailed     State = "failed"
)

type Rect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

func (r Rect) Right() int  { return r.X + r.Width }
func (r Rect) Bottom() int { return r.Y + r.Height }
func (r Rect) Contains(x, y int) bool {
	return x >= r.X && y >= r.Y && x < r.Right() && y < r.Bottom()
}

type BlockCoord struct {
	IndexX  int `json:"index_x"`
	IndexY  int `json:"index_y"`
	OriginX int `json:"origin_x"`
	OriginY int `json:"origin_y"`
	Width   int `json:"width"`
	Height  int `json:"height"`
}

func (b BlockCoord) ID() string {
	return fmt.Sprintf("b%02d_%02d@%d,%d", b.IndexX, b.IndexY, b.OriginX, b.OriginY)
}

func BuildBlocks(width, height, blockSize int) []BlockCoord {
	var blocks []BlockCoord
	for by := 0; by*blockSize < height; by++ {
		for bx := 0; bx*blockSize < width; bx++ {
			x := bx * blockSize
			y := by * blockSize
			w := minInt(blockSize, width-x)
			h := minInt(blockSize, height-y)
			blocks = append(blocks, BlockCoord{IndexX: bx, IndexY: by, OriginX: x, OriginY: y, Width: w, Height: h})
		}
	}
	return blocks
}

func (b BlockCoord) PatchRect(imageWidth, imageHeight, halo int) Rect {
	x := maxInt(0, b.OriginX-halo)
	y := maxInt(0, b.OriginY-halo)
	right := minInt(imageWidth, b.OriginX+b.Width+halo)
	bottom := minInt(imageHeight, b.OriginY+b.Height+halo)
	return Rect{X: x, Y: y, Width: right - x, Height: bottom - y}
}

type Mask struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Data   []byte `json:"data"`
}

func NewMask(width, height int) *Mask {
	return &Mask{Width: width, Height: height, Data: make([]byte, width*height)}
}

func (m *Mask) index(x, y int) int { return y*m.Width + x }

func (m *Mask) InBounds(x, y int) bool {
	return m != nil && x >= 0 && y >= 0 && x < m.Width && y < m.Height
}

func (m *Mask) At(x, y int) byte {
	if !m.InBounds(x, y) {
		return 0
	}
	return m.Data[m.index(x, y)]
}

func (m *Mask) Set(x, y int, v byte) {
	if m.InBounds(x, y) {
		m.Data[m.index(x, y)] = v
	}
}

func (m *Mask) Clone() *Mask {
	if m == nil {
		return nil
	}
	cp := NewMask(m.Width, m.Height)
	copy(cp.Data, m.Data)
	return cp
}

func (m *Mask) Equal(other *Mask) bool {
	if m == nil || other == nil || m.Width != other.Width || m.Height != other.Height {
		return false
	}
	if len(m.Data) != len(other.Data) {
		return false
	}
	for i := range m.Data {
		if (m.Data[i] != 0) != (other.Data[i] != 0) {
			return false
		}
	}
	return true
}

func XORCount(a, b *Mask) int {
	if a == nil || b == nil || a.Width != b.Width || a.Height != b.Height {
		return -1
	}
	n := 0
	for i := range a.Data {
		if (a.Data[i] != 0) != (b.Data[i] != 0) {
			n++
		}
	}
	return n
}

type Raster struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	RGBA   []byte `json:"rgba"`
}

func (r *Raster) RGBAAt(x, y int) (byte, byte, byte, byte) {
	i := (y*r.Width + x) * 4
	return r.RGBA[i], r.RGBA[i+1], r.RGBA[i+2], r.RGBA[i+3]
}

type Component struct {
	ID        string  `json:"id"`
	CentroidX float64 `json:"centroid_x"`
	CentroidY float64 `json:"centroid_y"`
	Area      int     `json:"area"`
	Bounds    Rect    `json:"bounds"`
	Holes     int     `json:"holes"`
}

type Truth struct {
	Mask       *Mask       `json:"mask"`
	Components []Component `json:"components"`
}

type Params struct {
	SchemaVersion            string `json:"schema_version"`
	Seed                     int64  `json:"seed"`
	BlockSize                int    `json:"block_size"`
	Operator                 string `json:"operator"`
	OperatorSize             int    `json:"operator_size"`
	FixedScale               int64  `json:"fixed_scale"`
	LumaWeightR              int    `json:"luma_weight_r"`
	LumaWeightG              int    `json:"luma_weight_g"`
	LumaWeightB              int    `json:"luma_weight_b"`
	QuantilePermille         int    `json:"quantile_permille"`
	EdgeBandWidth            int    `json:"edge_band_width"`
	CurvatureBins            int    `json:"curvature_bins"`
	CurvatureWindow          int    `json:"curvature_window"`
	RadiusMin                int    `json:"radius_min"`
	RadiusMax                int    `json:"radius_max"`
	MaxIterations            int    `json:"max_iterations"`
	ChangeThreshold          int    `json:"change_threshold"`
	ComposeThresholdPermille int    `json:"compose_threshold_permille"`
	ComposeOpenWeight        int    `json:"compose_open_weight"`
	ComposeAuxWeight         int    `json:"compose_aux_weight"`
	TraceScale               int64  `json:"trace_scale"`
	MaxParallelBlocks        int    `json:"max_parallel_blocks"`
	ThreadPixelBufferLimit   int    `json:"thread_pixel_buffer_limit"`
	QueuedPixelBudget        int    `json:"queued_pixel_budget"`
}

func DefaultParams() Params {
	return Params{
		SchemaVersion:            SchemaVersion,
		Seed:                     4208691,
		BlockSize:                32,
		Operator:                 "sobel3",
		OperatorSize:             3,
		FixedScale:               1024,
		LumaWeightR:              299,
		LumaWeightG:              587,
		LumaWeightB:              114,
		QuantilePermille:         720,
		EdgeBandWidth:            2,
		CurvatureBins:            4,
		CurvatureWindow:          3,
		RadiusMin:                1,
		RadiusMax:                1,
		MaxIterations:            4,
		ChangeThreshold:          2,
		ComposeThresholdPermille: 500,
		ComposeOpenWeight:        3,
		ComposeAuxWeight:         1,
		TraceScale:               1_000_000,
		MaxParallelBlocks:        2,
		ThreadPixelBufferLimit:   262_144,
		QueuedPixelBudget:        131_072,
	}
}

type Sample struct {
	ID        string       `json:"id"`
	Width     int          `json:"width"`
	Height    int          `json:"height"`
	Raster    *Raster      `json:"raster"`
	Rough     *Mask        `json:"rough"`
	Truth     *Truth       `json:"truth"`
	InputHash string       `json:"input_hash"`
	Blocks    []BlockCoord `json:"blocks"`
}

type Dataset struct {
	SchemaVersion string    `json:"schema_version"`
	AlgoVersion   string    `json:"algo_version"`
	Seed          int64     `json:"seed"`
	Params        Params    `json:"params"`
	RadiusBand    [2]int    `json:"radius_band"`
	Samples       []*Sample `json:"samples"`
}

func (d *Dataset) Sample(id string) *Sample {
	for _, s := range d.Samples {
		if s.ID == id {
			return s
		}
	}
	return nil
}

func HashCanonical(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		h := sha256.Sum256([]byte(fmt.Sprintf("%#v", v)))
		return hex.EncodeToString(h[:])
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func HashBytes(prefix string, b ...[]byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(prefix))
	for _, chunk := range b {
		_, _ = h.Write(chunk)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func SortedMapKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
