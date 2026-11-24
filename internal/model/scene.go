package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

const (
	SceneFormat = "volume-diagnostic/scene/v1"
	TruthFormat = "volume-diagnostic/truth/v1"
	GeneratorID = "volume-diagnostic/deterministic-generator/v1"
)

type Viewport struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

type BlockSize struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

type Camera struct {
	ID       string  `json:"id"`
	Position Vec3    `json:"position"`
	Target   Vec3    `json:"target"`
	Up       Vec3    `json:"up"`
	FovYDeg  float64 `json:"fov_y_deg"`
	Near     float64 `json:"near"`
	Far      float64 `json:"far"`
}

type DepthMesh struct {
	ID      string      `json:"id"`
	Origin  Vec3        `json:"origin"`
	AxisU   Vec3        `json:"axis_u"`
	AxisV   Vec3        `json:"axis_v"`
	Normal  Vec3        `json:"normal"`
	Columns int         `json:"columns"`
	Rows    int         `json:"rows"`
	Depths  [][]float64 `json:"depths"`
	Color   Color       `json:"color"`
}

type Light struct {
	ID           string     `json:"id"`
	Type         string     `json:"type"`
	Position     Vec3       `json:"position"`
	Direction    Vec3       `json:"direction"`
	Intensity    Color      `json:"intensity"`
	Radius       float64    `json:"radius"`
	Anisotropy   float64    `json:"anisotropy"`
	OccluderIDs  []string   `json:"occluder_ids"`
	ConeInnerDeg float64    `json:"cone_inner_deg,omitempty"`
	ConeOuterDeg float64    `json:"cone_outer_deg,omitempty"`
	HalfExtents  [2]float64 `json:"half_extents,omitempty"`
}

type Medium struct {
	Extinction Color   `json:"extinction"`
	Scattering Color   `json:"scattering"`
	PhaseModel string  `json:"phase_model"`
	PhaseG     float64 `json:"phase_g"`
}

type Scene struct {
	FormatVersion  string      `json:"format_version"`
	Seed           int64       `json:"seed"`
	Viewport       Viewport    `json:"viewport"`
	BlockSize      BlockSize   `json:"block_size"`
	ActiveCameraID string      `json:"active_camera_id"`
	CameraPath     []Camera    `json:"camera_path"`
	Geometry       []DepthMesh `json:"geometry"`
	Lights         []Light     `json:"lights"`
	Medium         Medium      `json:"medium"`
}

type Tolerance struct {
	TransmittanceAbs float64 `json:"transmittance_abs"`
	ScatterAbs       float64 `json:"scatter_abs"`
	PixelAbs         float64 `json:"pixel_abs"`
	Relative         float64 `json:"relative"`
}

type RayTruth struct {
	ID            string  `json:"id"`
	CameraID      string  `json:"camera_id"`
	PixelX        int     `json:"pixel_x"`
	PixelY        int     `json:"pixel_y"`
	Origin        Vec3    `json:"origin"`
	Direction     Vec3    `json:"direction"`
	TNear         float64 `json:"t_near"`
	TFar          float64 `json:"t_far"`
	Transmittance Color   `json:"transmittance"`
	Scatter       Color   `json:"scatter"`
}

type PixelTruth struct {
	CameraID string `json:"camera_id"`
	X        int    `json:"x"`
	Y        int    `json:"y"`
	Volume   Color  `json:"volume"`
}

type Truth struct {
	FormatVersion string       `json:"format_version"`
	Tolerance     Tolerance    `json:"tolerance"`
	Rays          []RayTruth   `json:"rays"`
	Pixels        []PixelTruth `json:"pixels"`
}

type SourceManifest struct {
	GeneratorID string `json:"generator_id"`
	Seed        int64  `json:"seed"`
	SourceHash  string `json:"source_hash"`
	SceneHash   string `json:"scene_hash"`
}

type Bundle struct {
	Scene      *Scene
	Truth      *Truth
	SourceHash string
	SceneHash  string
	Dir        string
}

type Versions struct {
	SceneHash       string            `json:"scene_hash"`
	MediumVersion   string            `json:"medium_version"`
	GeometryVersion string            `json:"geometry_version"`
	LightVersions   map[string]string `json:"light_versions"`
	CameraVersion   string            `json:"camera_version"`
}

func CanonicalJSON(v any) ([]byte, error) { return json.Marshal(v) }

func HashBytes(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func HashValue(v any) string {
	b, err := CanonicalJSON(v)
	if err != nil {
		return ""
	}
	return HashBytes(b)
}

func HashSources(scene, truth []byte) string {
	// Length framing prevents ambiguous concatenations.
	return HashBytes([]byte("scene\x00"), scene, []byte("\x00truth\x00"), truth)
}

func (s *Scene) ComputeHash() string { return HashValue(s) }

func (s *Scene) Versions() Versions {
	lights := make(map[string]string, len(s.Lights))
	for i := range s.Lights {
		l := s.Lights[i]
		lights[l.ID] = HashValue(l)
	}
	return Versions{
		SceneHash:       s.ComputeHash(),
		MediumVersion:   HashValue(s.Medium),
		GeometryVersion: HashValue(s.Geometry),
		LightVersions:   lights,
		CameraVersion: HashValue(struct {
			Active string   `json:"active"`
			Path   []Camera `json:"path"`
		}{s.ActiveCameraID, s.CameraPath}),
	}
}

func (s *Scene) ActiveCamera() (*Camera, int) {
	for i := range s.CameraPath {
		if s.CameraPath[i].ID == s.ActiveCameraID {
			return &s.CameraPath[i], i
		}
	}
	return nil, -1
}

func (s *Scene) FindLight(id string) (*Light, int) {
	for i := range s.Lights {
		if s.Lights[i].ID == id {
			return &s.Lights[i], i
		}
	}
	return nil, -1
}

func (s *Scene) FindMesh(id string) (*DepthMesh, int) {
	for i := range s.Geometry {
		if s.Geometry[i].ID == id {
			return &s.Geometry[i], i
		}
	}
	return nil, -1
}

func (s *Scene) BlockRows() int {
	if s.BlockSize.Height <= 0 {
		return 0
	}
	return (s.Viewport.Height + s.BlockSize.Height - 1) / s.BlockSize.Height
}

func (s *Scene) BlockCols() int {
	if s.BlockSize.Width <= 0 {
		return 0
	}
	return (s.Viewport.Width + s.BlockSize.Width - 1) / s.BlockSize.Width
}

func (s *Scene) Clone() *Scene {
	b, err := json.Marshal(s)
	if err != nil {
		return nil
	}
	var out Scene
	if err := json.Unmarshal(b, &out); err != nil {
		return nil
	}
	return &out
}
