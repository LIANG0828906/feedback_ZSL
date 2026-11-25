package fog

import (
	"fmt"
	"math/rand"
)

const (
	SchemaVersion    = "volfog.scene/v1"
	AlgorithmVersion = "volfog-algo-1"
	DefaultSeed      = int64(0x564f472d32303236) // "VFOG-2026"
)

type PhaseKind string

const (
	PhaseIsotropic PhaseKind = "isotropic"
	PhaseHG        PhaseKind = "henyey_greenstein"
)

type Light struct {
	ID           string  `json:"id"`
	Direction    Vec3    `json:"direction_to_light"`
	Radiance     float64 `json:"radiance"`
	Ambient      float64 `json:"ambient"`
	ShadowJitter float64 `json:"shadow_jitter"`
}

type Recipe struct {
	Version              string    `json:"version"`
	Steps                int       `json:"steps"`
	PreviewSteps         int       `json:"preview_steps"`
	ShadowSamples        int       `json:"shadow_samples"`
	PreviewShadowSamples int       `json:"preview_shadow_samples"`
	Phase                PhaseKind `json:"phase"`
	FixedScale           int64     `json:"fixed_scale"`
	TileSize             int       `json:"tile_size"`
	MaxParallelBlocks    int       `json:"max_parallel_blocks"`
	RayBufferPerWorker   int       `json:"ray_buffer_per_worker"`
	ShadowSampleBudget   int64     `json:"shadow_sample_budget"`
	PreviewDelayLimitMS  int64     `json:"preview_delay_limit_ms"`
	TransmittanceTol     float64   `json:"transmittance_tolerance"`
	RadianceTol          float64   `json:"radiance_tolerance"`
	PreviewTransTol      float64   `json:"preview_transmittance_tolerance"`
	PreviewRadianceTol   float64   `json:"preview_radiance_tolerance"`
}

type DensityField struct {
	Resolution [3]int  `json:"resolution"`
	VoxelSize  float64 `json:"voxel_size"`
	MinValue   int32   `json:"min_value"`
	MaxValue   int32   `json:"max_value"`
	Values     []int32 `json:"values"`
}

func (d DensityField) VoxelCount() int {
	return d.Resolution[0] * d.Resolution[1] * d.Resolution[2]
}

func (d DensityField) Index(x, y, z int) int {
	return x + d.Resolution[0]*(y+d.Resolution[1]*z)
}

func (d DensityField) InBounds(x, y, z int) bool {
	return x >= 0 && x < d.Resolution[0] &&
		y >= 0 && y < d.Resolution[1] &&
		z >= 0 && z < d.Resolution[2]
}

func (d DensityField) VoxelAABB(medium AABB, x, y, z int) AABB {
	vs := d.VoxelSize
	return AABB{
		Min: Vec3{
			X: medium.Min.X + float64(x)*vs,
			Y: medium.Min.Y + float64(y)*vs,
			Z: medium.Min.Z + float64(z)*vs,
		},
		Max: Vec3{
			X: medium.Min.X + float64(x+1)*vs,
			Y: medium.Min.Y + float64(y+1)*vs,
			Z: medium.Min.Z + float64(z+1)*vs,
		},
	}
}

func (d DensityField) VoxelAtWorld(b AABB, p Vec3) (x, y, z int, ok bool) {
	if !b.Contains(p) {
		return 0, 0, 0, false
	}
	x = int((p.X - b.Min.X) / d.VoxelSize)
	y = int((p.Y - b.Min.Y) / d.VoxelSize)
	z = int((p.Z - b.Min.Z) / d.VoxelSize)
	// Defensive boundary handling for points exactly on Max.
	if x >= d.Resolution[0] {
		x = d.Resolution[0] - 1
	}
	if y >= d.Resolution[1] {
		y = d.Resolution[1] - 1
	}
	if z >= d.Resolution[2] {
		z = d.Resolution[2] - 1
	}
	if !d.InBounds(x, y, z) {
		return 0, 0, 0, false
	}
	return x, y, z, true
}

func (d DensityField) At(b AABB, p Vec3) int32 {
	x, y, z, ok := d.VoxelAtWorld(b, p)
	if !ok {
		return 0
	}
	return d.Values[d.Index(x, y, z)]
}

type Medium struct {
	ID          string       `json:"id"`
	Bounds      AABB         `json:"bounds"`
	Absorption  float64      `json:"absorption"`
	Scattering  float64      `json:"scattering"`
	Anisotropy  float64      `json:"anisotropy"`
	Field       DensityField `json:"field"`
	CastShadows bool         `json:"cast_shadows"`
	ContentHash string       `json:"content_hash"`
}

type mediumHashData struct {
	ID         string       `json:"id"`
	Bounds     AABB         `json:"bounds"`
	Absorption float64      `json:"absorption"`
	Scattering float64      `json:"scattering"`
	Anisotropy float64      `json:"anisotropy"`
	Field      DensityField `json:"field"`
}

func (m *Medium) Rehash() string {
	m.ContentHash = ContentHash("medium", mediumHashData{
		ID: m.ID, Bounds: m.Bounds, Absorption: m.Absorption,
		Scattering: m.Scattering, Anisotropy: m.Anisotropy, Field: m.Field,
	})
	return m.ContentHash
}

type DensityEdit struct {
	MediumID string `json:"medium_id"`
	X        int    `json:"x"`
	Y        int    `json:"y"`
	Z        int    `json:"z"`
	OldValue int32  `json:"old_value"`
	NewValue int32  `json:"new_value"`
	Reason   string `json:"reason"`
}

type Scenario struct {
	SchemaVersion string           `json:"schema_version"`
	Seed          int64            `json:"seed"`
	Width         int              `json:"width"`
	Height        int              `json:"height"`
	FixedScale    int64            `json:"fixed_scale"`
	Light         Light            `json:"light"`
	Recipe        Recipe           `json:"recipe"`
	Cameras       CameraTrajectory `json:"cameras"`
	Media         []*Medium        `json:"media"`
	Truth         TruthBook        `json:"truth"`
	DeclaredEdit  DensityEdit      `json:"declared_edit"`
}

func (s *Scenario) Medium(id string) (*Medium, bool) {
	for _, m := range s.Media {
		if m.ID == id {
			return m, true
		}
	}
	return nil, false
}

func (s *Scenario) Camera(id string) (Camera, bool) {
	for _, c := range s.Cameras.Cameras {
		if c.ID == id {
			return c, true
		}
	}
	return Camera{}, false
}

func (s *Scenario) ActiveCamera() Camera {
	if len(s.Cameras.Cameras) == 0 {
		return Camera{}
	}
	return s.Cameras.Cameras[0]
}

// Generate builds the fixed-seed diagnostic scene. The RNG only fills the
// patterned density field; all coordinate and truth construction remains
// deterministic across processes and thread schedules.
func Generate(seed int64) (*Scenario, error) {
	if seed == 0 {
		seed = DefaultSeed
	}
	rng := rand.New(rand.NewSource(seed))
	scale := DefaultFixedScale

	recipe := Recipe{
		Version:              "recipe-v1-fixed-sample",
		Steps:                18,
		PreviewSteps:         6,
		ShadowSamples:        4,
		PreviewShadowSamples: 1,
		Phase:                PhaseHG,
		FixedScale:           scale,
		TileSize:             8,
		MaxParallelBlocks:    4,
		RayBufferPerWorker:   32,
		ShadowSampleBudget:   1_000_000,
		PreviewDelayLimitMS:  500,
		TransmittanceTol:     2e-7,
		RadianceTol:          2e-6,
		PreviewTransTol:      0.10,
		PreviewRadianceTol:   0.05,
	}

	lightDir := Vec3{X: 0.22, Y: 0.38, Z: 0.90}.Normalize()
	light := Light{
		ID:           "key-sun",
		Direction:    lightDir,
		Radiance:     1.35,
		Ambient:      0.22,
		ShadowJitter: 0.012,
	}

	cameras := CameraTrajectory{Cameras: []Camera{
		makeCamera("c0", 0.0, Vec3{X: 0, Y: 0, Z: 0}),
		makeCamera("c1", 1.0, Vec3{X: 0.10, Y: 0.015, Z: 0}),
		makeCamera("c2", 2.0, Vec3{X: -0.08, Y: -0.01, Z: 0}),
	}}
	for i := range cameras.Cameras {
		cameras.Cameras[i].Version = ContentHash("camera", cameras.Cameras[i])
	}

	uniform := &Medium{
		ID:          "fog-uniform-left",
		Bounds:      AABB{Min: Vec3{-1.8, -1.0, 2.2}, Max: Vec3{-0.3, 1.0, 4.2}},
		Absorption:  0.05,
		Scattering:  0.12,
		Anisotropy:  0.35,
		CastShadows: true,
		Field:       newUniformField([3]int{6, 8, 8}, 0.25, 2),
	}
	pattern := &Medium{
		ID:          "fog-pattern-right",
		Bounds:      AABB{Min: Vec3{0.3, -1.0, 2.0}, Max: Vec3{1.8, 1.0, 4.0}},
		Absorption:  0.04,
		Scattering:  0.15,
		Anisotropy:  0.55,
		CastShadows: true,
		Field:       newPatternField([3]int{6, 8, 8}, 0.25, rng),
	}
	overlap := &Medium{
		ID:          "fog-overlap-core",
		Bounds:      AABB{Min: Vec3{-0.4, -0.75, 2.5}, Max: Vec3{0.85, 0.75, 3.75}},
		Absorption:  0.06,
		Scattering:  0.10,
		Anisotropy:  0.15,
		CastShadows: true,
		Field:       newUniformField([3]int{5, 6, 5}, 0.25, 1),
	}
	for _, m := range []*Medium{uniform, pattern, overlap} {
		m.Rehash()
	}

	scenario := &Scenario{
		SchemaVersion: SchemaVersion,
		Seed:          seed,
		Width:         48,
		Height:        27,
		FixedScale:    scale,
		Light:         light,
		Recipe:        recipe,
		Cameras:       cameras,
		Media:         []*Medium{uniform, pattern, overlap},
	}

	edit := DensityEdit{
		MediumID: pattern.ID,
		X:        2, Y: 3, Z: 4,
		Reason: "deterministic density edit: raise central voxel",
	}
	edit.OldValue = pattern.Field.Values[pattern.Field.Index(edit.X, edit.Y, edit.Z)]
	edit.NewValue = edit.OldValue + 3
	if edit.NewValue > pattern.Field.MaxValue {
		pattern.Field.MaxValue = edit.NewValue
	}
	scenario.DeclaredEdit = edit

	if err := ValidateScenario(scenario); err != nil {
		return nil, fmt.Errorf("generated scenario failed validation: %w", err)
	}
	if err := buildTruth(scenario); err != nil {
		return nil, err
	}
	return scenario, nil
}

func makeCamera(id string, t float64, pos Vec3) Camera {
	forward := Vec3{X: 0, Y: 0, Z: 1}
	right := Vec3{X: 1, Y: 0, Z: 0}
	up := Vec3{X: 0, Y: 1, Z: 0}
	return Camera{
		ID: id, Time: t, Position: pos,
		Forward: forward, Right: right, Up: up,
		FovY: 50, Near: 0.1, Far: 20,
	}
}

func newUniformField(res [3]int, vs float64, value int32) DensityField {
	n := res[0] * res[1] * res[2]
	values := make([]int32, n)
	for i := range values {
		values[i] = value
	}
	return DensityField{
		Resolution: res, VoxelSize: vs,
		MinValue: 0, MaxValue: 4, Values: values,
	}
}

func newPatternField(res [3]int, vs float64, rng *rand.Rand) DensityField {
	n := res[0] * res[1] * res[2]
	values := make([]int32, n)
	idx := 0
	for z := 0; z < res[2]; z++ {
		for y := 0; y < res[1]; y++ {
			for x := 0; x < res[0]; x++ {
				wave := (x*7 + y*11 + z*13) % 5
				v := int32(1 + (wave+rng.Intn(2))%3)
				values[idx] = v
				idx++
			}
		}
	}
	return DensityField{
		Resolution: res, VoxelSize: vs,
		MinValue: 0, MaxValue: 4, Values: values,
	}
}
