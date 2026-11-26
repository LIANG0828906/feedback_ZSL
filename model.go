package quantforensics

import (
	"encoding/hex"
	"hash/fnv"
)

const (
	SchemaV1 = "forensics.v1"
	SchemaV2 = "forensics.v2"
	SchemaV3 = "forensics.v3"

	InternalSchema = "quant-forensics.internal/v1"

	AlgoVersion        = "det-block-quant-v1"
	ParamFormatVersion = "params-int-v1"
	CoordDeriveVersion = "coord-floor-v1"
)

type Metric string

const (
	MetricWeightedRGB Metric = "weighted_rgb"
	MetricEuclidean   Metric = "euclidean_rgb"
	MetricMaxRGB      Metric = "max_component"
)

type BlockKind string

const (
	KindForeground BlockKind = "foreground_shape"
	KindGradient   BlockKind = "dense_gradient"
	KindStroke     BlockKind = "translucent_stroke"
	KindNoise      BlockKind = "embedded_noise"
	KindMixed      BlockKind = "mixed"
)

// RGB is an 8-bit sRGB sample. Components are ordered R, G, B everywhere.
type RGB struct {
	R uint8 `json:"r"`
	G uint8 `json:"g"`
	B uint8 `json:"b"`
}

func (c RGB) Ints() [3]int { return [3]int{int(c.R), int(c.G), int(c.B)} }

func (c RGB) Component(name string) int {
	switch name {
	case "R", "r":
		return int(c.R)
	case "G", "g":
		return int(c.G)
	case "B", "b":
		return int(c.B)
	default:
		return 0
	}
}

type Image struct {
	ID     string `json:"id"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Pixels []RGB  `json:"pixels"`
	SHA256 string `json:"sha256"`
}

func (im Image) Index(x, y int) int { return y*im.Width + x }

func (im Image) At(x, y int) RGB { return im.Pixels[im.Index(x, y)] }

// ContentHash returns the precomputed whole-image hash. Block hashes are
// derived separately so cache keys remain stable for unchanged blocks.
func (im Image) ContentHash() string { return im.SHA256 }

type Block struct {
	ID        string    `json:"id"`
	ImageID   string    `json:"imageId"`
	Index     int       `json:"index"`
	Col       int       `json:"col"`
	Row       int       `json:"row"`
	X         int       `json:"x"`
	Y         int       `json:"y"`
	W         int       `json:"w"`
	H         int       `json:"h"`
	ResidualX int       `json:"residualX,omitempty"`
	ResidualY int       `json:"residualY,omitempty"`
	Kind      BlockKind `json:"kind"`
	Tags      []string  `json:"tags,omitempty"`

	// DitherPPM is the effective block value. HasDitherOverride distinguishes
	// an explicit zero from inheritance of the project/global value.
	DitherPPM         int64 `json:"ditherPPM"`
	HasDitherOverride bool  `json:"hasDitherOverride"`
}

func (b Block) Coordinate() BlockCoordinate {
	return BlockCoordinate{BlockID: b.ID, Col: b.Col, Row: b.Row, X: b.X, Y: b.Y, W: b.W, H: b.H}
}

type BlockCoordinate struct {
	BlockID string `json:"blockId"`
	Col     int    `json:"col"`
	Row     int    `json:"row"`
	X       int    `json:"x"`
	Y       int    `json:"y"`
	W       int    `json:"w"`
	H       int    `json:"h"`
}

type PaletteEntry struct {
	Index   int    `json:"index"`
	ID      string `json:"id"`
	Name    string `json:"name"`
	Group   string `json:"group"`
	Color   RGB    `json:"color"`
	GainPPM int64  `json:"gainPPM"`
}

// EffectiveColor applies the integer gain and clamps to the 8-bit range.
func (e PaletteEntry) EffectiveColor() RGB {
	if e.GainPPM <= 0 || e.GainPPM == 1_000_000 {
		return e.Color
	}
	scale := func(v uint8) uint8 {
		out := int64(v) * e.GainPPM / 1_000_000
		if out < 0 {
			out = 0
		}
		if out > 255 {
			out = 255
		}
		return uint8(out)
	}
	return RGB{R: scale(e.Color.R), G: scale(e.Color.G), B: scale(e.Color.B)}
}

type Palette struct {
	Version      string         `json:"version"`
	Organization string         `json:"organization"` // flat | grouped
	Entries      []PaletteEntry `json:"entries"`
}

func (p Palette) ByIndex(i int) (PaletteEntry, bool) {
	if i < 0 || i >= len(p.Entries) {
		return PaletteEntry{}, false
	}
	e := p.Entries[i]
	if e.Index == i {
		return e, true
	}
	for _, candidate := range p.Entries {
		if candidate.Index == i {
			return candidate, true
		}
	}
	return PaletteEntry{}, false
}

type Params struct {
	Metric             Metric `json:"metric"`
	QuantScalePPM      int64  `json:"quantScalePPM"`
	GainPPM            int64  `json:"gainPPM"`
	GlobalDitherPPM    int64  `json:"globalDitherPPM"`
	ParamVersion       string `json:"paramVersion"`
	AlgoVersion        string `json:"algoVersion"`
	CoordDeriveVersion string `json:"coordDeriveVersion"`
}

type Tolerance struct {
	MaxComponentDeviation int   `json:"maxComponentDeviation"`
	MaxWeightedColorDiff  int64 `json:"maxWeightedColorDiff"`
}

type Project struct {
	Schema       string    `json:"schema"`
	SourceSchema string    `json:"sourceSchema"`
	ProjectID    string    `json:"projectId"`
	Seed         int64     `json:"seed"`
	OriginX      int       `json:"originX"`
	OriginY      int       `json:"originY"`
	TileW        int       `json:"tileW"`
	TileH        int       `json:"tileH"`
	Images       []Image   `json:"images"`
	Blocks       []Block   `json:"blocks"`
	Palette      Palette   `json:"palette"`
	Params       Params    `json:"params"`
	Tolerance    Tolerance `json:"tolerance"`
	Traces       []Trace   `json:"traces,omitempty"`
}

type Trace struct {
	Field         string    `json:"field"`
	FromPath      string    `json:"fromPath,omitempty"`
	ToPath        string    `json:"toPath,omitempty"`
	Rule          string    `json:"rule"`
	OldValue      any       `json:"oldValue,omitempty"`
	NewValue      any       `json:"newValue,omitempty"`
	SourceDefault bool      `json:"sourceDefault"`
	DefaultValue  any       `json:"defaultValue,omitempty"`
	Residual      *Residual `json:"residual,omitempty"`
}

type LossItem struct {
	Field       string `json:"field"`
	Code        string `json:"code"`
	Kind        string `json:"kind"`
	Value       any    `json:"value,omitempty"`
	Reason      string `json:"reason"`
	Disposition string `json:"disposition"`
}

type ConversionReport struct {
	SourceVersion string     `json:"sourceVersion"`
	TargetVersion string     `json:"targetVersion"`
	Traces        []Trace    `json:"traces"`
	Losses        []LossItem `json:"losses"`
	OK            bool       `json:"ok"`
}

type PixelTruth struct {
	X             int    `json:"x"`
	Y             int    `json:"y"`
	ExpectedIndex int    `json:"expectedIndex"`
	QuantError    [3]int `json:"quantError"`
}

type BlockTruth struct {
	BlockID              string       `json:"blockId"`
	ImageID              string       `json:"imageId"`
	ExpectedPrimaryIndex int          `json:"expectedPrimaryIndex"`
	ExpectedIndices      []int        `json:"expectedIndices"`
	Pixels               []PixelTruth `json:"pixels"`
	ExpectedMaxDeviation [3]int       `json:"expectedMaxDeviation"`
	ExpectedWeightedDiff int64        `json:"expectedWeightedDiff"`
	Construction         string       `json:"construction"`
}

type DirtyRecord struct {
	PanelID  string `json:"panelId"`
	ImageID  string `json:"imageId"`
	BlockID  string `json:"blockId"`
	Reason   string `json:"reason"`
	EditPath string `json:"editPath"`
}

type SampleSet struct {
	Seed         int64         `json:"seed"`
	ProjectID    string        `json:"projectId"`
	Images       []Image       `json:"images"`
	Blocks       []Block       `json:"blocks"`
	Truths       []BlockTruth  `json:"truths"`
	DirtyRecords []DirtyRecord `json:"dirtyRecords"`
}

// StableHash returns a short non-cryptographic FNV hash used for deterministic
// local sequence derivation. Content cache hashes use SHA-256 separately.
func StableHash(parts ...string) uint64 {
	h := fnv.New64a()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return h.Sum64()
}

func HashHex(b []byte) string {
	sum := sha256Bytes(b)
	return hex.EncodeToString(sum[:])
}
