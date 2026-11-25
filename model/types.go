// Package model contains the stable data contracts shared by the terrain
// sample generator, loader, baker, persistence layer, and diagnostics.
package model

import "math"

// Vec2 is a horizontal terrain coordinate (x east, z north).
type Vec2 struct {
	X float64 `json:"x"`
	Z float64 `json:"z"`
}

// Vec3 is a terrain-space position (x east, y up, z north).
type Vec3 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

// Bounds is an axis-aligned terrain footprint.
type Bounds struct {
	MinX float64 `json:"min_x"`
	MinZ float64 `json:"min_z"`
	MaxX float64 `json:"max_x"`
	MaxZ float64 `json:"max_z"`
}

// Width returns the x extent.
func (b Bounds) Width() float64 { return b.MaxX - b.MinX }

// Depth returns the z extent.
func (b Bounds) Depth() float64 { return b.MaxZ - b.MinZ }

// Area returns the footprint area.
func (b Bounds) Area() float64 { return b.Width() * b.Depth() }

// Contains reports whether p is inside the closed bounds.
func (b Bounds) Contains(p Vec2) bool {
	return p.X >= b.MinX && p.X <= b.MaxX && p.Z >= b.MinZ && p.Z <= b.MaxZ
}

// Chunk is one regular terrain partition. Heights are stored row-major with z
// as the outer index and x as the inner index.
type Chunk struct {
	ID        string     `json:"id"`
	GridX     int        `json:"grid_x"`
	GridZ     int        `json:"grid_z"`
	Bounds    Bounds     `json:"bounds"`
	ResX      int        `json:"res_x"`
	ResZ      int        `json:"res_z"`
	Heights   []float64  `json:"heights"`
	Albedo    [3]float64 `json:"albedo"`
	Occluders []string   `json:"occluders"`
}

// SampleCount returns the declared number of height samples.
func (c Chunk) SampleCount() int { return len(c.Heights) }

// VertexIndex converts a local vertex coordinate to the row-major index.
func (c Chunk) VertexIndex(x, z int) int { return z*c.ResX + x }

// VertexAt returns the world position of a local vertex.
func (c Chunk) VertexAt(x, z int) Vec3 {
	fx := float64(x) / float64(c.ResX-1)
	fz := float64(z) / float64(c.ResZ-1)
	return Vec3{
		X: c.Bounds.MinX + fx*c.Bounds.Width(),
		Y: c.Heights[c.VertexIndex(x, z)],
		Z: c.Bounds.MinZ + fz*c.Bounds.Depth(),
	}
}

// Occluder is an independent vertical cylinder that can cast shadows. Chunks
// reference occluders by ID; the actual geometry is project-global.
type Occluder struct {
	ID     string  `json:"id"`
	Center Vec2    `json:"center"`
	Radius float64 `json:"radius"`
	Base   float64 `json:"base"`
	Height float64 `json:"height"`
}

// Top returns the occluder's top elevation.
func (o Occluder) Top() float64 { return o.Base + o.Height }

// SkySample is one discrete directional sky-light sample.
type SkySample struct {
	ID           string     `json:"id"`
	AzimuthDeg   float64    `json:"azimuth_deg"`
	ElevationDeg float64    `json:"elevation_deg"`
	Radiance     [3]float64 `json:"radiance"`
	Weight       float64    `json:"weight"`
}

// SunSample is one logical-time entry in the monotonic solar sequence.
type SunSample struct {
	Time         int        `json:"time"`
	AzimuthDeg   float64    `json:"azimuth_deg"`
	ElevationDeg float64    `json:"elevation_deg"`
	Radiance     [3]float64 `json:"radiance"`
}

// BakeParams fixes the numeric and scheduling contract used by the kernel.
type BakeParams struct {
	FixedScale          int64   `json:"fixed_scale"`
	MaxChannelFixed     int64   `json:"max_channel_fixed"`
	ShadowStepBudget    int     `json:"shadow_step_budget"`
	RayStep             float64 `json:"ray_step"`
	RayEpsilon          float64 `json:"ray_epsilon"`
	IrradianceTolerance float64 `json:"irradiance_tolerance"`
	MaxParallelism      int     `json:"max_parallelism"`
	VertexBufferCap     int     `json:"vertex_buffer_cap"`
	QueueBudget         int     `json:"queue_budget"`
	ParameterVersion    int     `json:"parameter_version"`
	AlgorithmVersion    string  `json:"algorithm_version"`
}

// Project is the complete deterministic terrain lighting engineering input.
type Project struct {
	Seed      int64       `json:"seed"`
	Bounds    Bounds      `json:"bounds"`
	Chunks    []Chunk     `json:"chunks"`
	Occluders []Occluder  `json:"occluders"`
	Sky       []SkySample `json:"sky"`
	Sun       []SunSample `json:"sun"`
	Params    BakeParams  `json:"params"`
}

// VertexPoint identifies one sample point within a chunk.
type VertexPoint struct {
	X int `json:"x"`
	Z int `json:"z"`
}

// WorldPoint identifies a world-space point and its containing sample.
type WorldPoint struct {
	ChunkID string  `json:"chunk_id"`
	X       float64 `json:"x"`
	Z       float64 `json:"z"`
}

// Hit describes the first ray sample that occluded a visibility test.
type Hit struct {
	Step     int     `json:"step"`
	Position Vec3    `json:"position"`
	ChunkID  string  `json:"chunk_id,omitempty"`
	Height   float64 `json:"height"`
	Kind     string  `json:"kind"`
	Occluder string  `json:"occluder,omitempty"`
}

// ShadowFlip records where a direct-light visibility state first changed from
// visible to blocked.
type ShadowFlip struct {
	AtStep   int  `json:"at_step"`
	Position Vec3 `json:"position"`
}

// Handoff records one chunk boundary crossing and the shared-edge heights
// sampled from both sides.
type Handoff struct {
	EdgeID      string  `json:"edge_id"`
	FromChunk   string  `json:"from_chunk"`
	ToChunk     string  `json:"to_chunk"`
	Position    Vec2    `json:"position"`
	HeightFrom  float64 `json:"height_from"`
	HeightTo    float64 `json:"height_to"`
	Consistent  bool    `json:"consistent"`
	SourceChunk string  `json:"source_chunk"`
}

// ShadowResult is the pure output of one direct-light visibility test.
type ShadowResult struct {
	Visible      bool        `json:"visible"`
	Shadow       bool        `json:"shadow"`
	Steps        int         `json:"steps"`
	StepBudget   int         `json:"step_budget"`
	FirstHit     *Hit        `json:"first_hit,omitempty"`
	Flip         *ShadowFlip `json:"flip,omitempty"`
	Handoffs     []Handoff   `json:"handoffs,omitempty"`
	SourceChunks []string    `json:"source_chunks,omitempty"`
	EdgeIDs      []string    `json:"edge_ids,omitempty"`
}

// Truncation records where a declared fixed-point accumulation discarded a
// fractional remainder.
type Truncation struct {
	Vertex   VertexPoint `json:"vertex"`
	World    Vec2        `json:"world"`
	Stage    string      `json:"stage"`
	Channel  string      `json:"channel"`
	Exact    float64     `json:"exact"`
	Fixed    int64       `json:"fixed"`
	Scale    int64       `json:"scale"`
	Residual float64     `json:"residual"`
}

// PointResult is the complete per-vertex result.
type PointResult struct {
	Vertex             VertexPoint  `json:"vertex"`
	World              Vec3         `json:"world"`
	DirectShadow       bool         `json:"direct_shadow"`
	Shadow             ShadowResult `json:"shadow"`
	SkyVisibility      float64      `json:"sky_visibility"`
	SkyVisibilityFixed int64        `json:"sky_visibility_fixed"`
	Irradiance         [3]float64   `json:"irradiance"`
	IrradianceFixed    [3]int64     `json:"irradiance_fixed"`
	Truncations        []Truncation `json:"truncations,omitempty"`
}

// TruthPoint is generator-authored ground truth. It is computed from source
// geometry and parameters only, never from a bake result.
type TruthPoint struct {
	ChunkID       string      `json:"chunk_id"`
	Time          int         `json:"time"`
	Vertex        VertexPoint `json:"vertex"`
	World         Vec3        `json:"world"`
	DirectShadow  bool        `json:"direct_shadow"`
	SkyVisibility float64     `json:"sky_visibility"`
	Irradiance    [3]float64  `json:"irradiance"`
	ShadowSteps   int         `json:"shadow_steps"`
	ShadowBudget  int         `json:"shadow_budget"`
	FirstHit      *Hit        `json:"first_hit,omitempty"`
}

// Truth is the deterministic verification inventory.
type Truth struct {
	Seed   int64        `json:"seed"`
	Points []TruthPoint `json:"points"`
}

// SampleBundle is the generated project plus independent truth inventory.
type SampleBundle struct {
	Project Project `json:"project"`
	Truth   Truth   `json:"truth"`
}

// SharedEdge is the explicit handoff contract between two chunk height fields.
type SharedEdge struct {
	ID         string    `json:"id"`
	ChunkA     string    `json:"chunk_a"`
	ChunkB     string    `json:"chunk_b"`
	Axis       string    `json:"axis"`
	Position   float64   `json:"position"`
	Start      float64   `json:"start"`
	End        float64   `json:"end"`
	HeightsA   []float64 `json:"heights_a"`
	HeightsB   []float64 `json:"heights_b"`
	Consistent bool      `json:"consistent"`
}

// Quantize converts a non-negative physical value to the declared fixed-point
// scale with truncation toward zero. A residual is returned whenever a
// non-zero fraction was discarded.
func Quantize(value float64, scale int64) (fixed int64, residual float64) {
	if scale <= 0 {
		return 0, value
	}
	scaled := value * float64(scale)
	fixed = int64(scaled)
	residual = scaled - math.Trunc(scaled)
	return fixed, residual
}
