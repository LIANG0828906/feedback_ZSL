package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

const (
	SchemaVersion    = "terrain-studio/v1"
	AlgorithmVersion = "normal-bake-v1.4.0"
	FixedSeed        = int64(0x51EED2026)
)

type ErrorCode string

const (
	CodeSchema ErrorCode = "E_SCHEMA"
	CodeSize   ErrorCode = "E_SIZE"
	CodeRef    ErrorCode = "E_REF"
	CodeSeam   ErrorCode = "E_SEAM"
	CodeDiff   ErrorCode = "E_DIFF"
	CodeBudget ErrorCode = "E_BUDGET"
	CodeIO     ErrorCode = "E_IO"
)

type Error struct {
	Code    ErrorCode      `json:"code"`
	BlockID string         `json:"block_id,omitempty"`
	Field   string         `json:"field,omitempty"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e.BlockID != "" && e.Field != "" {
		return fmt.Sprintf("%s block=%s field=%s: %s", e.Code, e.BlockID, e.Field, e.Message)
	}
	if e.BlockID != "" {
		return fmt.Sprintf("%s block=%s: %s", e.Code, e.BlockID, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func NewError(code ErrorCode, blockID, field, format string, args ...any) *Error {
	return &Error{Code: code, BlockID: blockID, Field: field, Message: fmt.Sprintf(format, args...)}
}

func AsError(err error) (*Error, bool) {
	if err == nil {
		return nil, false
	}
	if e, ok := err.(*Error); ok {
		return e, true
	}
	return nil, false
}

type Vec3 [3]float64

type Edge string

const (
	EdgeLeft   Edge = "left"
	EdgeRight  Edge = "right"
	EdgeTop    Edge = "top"
	EdgeBottom Edge = "bottom"
)

type Layer struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Strength  float64 `json:"strength"`
	Frequency float64 `json:"frequency"`
}

type BakeParams struct {
	Differential      string  `json:"differential"`
	HeightScale       float64 `json:"height_scale"`
	SeamThresholdDeg  float64 `json:"seam_threshold_deg"`
	WeldMaxCorrection float64 `json:"weld_max_correction_deg"`
}

func (p BakeParams) WithDefaults() BakeParams {
	if p.Differential == "" {
		p.Differential = "central"
	}
	if p.HeightScale == 0 {
		p.HeightScale = 1
	}
	if p.SeamThresholdDeg == 0 {
		p.SeamThresholdDeg = 8
	}
	if p.WeldMaxCorrection == 0 {
		p.WeldMaxCorrection = 12
	}
	return p
}

func (p BakeParams) ParameterVersion() string {
	h := sha256.New()
	fmt.Fprintf(h, "diff=%s|scale=%.12g|threshold=%.12g|weld=%.12g|", p.Differential, p.HeightScale, p.SeamThresholdDeg, p.WeldMaxCorrection)
	sum := hex.EncodeToString(h.Sum(nil))
	return "pv-" + sum[:12]
}

type Block struct {
	ID        string    `json:"id"`
	NX        int       `json:"nx"`
	NZ        int       `json:"nz"`
	DX        float64   `json:"dx"`
	DZ        float64   `json:"dz"`
	OriginX   float64   `json:"origin_x"`
	OriginZ   float64   `json:"origin_z"`
	Elevation []float64 `json:"-"`
	Weights   [][]uint8 `json:"-"`
}

func (b *Block) Index(i, j int) int { return j*b.NX + i }

func (b *Block) InBounds(i, j int) bool {
	return i >= 0 && i < b.NX && j >= 0 && j < b.NZ
}

func (b *Block) Height(i, j int) float64 {
	return b.Elevation[b.Index(i, j)]
}

type SeamConstraint struct {
	ID           string  `json:"id"`
	BlockA       string  `json:"block_a"`
	BlockB       string  `json:"block_b"`
	EdgeA        Edge    `json:"edge_a"`
	EdgeB        Edge    `json:"edge_b"`
	SampleCount  int     `json:"sample_count"`
	ThresholdDeg float64 `json:"threshold_deg,omitempty"`
	Note         string  `json:"note,omitempty"`
}

type PixelRef struct {
	BlockID string `json:"block_id"`
	I       int    `json:"i"`
	J       int    `json:"j"`
}

type NormalTruth struct {
	BlockID   string  `json:"block_id"`
	I         int     `json:"i"`
	J         int     `json:"j"`
	Normal    Vec3    `json:"normal"`
	SlopeDeg  float64 `json:"slope_deg"`
	Tolerance float64 `json:"tolerance"`
}

type SeamTruth struct {
	SeamID    string  `json:"seam_id"`
	Index     int     `json:"index"`
	AngleDeg  float64 `json:"angle_deg"`
	Tolerance float64 `json:"tolerance"`
	Metric    string  `json:"metric,omitempty"`
}

type TruthSet struct {
	Normals []NormalTruth `json:"normals"`
	Seams   []SeamTruth   `json:"seams"`
}

type Project struct {
	SchemaVersion string           `json:"schema_version"`
	Seed          int64            `json:"seed"`
	Params        BakeParams       `json:"params"`
	ParamVersion  string           `json:"param_version"`
	Layers        []Layer          `json:"layers"`
	Blocks        []*Block         `json:"blocks"`
	Seams         []SeamConstraint `json:"seams"`
	Truth         TruthSet         `json:"truth"`
	ManifestHash  string           `json:"manifest_hash,omitempty"`
}

func (p *Project) BlockByID(id string) *Block {
	for _, b := range p.Blocks {
		if b.ID == id {
			return b
		}
	}
	return nil
}

func (p *Project) SortedBlockIDs() []string {
	ids := make([]string, 0, len(p.Blocks))
	for _, b := range p.Blocks {
		ids = append(ids, b.ID)
	}
	sort.Strings(ids)
	return ids
}

type PixelBake struct {
	I        int      `json:"i"`
	J        int      `json:"j"`
	Normal   Vec3     `json:"normal"`
	RGB      [3]uint8 `json:"rgb"`
	SlopeDeg float64  `json:"slope_deg"`
	Weights  []uint8  `json:"weights"`
}

type BlockResult struct {
	BlockID      string      `json:"block_id"`
	NX           int         `json:"nx"`
	NZ           int         `json:"nz"`
	Pixels       []PixelBake `json:"pixels"`
	NormalDigest string      `json:"normal_digest"`
	SlopeDigest  string      `json:"slope_digest"`
	WeightDigest string      `json:"weight_digest"`
	ResultHash   string      `json:"result_hash"`
	NormalBytes  int64       `json:"normal_bytes"`
	PixelBuffer  int         `json:"-"`
}

type Attribution string

const (
	AttributionNone         Attribution = "NONE"
	AttributionGeometric    Attribution = "GEOMETRIC_CREASE"
	AttributionDifferential Attribution = "BAKE_DISCRETIZATION"
	AttributionMaterial     Attribution = "BAKE_MATERIAL_MIX"
	AttributionAmbiguous    Attribution = "AMBIGUOUS"
)

type SeamPointReport struct {
	Index               int                `json:"index"`
	PixelA              PixelRef           `json:"pixel_a"`
	PixelB              PixelRef           `json:"pixel_b"`
	RawAngleDeg         float64            `json:"raw_angle_deg"`
	GeometryAngleDeg    float64            `json:"geometry_angle_deg"`
	AlternativeAngleDeg float64            `json:"alternative_angle_deg"`
	WeldedAngleDeg      float64            `json:"welded_angle_deg"`
	ThresholdDeg        float64            `json:"threshold_deg"`
	Violation           bool               `json:"violation"`
	PostViolation       bool               `json:"post_violation"`
	Attribution         Attribution        `json:"attribution"`
	Code                ErrorCode          `json:"code,omitempty"`
	Basis               map[string]float64 `json:"basis,omitempty"`
	Message             string             `json:"message,omitempty"`
}

type SeamReport struct {
	SeamID            string              `json:"seam_id"`
	BlockA            string              `json:"block_a"`
	BlockB            string              `json:"block_b"`
	MaxRawAngleDeg    float64             `json:"max_raw_angle_deg"`
	MaxWeldedAngleDeg float64             `json:"max_welded_angle_deg"`
	ViolatingCount    int                 `json:"violating_count"`
	UnresolvedCount   int                 `json:"unresolved_count"`
	Points            []SeamPointReport   `json:"points"`
	ViolatingPoints   []SeamPointReport   `json:"violating_points"`
	AttributionCounts map[Attribution]int `json:"attribution_counts"`
}

func DigestBytes(prefix string, data []byte) string {
	sum := sha256.Sum256(append([]byte(prefix), data...))
	return hex.EncodeToString(sum[:])
}

func HashJSON(prefix string, v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return DigestBytes(prefix, data)
}

func CanonicalFloat(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*1e12) / 1e12
}
