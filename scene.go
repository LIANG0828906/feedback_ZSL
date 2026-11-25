package fog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

const (
	PhaseIsotropic        = "isotropic"
	PhaseHenyeyGreenstein = "henyey_greenstein"

	AlgoVersion = "algo-v1.4.0"
)

type DensityField struct {
	MediumID   string    `json:"medium_id"`
	Resolution [3]int    `json:"resolution"`
	VoxelSize  FixedVec3 `json:"voxel_size"`
	Values     []int     `json:"values"`
}

func (d *DensityField) Index(x, y, z int) int {
	return x + d.Resolution[0]*(y+d.Resolution[1]*z)
}

func (d *DensityField) VoxelBounds(x, y, z int) AABB {
	minSize := d.VoxelSize
	return AABB{
		Min: FixedVec3{
			X: Fixed(int64(x)) * minSize.X,
			Y: Fixed(int64(y)) * minSize.Y,
			Z: Fixed(int64(z)) * minSize.Z,
		},
		Max: FixedVec3{
			X: Fixed(int64(x+1)) * minSize.X,
			Y: Fixed(int64(y+1)) * minSize.Y,
			Z: Fixed(int64(z+1)) * minSize.Z,
		},
	}
}

type Medium struct {
	ID           string        `json:"id"`
	Label        string        `json:"label"`
	Bounds       AABB          `json:"bounds"`
	Absorption   Fixed         `json:"absorption"`
	Scattering   Fixed         `json:"scattering"`
	Anisotropy   float64       `json:"anisotropy"`
	DensityScale float64       `json:"density_scale"`
	Density      *DensityField `json:"density"`
	Priority     int           `json:"priority"`
	Version      int64         `json:"version"`
}

func (m *Medium) ContentHash() string {
	b, _ := json.Marshal(m)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type CameraSample struct {
	LogicalTime int64   `json:"logical_time"`
	Position    Vec3    `json:"position"`
	Forward     Vec3    `json:"forward"`
	Up          Vec3    `json:"up"`
	FovYDegrees float64 `json:"fov_y_degrees"`
	Near        Fixed   `json:"near"`
	Far         Fixed   `json:"far"`
}

type CameraTrack struct {
	ID      string         `json:"id"`
	Version int64          `json:"version"`
	Samples []CameraSample `json:"samples"`
}

func (c *CameraTrack) ContentHash() string {
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type Viewport struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

type Recipe struct {
	ID                   string  `json:"id"`
	Version              int64   `json:"version"`
	TileWidth            int     `json:"tile_width"`
	TileHeight           int     `json:"tile_height"`
	MaxStepsPerRay       int     `json:"max_steps_per_ray"`
	ShadowSamplesPerStep int     `json:"shadow_samples_per_step"`
	ShadowBudgetPerRay   int     `json:"shadow_budget_per_ray"`
	PreviewStepsPerRay   int     `json:"preview_steps_per_ray"`
	PreviewShadowBudget  int     `json:"preview_shadow_budget"`
	PreviewDelayMS       int64   `json:"preview_delay_ms"`
	PhaseFunction        string  `json:"phase_function"`
	StepLength           float64 `json:"step_length,omitempty"`
}

func (r *Recipe) ContentHash() string {
	b, _ := json.Marshal(r)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type Light struct {
	Direction Vec3       `json:"direction"`
	Color     [3]float64 `json:"color"`
	Intensity float64    `json:"intensity"`
	Ambient   [3]float64 `json:"ambient"`
}

type Tolerances struct {
	SegmentTransmittance float64 `json:"segment_transmittance"`
	SegmentScattering    float64 `json:"segment_scattering"`
	PreviewPixel         float64 `json:"preview_pixel"`
	CheckpointPixel      float64 `json:"checkpoint_pixel"`
	Convergence          float64 `json:"convergence"`
}

type Scene struct {
	SchemaVersion string      `json:"schema_version"`
	Seed          int64       `json:"seed"`
	FixedScale    int64       `json:"fixed_scale"`
	Viewport      Viewport    `json:"viewport"`
	ActiveCamera  int         `json:"active_camera"`
	CameraTrack   CameraTrack `json:"camera_track"`
	Media         []*Medium   `json:"media"`
	Light         Light       `json:"light"`
	Recipe        Recipe      `json:"recipe"`
	Tolerances    Tolerances  `json:"tolerances"`
}

func (s *Scene) Medium(id string) *Medium {
	for _, m := range s.Media {
		if m != nil && m.ID == id {
			return m
		}
	}
	return nil
}

func (s *Scene) ActiveSample() CameraSample {
	if s.ActiveCamera >= 0 && s.ActiveCamera < len(s.CameraTrack.Samples) {
		return s.CameraTrack.Samples[s.ActiveCamera]
	}
	return s.CameraTrack.Samples[0]
}

type Ray struct {
	ID     string     `json:"id"`
	Pixel  PixelCoord `json:"pixel"`
	Origin Vec3       `json:"origin"`
	Dir    Vec3       `json:"dir"`
	Near   Fixed      `json:"near"`
	Far    Fixed      `json:"far"`
}

type SegmentEndpoint struct {
	Fixed    Fixed   `json:"fixed"`
	Raw      float64 `json:"raw"`
	Residual float64 `json:"residual"`
}

type RaySegment struct {
	Index       int             `json:"index"`
	MediumIDs   []string        `json:"medium_ids"`
	Near        SegmentEndpoint `json:"near"`
	Far         SegmentEndpoint `json:"far"`
	Length      float64         `json:"length"`
	PriorityKey string          `json:"priority_key"`
}

type SegmentResult struct {
	SegmentIndex         int      `json:"segment_index"`
	MediumIDs            []string `json:"medium_ids"`
	Near                 Fixed    `json:"near"`
	Far                  Fixed    `json:"far"`
	Transmittance        float64  `json:"transmittance"`
	CumulativeT          float64  `json:"cumulative_t"`
	Scattering           float64  `json:"scattering"`
	CumulativeScattering float64  `json:"cumulative_scattering"`
	Steps                int      `json:"steps"`
	ShadowSamples        int      `json:"shadow_samples"`
}

type RayResult struct {
	RayID              string          `json:"ray_id"`
	Pixel              PixelCoord      `json:"pixel"`
	Segments           []SegmentResult `json:"segments"`
	FinalTransmittance float64         `json:"final_transmittance"`
	Scattering         float64         `json:"scattering"`
	Radiance           [3]float64      `json:"radiance"`
	Color              [3]uint8        `json:"color"`
	Steps              int             `json:"steps"`
	ShadowSamples      int             `json:"shadow_samples"`
	ShadowExhausted    bool            `json:"shadow_budget_exhausted"`
	CacheKey           string          `json:"cache_key"`
}

type ExpectedPixel struct {
	X        int        `json:"x"`
	Y        int        `json:"y"`
	Radiance [3]float64 `json:"radiance"`
	Color    [3]uint8   `json:"color"`
}

type SegmentTruth struct {
	SegmentIndex         int      `json:"segment_index"`
	MediumIDs            []string `json:"medium_ids"`
	Near                 Fixed    `json:"near"`
	Far                  Fixed    `json:"far"`
	Transmittance        float64  `json:"transmittance"`
	CumulativeT          float64  `json:"cumulative_t"`
	Scattering           float64  `json:"scattering"`
	CumulativeScattering float64  `json:"cumulative_scattering"`
}

type RayTruth struct {
	RayID              string         `json:"ray_id"`
	Pixel              PixelCoord     `json:"pixel"`
	Segments           []SegmentTruth `json:"segments"`
	FinalTransmittance float64        `json:"final_transmittance"`
	Scattering         float64        `json:"scattering"`
	Radiance           [3]float64     `json:"radiance"`
	Color              [3]uint8       `json:"color"`
}

type TileTruth struct {
	Tile   TileCoord       `json:"tile"`
	Pixels []ExpectedPixel `json:"pixels"`
}

type TruthBundle struct {
	Rays  []RayTruth  `json:"rays"`
	Tiles []TileTruth `json:"tiles"`
}

type GeneratedSample struct {
	Scene Scene       `json:"scene"`
	Truth TruthBundle `json:"truth"`
}
