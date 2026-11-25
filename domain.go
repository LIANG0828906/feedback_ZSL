package fogkernel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
)

type Vec3I struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
	Z int64 `json:"z"`
}

type Vec3 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

func (v Vec3I) ToMeters() Vec3 {
	return Vec3{
		X: float64(v.X) / float64(DistanceScale),
		Y: float64(v.Y) / float64(DistanceScale),
		Z: float64(v.Z) / float64(DistanceScale),
	}
}

func (v Vec3) Add(o Vec3) Vec3    { return Vec3{v.X + o.X, v.Y + o.Y, v.Z + o.Z} }
func (v Vec3) Sub(o Vec3) Vec3    { return Vec3{v.X - o.X, v.Y - o.Y, v.Z - o.Z} }
func (v Vec3) Mul(s float64) Vec3 { return Vec3{v.X * s, v.Y * s, v.Z * s} }
func (v Vec3) Dot(o Vec3) float64 { return v.X*o.X + v.Y*o.Y + v.Z*o.Z }

func (v Vec3) Normalize() Vec3 {
	n := math.Sqrt(v.Dot(v))
	if n == 0 {
		return Vec3{}
	}
	return v.Mul(1 / n)
}

type DensityField struct {
	Size   Vec3I `json:"size"`
	Values []int `json:"values"`
	Max    int   `json:"max"`
}

func (d DensityField) Index(x, y, z int) int {
	return x + int(d.Size.X)*(y+int(d.Size.Y)*z)
}

func (d DensityField) Get(x, y, z int) int {
	return d.Values[d.Index(x, y, z)]
}

type PhaseFunction struct {
	Type string `json:"type"`
	GQ   int64  `json:"g_q"`
}

type Medium struct {
	ID             string        `json:"id"`
	MinMicro       Vec3I         `json:"min_micro"`
	MaxMicro       Vec3I         `json:"max_micro"`
	VoxelMicro     int64         `json:"voxel_micro"`
	Density        DensityField  `json:"density"`
	SigmaAQ        int64         `json:"sigma_a_q"`
	SigmaSQ        int64         `json:"sigma_s_q"`
	Phase          PhaseFunction `json:"phase"`
	SourceVersions []string      `json:"source_versions,omitempty"`
}

func (m *Medium) SizeMicro() Vec3I {
	return Vec3I{X: m.MaxMicro.X - m.MinMicro.X, Y: m.MaxMicro.Y - m.MinMicro.Y, Z: m.MaxMicro.Z - m.MinMicro.Z}
}

func (m *Medium) SigmaA() float64 { return QToFloat(m.SigmaAQ, CoeffScale) }
func (m *Medium) SigmaS() float64 { return QToFloat(m.SigmaSQ, CoeffScale) }
func (m *Medium) SigmaT() float64 { return m.SigmaA() + m.SigmaS() }
func (m *Medium) G() float64      { return QToFloat(m.Phase.GQ, CoeffScale) }

type Recipe struct {
	ChunkSizeMicro          int64 `json:"chunk_size_micro"`
	BlockSampleLimit        int   `json:"block_sample_limit"`
	TransmittanceToleranceQ int64 `json:"transmittance_tolerance_q"`
	TerminationThresholdQ   int64 `json:"termination_threshold_q"`
	PreviewStride           int   `json:"preview_stride"`
	PreviewErrorBoundQ      int64 `json:"preview_error_bound_q"`
	DefaultWorkers          int   `json:"default_workers"`
	DefaultBuffer           int   `json:"default_buffer"`
}

func (r Recipe) ChunkMeters() float64 {
	return float64(r.ChunkSizeMicro) / float64(DistanceScale)
}

func (r Recipe) TerminationThreshold() float64 {
	return float64(r.TerminationThresholdQ) / float64(TransmittanceQ)
}

type CameraSample struct {
	LogicalTime   int64 `json:"logical_time"`
	PositionMicro Vec3I `json:"position_micro"`
	YawMilliDeg   int64 `json:"yaw_milli_deg"`
	PitchMilliDeg int64 `json:"pitch_milli_deg"`
	FovYMilliDeg  int64 `json:"fov_y_milli_deg"`
	AspectQ       int64 `json:"aspect_q"`
}

type Ray struct {
	Index       int     `json:"index"`
	CameraIndex int     `json:"camera_index"`
	RayInCamera int     `json:"ray_in_camera"`
	ID          string  `json:"id"`
	Origin      Vec3    `json:"origin"`
	Direction   Vec3    `json:"direction"`
	ScreenX     float64 `json:"screen_x"`
	ScreenY     float64 `json:"screen_y"`
}

type Scene struct {
	ProjectID    string         `json:"project_id"`
	Seed         int64          `json:"seed"`
	Recipe       Recipe         `json:"recipe"`
	LightVectorQ [3]int64       `json:"light_vector_q"`
	LightDir     Vec3           `json:"light_dir"`
	Media        []*Medium      `json:"media"`
	Cameras      []CameraSample `json:"cameras"`
	Rays         []Ray          `json:"rays"`
}

func (s *Scene) Medium(id string) (*Medium, int, error) {
	for i, m := range s.Media {
		if m.ID == id {
			return m, i, nil
		}
	}
	return nil, -1, NewBizError(CodeEditTarget, "$.media", "medium %q not found", id).
		WithContext("medium_id", id)
}

func (s *Scene) Clone() *Scene {
	if s == nil {
		return nil
	}
	cp := *s
	cp.Media = make([]*Medium, len(s.Media))
	for i, m := range s.Media {
		mc := *m
		mc.Density.Values = append([]int(nil), m.Density.Values...)
		cp.Media[i] = &mc
	}
	cp.Cameras = append([]CameraSample(nil), s.Cameras...)
	cp.Rays = append([]Ray(nil), s.Rays...)
	return &cp
}

// Version is a canonical hash of all numerical inputs. Schema envelope fields
// and JSON field names intentionally do not affect it.
func (s *Scene) Version() string {
	cp := s.Clone()
	cp.Rays = nil
	data, _ := json.Marshal(cp)
	sum := sha256.Sum256(data)
	return "scene-" + hex.EncodeToString(sum[:16])
}

func (s *Scene) SortedMedia() []*Medium {
	out := append([]*Medium(nil), s.Media...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func BuildRays(scene *Scene) []Ray {
	layout := []struct{ x, y float64 }{
		{0, 0}, {-.75, .65}, {.75, .65}, {-.75, -.65}, {.75, -.65},
	}
	rays := make([]Ray, 0, len(scene.Cameras)*len(layout))
	for ci, cam := range scene.Cameras {
		yaw := float64(cam.YawMilliDeg) / 1000 * math.Pi / 180
		pitch := float64(cam.PitchMilliDeg) / 1000 * math.Pi / 180
		fovY := float64(cam.FovYMilliDeg) / 1000 * math.Pi / 180
		aspect := float64(cam.AspectQ) / float64(CoeffScale)
		if aspect == 0 {
			aspect = 1
		}
		for ri, ndc := range layout {
			forward := Vec3{X: math.Sin(yaw) * math.Cos(pitch), Y: math.Sin(pitch), Z: math.Cos(yaw) * math.Cos(pitch)}.Normalize()
			right := Vec3{X: math.Cos(yaw), Y: 0, Z: -math.Sin(yaw)}.Normalize()
			up := Vec3{X: -math.Sin(yaw) * math.Sin(pitch), Y: math.Cos(pitch), Z: -math.Cos(yaw) * math.Sin(pitch)}.Normalize()
			tanY := math.Tan(fovY / 2)
			d := forward.
				Add(right.Mul(ndc.x * tanY * aspect)).
				Add(up.Mul(ndc.y * tanY)).
				Normalize()
			origin := cam.PositionMicro.ToMeters()
			id := "r-" + strconvI(ci) + "-" + strconvI(ri)
			rays = append(rays, Ray{
				Index: len(rays), CameraIndex: ci, RayInCamera: ri, ID: id,
				Origin: origin, Direction: d, ScreenX: ndc.x, ScreenY: ndc.y,
			})
		}
	}
	return rays
}

func strconvI(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func HGPhase(g, cosTheta float64) float64 {
	g2 := g * g
	den := 1 + g2 - 2*g*cosTheta
	if den <= 1e-12 {
		if g > 0.999999 {
			return 1e6
		}
		den = 1e-12
	}
	return (1 - g2) / math.Pow(den, 1.5)
}
