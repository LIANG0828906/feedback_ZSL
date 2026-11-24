package model

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

const (
	SchemaVersion    = "terrain-bake/v1"
	AlgorithmVersion = "tnb-1.0.0"
	Coefficient      = 0.5
	WeightScale      = 1000
)

type Error struct {
	Code    string `json:"code"`
	BlockID int    `json:"blockId"`
	Field   string `json:"field"`
	Message string `json:"message"`
	Value   any    `json:"value,omitempty"`
	Want    any    `json:"want,omitempty"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s block=%d field=%s: %s", e.Code, e.BlockID, e.Field, e.Message)
}

func NewError(code string, blockID int, field, message string) *Error {
	return &Error{Code: code, BlockID: blockID, Field: field, Message: message}
}

const (
	ErrSchema   = "E_SCHEMA"
	ErrSize     = "E_SIZE"
	ErrWeight   = "E_WEIGHT"
	ErrPara     = "E_PARA"
	ErrSeam     = "E_SEAM"
	ErrDiff     = "E_DIFF"
	ErrBudget   = "E_BUDGET"
	ErrBlock    = "E_BLOCK"
	ErrIO       = "E_IO"
	ErrSnapshot = "E_SNAPSHOT"
)

type Vec3 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

func (v Vec3) Add(o Vec3) Vec3 {
	return Vec3{v.X + o.X, v.Y + o.Y, v.Z + o.Z}
}

func (v Vec3) Sub(o Vec3) Vec3 {
	return Vec3{v.X - o.X, v.Y - o.Y, v.Z - o.Z}
}

func (v Vec3) Mul(s float64) Vec3 {
	return Vec3{v.X * s, v.Y * s, v.Z * s}
}

func (v Vec3) Dot(o Vec3) float64 {
	return v.X*o.X + v.Y*o.Y + v.Z*o.Z
}

func (v Vec3) Length() float64 {
	return math.Sqrt(v.Dot(v))
}

func (v Vec3) Normalize() Vec3 {
	l := v.Length()
	if l < 1e-15 {
		return Vec3{Z: 1}
	}
	return v.Mul(1 / l)
}

func (v Vec3) Lerp(o Vec3, t float64) Vec3 {
	return v.Mul(1 - t).Add(o.Mul(t))
}

func (v Vec3) Equal(o Vec3) bool {
	return math.Float64bits(v.X) == math.Float64bits(o.X) &&
		math.Float64bits(v.Y) == math.Float64bits(o.Y) &&
		math.Float64bits(v.Z) == math.Float64bits(o.Z)
}

type BoundingBox struct {
	Min Vec3 `json:"min"`
	Max Vec3 `json:"max"`
}

type MaterialLayer struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	NormalBiasX  float64 `json:"normalBiasX"`
	NormalBiasY  float64 `json:"normalBiasY"`
	NormalBiasZ  float64 `json:"normalBiasZ"`
	Strength     float64 `json:"strength"`
	ParameterTag string  `json:"parameterTag"`
}

type Block struct {
	ID      int       `json:"id"`
	Name    string    `json:"name"`
	Width   int       `json:"width"`
	Height  int       `json:"height"`
	OriginX int       `json:"originX"`
	OriginY int       `json:"originY"`
	Heights []float64 `json:"heights"`
	Weights [][]int   `json:"weights"`
}

type BakeParameters struct {
	DifferenceOperator string  `json:"differenceOperator"`
	Scale              float64 `json:"scale"`
	MaterialMix        string  `json:"materialMix"`
	PreviewOnly        bool    `json:"previewOnly"`
	SeamRelaxation     float64 `json:"seamRelaxation"`
}

type Seam struct {
	ID                        string  `json:"id"`
	BlockA                    int     `json:"blockA"`
	BlockB                    int     `json:"blockB"`
	Axis                      string  `json:"axis"`
	EdgeA                     string  `json:"edgeA"`
	EdgeB                     string  `json:"edgeB"`
	ThresholdDeg              float64 `json:"thresholdDeg"`
	MaxIterations             int     `json:"maxIterations"`
	GeometrySlopeThresholdDeg float64 `json:"geometrySlopeThresholdDeg"`
}

type PixelTruth struct {
	X         int     `json:"x"`
	Y         int     `json:"y"`
	Normal    Vec3    `json:"normal"`
	SlopeDeg  float64 `json:"slopeDeg"`
	Tolerance float64 `json:"tolerance"`
}

type BlockTruth struct {
	BlockID int          `json:"blockId"`
	Pixels  []PixelTruth `json:"pixels"`
}

type SeamTruth struct {
	SeamID                   string   `json:"seamId"`
	ThresholdDeg             float64  `json:"thresholdDeg"`
	ExpectedContinuous       bool     `json:"expectedContinuous"`
	ExpectedAdjacentAngleDeg float64  `json:"expectedAdjacentAngleDeg"`
	ToleranceDeg             float64  `json:"toleranceDeg"`
	ExpectedCategories       []string `json:"expectedCategories,omitempty"`
}

type TruthManifest struct {
	Blocks []BlockTruth `json:"blocks"`
	Seams  []SeamTruth  `json:"seams"`
}

type Project struct {
	SchemaVersion string          `json:"schemaVersion"`
	Seed          int64           `json:"seed"`
	Revision      int64           `json:"revision"`
	World         World           `json:"world"`
	Parameters    BakeParameters  `json:"parameters"`
	Layers        []MaterialLayer `json:"layers"`
	Blocks        []Block         `json:"blocks"`
	Seams         []Seam          `json:"seams"`
	Truth         TruthManifest   `json:"truth"`
}

type World struct {
	Unit        string  `json:"unit"`
	HeightScale float64 `json:"heightScale"`
}

func (p *Project) Clone() *Project {
	data, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	var out Project
	if err := json.Unmarshal(data, &out); err != nil {
		panic(err)
	}
	return &out
}

func (p *Project) Block(id int) (*Block, bool) {
	for i := range p.Blocks {
		if p.Blocks[i].ID == id {
			return &p.Blocks[i], true
		}
	}
	return nil, false
}

func (p *Project) Layer(id string) (*MaterialLayer, bool) {
	for i := range p.Layers {
		if p.Layers[i].ID == id {
			return &p.Layers[i], true
		}
	}
	return nil, false
}

func (p *Project) SortedBlocks() []Block {
	out := append([]Block(nil), p.Blocks...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (p *Project) SortedSeams() []Seam {
	out := append([]Seam(nil), p.Seams...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (b *Block) PixelCount() int {
	return b.Width * b.Height
}

func (b *Block) ValidXY(x, y int) bool {
	return x >= 0 && y >= 0 && x < b.Width && y < b.Height
}

func (b *Block) Index(x, y int) int {
	return y*b.Width + x
}

func (b *Block) HeightAt(x, y int) float64 {
	return b.Heights[b.Index(x, y)]
}

func (b *Block) WeightsAt(x, y int) []int {
	return b.Weights[b.Index(x, y)]
}

func (p *Project) CanonicalHash() string {
	clone := p.Clone()
	clone.Revision = 0
	data, _ := json.Marshal(clone)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func HashFloat64(values []float64) string {
	h := sha256.New()
	var buf [8]byte
	for _, v := range values {
		binary.LittleEndian.PutUint64(buf[:], math.Float64bits(v))
		h.Write(buf[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func HashNormals(values []Vec3) string {
	flat := make([]float64, 0, len(values)*3)
	for _, v := range values {
		flat = append(flat, v.X, v.Y, v.Z)
	}
	return HashFloat64(flat)
}

func HashJSON(v any) string {
	data, _ := json.Marshal(v)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func ParameterVersion(p BakeParameters) string {
	canonical := struct {
		Operator string  `json:"operator"`
		Scale    float64 `json:"scale"`
		Mix      string  `json:"mix"`
		Relax    float64 `json:"relax"`
	}{p.DifferenceOperator, p.Scale, p.MaterialMix, p.SeamRelaxation}
	return HashJSON(canonical)
}

func Clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func AngleDeg(a, b Vec3) float64 {
	d := Clamp(a.Normalize().Dot(b.Normalize()), -1, 1)
	return math.Acos(d) * 180 / math.Pi
}

func SlopeDeg(n Vec3) float64 {
	n = n.Normalize()
	return math.Acos(Clamp(n.Z, -1, 1)) * 180 / math.Pi
}

func ColorCode(n Vec3) [3]uint8 {
	n = n.Normalize()
	return [3]uint8{
		uint8(math.Round(Clamp(n.X*Coefficient+Coefficient, 0, 1) * 255)),
		uint8(math.Round(Clamp(n.Y*Coefficient+Coefficient, 0, 1) * 255)),
		uint8(math.Round(Clamp(n.Z*Coefficient+Coefficient, 0, 1) * 255)),
	}
}
