package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

const (
	SchemaVersion    = 1
	GeneratorVersion = "occlusionlab-gen/1"
	MaxAbsQuantized  = int64(1) << 50
)

// Vec3 is always expressed in the precision declared by Quantization. It is
// never a floating point value in project or checkpoint payloads.
type Vec3 struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
	Z int64 `json:"z"`
}

func (v Vec3) Add(o Vec3) Vec3 { return Vec3{X: v.X + o.X, Y: v.Y + o.Y, Z: v.Z + o.Z} }
func (v Vec3) Sub(o Vec3) Vec3 { return Vec3{X: v.X - o.X, Y: v.Y - o.Y, Z: v.Z - o.Z} }
func (v Vec3) AbsSum() int64 {
	x, y, z := v.X, v.Y, v.Z
	if x < 0 {
		x = -x
	}
	if y < 0 {
		y = -y
	}
	if z < 0 {
		z = -z
	}
	return x + y + z
}

type AABB struct {
	Min Vec3 `json:"min"`
	Max Vec3 `json:"max"`
}

func (b AABB) Valid() bool {
	return b.Min.X <= b.Max.X && b.Min.Y <= b.Max.Y && b.Min.Z <= b.Max.Z
}

func (b AABB) Center() Vec3 {
	return Vec3{
		X: b.Min.X + (b.Max.X-b.Min.X)/2,
		Y: b.Min.Y + (b.Max.Y-b.Min.Y)/2,
		Z: b.Min.Z + (b.Max.Z-b.Min.Z)/2,
	}
}

func (b AABB) Size() Vec3 {
	return b.Max.Sub(b.Min)
}

func (b AABB) Contains(p Vec3) bool {
	return p.X >= b.Min.X && p.X <= b.Max.X &&
		p.Y >= b.Min.Y && p.Y <= b.Max.Y &&
		p.Z >= b.Min.Z && p.Z <= b.Max.Z
}

func (b AABB) Corners() [8]Vec3 {
	return [8]Vec3{
		{X: b.Min.X, Y: b.Min.Y, Z: b.Min.Z},
		{X: b.Max.X, Y: b.Min.Y, Z: b.Min.Z},
		{X: b.Min.X, Y: b.Max.Y, Z: b.Min.Z},
		{X: b.Max.X, Y: b.Max.Y, Z: b.Min.Z},
		{X: b.Min.X, Y: b.Min.Y, Z: b.Max.Z},
		{X: b.Max.X, Y: b.Min.Y, Z: b.Max.Z},
		{X: b.Min.X, Y: b.Max.Y, Z: b.Max.Z},
		{X: b.Max.X, Y: b.Max.Y, Z: b.Max.Z},
	}
}

// Quantization is the declared, global numerical contract for a project.
type Quantization struct {
	WorldFracBits  uint8 `json:"world_frac_bits"`
	BasisFracBits  uint8 `json:"basis_frac_bits"`
	ScreenFracBits uint8 `json:"screen_frac_bits"`
	ParamFracBits  uint8 `json:"param_frac_bits"`
}

func (q Quantization) WorldStep() int64  { return int64(1) << q.WorldFracBits }
func (q Quantization) BasisStep() int64  { return int64(1) << q.BasisFracBits }
func (q Quantization) ScreenStep() int64 { return int64(1) << q.ScreenFracBits }
func (q Quantization) ParamStep() int64  { return int64(1) << q.ParamFracBits }

type Params struct {
	Quantization      Quantization `json:"quantization"`
	RasterResolution  int          `json:"raster_resolution"`
	MaxWorkers        int          `json:"max_workers"`
	RasterBufferCells int          `json:"raster_buffer_cells"`
	MaxResident       int          `json:"max_resident"`
	LoadCostBudget    int          `json:"load_cost_budget"`
	TolerancePxQ      int64        `json:"tolerance_px_q"`
	Version           int          `json:"version"`
}

func (p Params) Hash() string {
	return CanonicalHash(p)
}

type ProjectMeta struct {
	SchemaVersion    int          `json:"schema_version"`
	GeneratorVersion string       `json:"generator_version"`
	Seed             int64        `json:"seed"`
	Title            string       `json:"title"`
	Precision        Quantization `json:"precision"`
	TolerancePxQ     int64        `json:"tolerance_px_q"`
}

type Chunk struct {
	Index        int      `json:"index"`
	Label        string   `json:"label"`
	Bounds       AABB     `json:"bounds"`
	Anchor       Vec3     `json:"anchor"`
	OccluderRefs []string `json:"occluder_refs"`
	Complexity   int      `json:"complexity"`
	LoadCost     int      `json:"load_cost"`
	Color        string   `json:"color"`
}

type Occluder struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Bounds      AABB   `json:"bounds"`
	SourceChunk int    `json:"source_chunk"`
	Color       string `json:"color"`
}

type CameraSample struct {
	Window       int   `json:"window"`
	Time         int64 `json:"time"`
	Position     Vec3  `json:"position"`
	Right        Vec3  `json:"right"`
	Up           Vec3  `json:"up"`
	Forward      Vec3  `json:"forward"`
	Near         int64 `json:"near"`
	Far          int64 `json:"far"`
	TanHalfX     int64 `json:"tan_half_x"`
	TanHalfY     int64 `json:"tan_half_y"`
	FocalX       int64 `json:"focal_x"`
	FocalY       int64 `json:"focal_y"`
	CenterX      int64 `json:"center_x"`
	CenterY      int64 `json:"center_y"`
	ScreenWidth  int   `json:"screen_width"`
	ScreenHeight int   `json:"screen_height"`
}

type World struct {
	WorldBounds AABB           `json:"world_bounds"`
	Chunks      []Chunk        `json:"chunks"`
	Occluders   []Occluder     `json:"occluders"`
	Cameras     []CameraSample `json:"cameras"`
}

type TruthChunk struct {
	Chunk             int    `json:"chunk"`
	Visible           bool   `json:"visible"`
	ReferenceOccluder string `json:"reference_occluder"`
	Reason            string `json:"reason"`
	UncertaintyMargin int64  `json:"uncertainty_margin_q"`
}

type TruthSample struct {
	Window int          `json:"window"`
	Time   int64        `json:"time"`
	Chunks []TruthChunk `json:"chunks"`
}

type KnownProjection struct {
	ID        string `json:"id"`
	Window    int    `json:"window"`
	Chunk     int    `json:"chunk"`
	World     Vec3   `json:"world"`
	ExpectedX int64  `json:"expected_x_q"`
	ExpectedY int64  `json:"expected_y_q"`
	Note      string `json:"note"`
}

type Truth struct {
	Samples          []TruthSample     `json:"samples"`
	KnownProjections []KnownProjection `json:"known_projections"`
	TolerancePxQ     int64             `json:"tolerance_px_q"`
	OcclusionRule    string            `json:"occlusion_rule"`
	ProjectionRule   string            `json:"projection_rule"`
}

type Project struct {
	Meta        ProjectMeta `json:"meta"`
	World       World       `json:"world"`
	Truth       Truth       `json:"truth"`
	ContentHash string      `json:"content_hash"`
}

type Manifest struct {
	SchemaVersion int    `json:"schema_version"`
	ContentHash   string `json:"content_hash"`
	ProjectHash   string `json:"project_sha256"`
	WorldHash     string `json:"world_sha256"`
	TruthHash     string `json:"truth_sha256"`
}

func CanonicalJSON(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var tmp any
	if err := json.Unmarshal(b, &tmp); err != nil {
		return nil, err
	}
	return json.Marshal(tmp)
}

func CanonicalHash(v any) string {
	b, err := CanonicalJSON(v)
	if err != nil {
		sum := sha256.Sum256([]byte(err.Error()))
		return hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func ProjectContentHash(meta ProjectMeta, world World, truth Truth) string {
	return CanonicalHash(struct {
		Meta  ProjectMeta `json:"meta"`
		World World       `json:"world"`
		Truth Truth       `json:"truth"`
	}{meta, world, truth})
}

func FormatQ(v int64, bits uint8) string {
	if bits == 0 {
		return fmt.Sprintf("%d", v)
	}
	return fmt.Sprintf("%.6f", float64(v)/float64(int64(1)<<bits))
}
