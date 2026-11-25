package scene

import "fmt"

const (
	SchemaVersion            = "lightprobe.scene.v1"
	SamplingParameterVersion = "sampling-v1"
	AlgorithmVersion         = "recon-v1"
	DefaultFixedPointScale   = int64(1_000_000)
	DefaultSeed              = int64(424242)
	SHCoefficientCount       = 9
	ColorChannelCount        = 3
)

type Vec3 [3]float64

func (v Vec3) X() float64 { return v[0] }
func (v Vec3) Y() float64 { return v[1] }
func (v Vec3) Z() float64 { return v[2] }

func (v Vec3) String() string {
	return fmt.Sprintf("[%.6g %.6g %.6g]", v[0], v[1], v[2])
}

type AABB struct {
	Min Vec3 `json:"min"`
	Max Vec3 `json:"max"`
}

type Resolution struct {
	X int `json:"x"`
	Y int `json:"y"`
	Z int `json:"z"`
}

func (r Resolution) Product() int { return r.X * r.Y * r.Z }

type Room struct {
	ID                 string     `json:"id"`
	Index              int        `json:"index"`
	Name               string     `json:"name"`
	Bounds             AABB       `json:"bounds"`
	Resolution         Resolution `json:"resolution"`
	GridOffset         Vec3       `json:"grid_offset"`
	BoundaryBlendWidth float64    `json:"boundary_blend_width"`
	BlendRule          string     `json:"blend_rule"`
	AnalyticBias       Vec3       `json:"analytic_bias"`
}

type Probe struct {
	ID                string `json:"id"`
	RoomID            string `json:"room_id"`
	I                 int    `json:"i"`
	J                 int    `json:"j"`
	K                 int    `json:"k"`
	Position          Vec3   `json:"position"`
	Radiance          Vec3   `json:"radiance"`
	VisibilitySamples int    `json:"visibility_samples"`
}

type PhotonCell struct {
	Cell   [3]int      `json:"cell"`
	Center Vec3        `json:"center"`
	Coeff  [9][3]int64 `json:"coeff_fixed"`
}

type PhotonGrid struct {
	GridMin  Vec3         `json:"grid_min"`
	CellSize Vec3         `json:"cell_size"`
	Dims     [3]int       `json:"dims"`
	Scale    int64        `json:"fixed_scale"`
	Cells    []PhotonCell `json:"cells"`
}

type BoundaryRule struct {
	ID         string  `json:"id"`
	RoomA      string  `json:"room_a"`
	RoomB      string  `json:"room_b"`
	Axis       int     `json:"axis"`
	Position   float64 `json:"position"`
	BlendWidth float64 `json:"blend_width"`
	Rule       string  `json:"rule"`
}

type Pixel struct {
	ID         string  `json:"id"`
	Position   Vec3    `json:"position"`
	RoomID     string  `json:"room_id"`
	BoundaryID string  `json:"boundary_id,omitempty"`
	BaseColor  Vec3    `json:"base_color"`
	Ambient    Vec3    `json:"ambient"`
	Exposure   float64 `json:"exposure"`
}

type Tolerances struct {
	Radiance float64 `json:"radiance"`
	Color    float64 `json:"color"`
	Jump     float64 `json:"jump"`
}

type ProbeTruth struct {
	ProbeID  string `json:"probe_id"`
	Radiance Vec3   `json:"radiance"`
}

type PixelTruth struct {
	PixelID string `json:"pixel_id"`
	Color   Vec3   `json:"color"`
}

type JumpTruth struct {
	BoundaryID string `json:"boundary_id"`
	Position   Vec3   `json:"position"`
	Jump       Vec3   `json:"jump"`
}

type Truth struct {
	Probes []ProbeTruth `json:"probes"`
	Pixels []PixelTruth `json:"pixels"`
	Jumps  []JumpTruth  `json:"jumps"`
}

type Scene struct {
	SchemaVersion            string         `json:"schema_version"`
	Seed                     int64          `json:"seed"`
	SamplingParameterVersion string         `json:"sampling_parameter_version"`
	AlgorithmVersion         string         `json:"algorithm_version"`
	Tolerance                Tolerances     `json:"tolerance"`
	Rooms                    []Room         `json:"rooms"`
	Probes                   []Probe        `json:"probes"`
	Boundaries               []BoundaryRule `json:"boundaries"`
	PhotonGrid               PhotonGrid     `json:"photon_grid"`
	Pixels                   []Pixel        `json:"pixels"`
	Truth                    Truth          `json:"truth"`
}

func (s *Scene) RoomByID(id string) (Room, int, bool) {
	for i, room := range s.Rooms {
		if room.ID == id {
			return room, i, true
		}
	}
	return Room{}, -1, false
}

func (s *Scene) ProbeByID(id string) (Probe, int, bool) {
	for i, probe := range s.Probes {
		if probe.ID == id {
			return probe, i, true
		}
	}
	return Probe{}, -1, false
}
