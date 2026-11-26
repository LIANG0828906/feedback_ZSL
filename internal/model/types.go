package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

const (
	FixedScale         int64 = 1_000_000
	DensityMax               = 255
	AlgorithmVersion         = "volume-kernel/1"
	CacheFormatVersion       = "block-cache/1"
)

type Vec3 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

func (v Vec3) Add(o Vec3) Vec3    { return Vec3{v.X + o.X, v.Y + o.Y, v.Z + o.Z} }
func (v Vec3) Sub(o Vec3) Vec3    { return Vec3{v.X - o.X, v.Y - o.Y, v.Z - o.Z} }
func (v Vec3) Mul(s float64) Vec3 { return Vec3{v.X * s, v.Y * s, v.Z * s} }
func (v Vec3) Dot(o Vec3) float64 { return v.X*o.X + v.Y*o.Y + v.Z*o.Z }
func (v Vec3) Length() float64    { return math.Sqrt(v.Dot(v)) }
func (v Vec3) IsFinite() bool     { return isFinite(v.X) && isFinite(v.Y) && isFinite(v.Z) }
func (v Vec3) Cross(o Vec3) Vec3 {
	return Vec3{
		X: v.Y*o.Z - v.Z*o.Y,
		Y: v.Z*o.X - v.X*o.Z,
		Z: v.X*o.Y - v.Y*o.X,
	}
}
func (v Vec3) Normalize() Vec3 {
	n := v.Length()
	if n == 0 || math.IsNaN(n) || math.IsInf(n, 0) {
		return Vec3{}
	}
	return v.Mul(1 / n)
}

type AABB struct {
	Min Vec3 `json:"min"`
	Max Vec3 `json:"max"`
}

func (b AABB) Extent() Vec3 { return b.Max.Sub(b.Min) }

func (b AABB) Center() Vec3 {
	return Vec3{(b.Min.X + b.Max.X) * 0.5, (b.Min.Y + b.Max.Y) * 0.5, (b.Min.Z + b.Max.Z) * 0.5}
}

func (b AABB) Contains(p Vec3) bool {
	return p.X >= b.Min.X && p.X <= b.Max.X &&
		p.Y >= b.Min.Y && p.Y <= b.Max.Y &&
		p.Z >= b.Min.Z && p.Z <= b.Max.Z
}

func (b AABB) Valid() bool {
	return b.Min.IsFinite() && b.Max.IsFinite() &&
		b.Min.X <= b.Max.X && b.Min.Y <= b.Max.Y && b.Min.Z <= b.Max.Z
}

type DensityField struct {
	NX     int     `json:"nx"`
	NY     int     `json:"ny"`
	NZ     int     `json:"nz"`
	Values []uint8 `json:"values"`
}

func (d DensityField) Len() int { return d.NX * d.NY * d.NZ }

func (d DensityField) At(i, j, k int) float64 {
	if i < 0 || j < 0 || k < 0 || i >= d.NX || j >= d.NY || k >= d.NZ {
		return 0
	}
	return float64(d.Values[(k*d.NY+j)*d.NX+i]) / DensityMax
}

func (d DensityField) AtLocal(u, v, w float64) float64 {
	if u < 0 || v < 0 || w < 0 || u > 1 || v > 1 || w > 1 {
		return 0
	}
	i := int(u * float64(d.NX))
	j := int(v * float64(d.NY))
	k := int(w * float64(d.NZ))
	if i >= d.NX {
		i = d.NX - 1
	}
	if j >= d.NY {
		j = d.NY - 1
	}
	if k >= d.NZ {
		k = d.NZ - 1
	}
	return d.At(i, j, k)
}

type Medium struct {
	ID              string       `json:"id"`
	Bounds          AABB         `json:"bounds"`
	Absorption      float64      `json:"absorption"`
	Scattering      float64      `json:"scattering"`
	Anisotropy      float64      `json:"anisotropy"`
	Density         DensityField `json:"density"`
	MaterialVersion string       `json:"material_version"`
}

func (m Medium) Extinction() float64 { return m.Absorption + m.Scattering }

func (m Medium) DensityAt(p Vec3) float64 {
	if !m.Bounds.Contains(p) {
		return 0
	}
	e := m.Bounds.Extent()
	u := (p.X - m.Bounds.Min.X) / e.X
	v := (p.Y - m.Bounds.Min.Y) / e.Y
	w := (p.Z - m.Bounds.Min.Z) / e.Z
	return m.Density.AtLocal(u, v, w)
}

type CameraSample struct {
	ID       string  `json:"id"`
	Time     float64 `json:"time"`
	Position Vec3    `json:"position"`
	Target   Vec3    `json:"target"`
	Up       Vec3    `json:"up"`
	FOVDeg   float64 `json:"fov_deg"`
	Aspect   float64 `json:"aspect"`
}

type CameraTrack struct {
	ID      string         `json:"id"`
	Samples []CameraSample `json:"samples"`
}

type PhaseType string

const (
	PhaseIsotropic PhaseType = "isotropic"
	PhaseHG        PhaseType = "henyey-greenstein"
)

type Recipe struct {
	Version              string    `json:"version"`
	Steps                int       `json:"steps"`
	Phase                PhaseType `json:"phase"`
	ShadowSamples        int       `json:"shadow_samples"`
	ShadowBudget         int       `json:"shadow_budget"`
	BlockWidth           int       `json:"block_width"`
	BlockHeight          int       `json:"block_height"`
	MaxParallelBlocks    int       `json:"max_parallel_blocks"`
	MaxRaysPerBlock      int       `json:"max_rays_per_block"`
	PreviewSteps         int       `json:"preview_steps"`
	PreviewShadowSamples int       `json:"preview_shadow_samples"`
	PreviewLatencyMS     int       `json:"preview_latency_ms"`
	PixelTolerance       float64   `json:"pixel_tolerance"`
	OverlapMode          string    `json:"overlap_mode"`
}

type Viewport struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

type DirectionalLight struct {
	Direction         Vec3    `json:"direction"`
	Radiance          float64 `json:"radiance"`
	ShadowTau         float64 `json:"shadow_tau"`
	ShadowProfileGain float64 `json:"shadow_profile_gain"`
}

type Scene struct {
	ID               string           `json:"id"`
	Version          string           `json:"version"`
	Seed             int64            `json:"seed"`
	FixedScale       int64            `json:"fixed_scale"`
	CoefficientScale int64            `json:"coefficient_scale"`
	Media            []Medium         `json:"media"`
	CameraTrack      CameraTrack      `json:"camera_track"`
	Recipe           Recipe           `json:"recipe"`
	Viewport         Viewport         `json:"viewport"`
	Light            DirectionalLight `json:"light"`
}

type SegmentTruth struct {
	Order             int      `json:"order"`
	T0Ticks           int64    `json:"t0_ticks"`
	T1Ticks           int64    `json:"t1_ticks"`
	T0ResidualTicks   float64  `json:"t0_residual_ticks"`
	T1ResidualTicks   float64  `json:"t1_residual_ticks"`
	MediaIDs          []string `json:"media_ids"`
	Transmittance     float64  `json:"transmittance"`
	ScatteredRadiance float64  `json:"scattered_radiance"`
}

type TruthRay struct {
	CameraIndex       int            `json:"camera_index"`
	PixelX            int            `json:"pixel_x"`
	PixelY            int            `json:"pixel_y"`
	RayID             string         `json:"ray_id"`
	Transmittance     float64        `json:"transmittance"`
	ScatteredRadiance float64        `json:"scattered_radiance"`
	Segments          []SegmentTruth `json:"segments"`
}

type TruthManifest struct {
	SceneHash        string     `json:"scene_hash"`
	SceneVersion     string     `json:"scene_version"`
	CameraVersion    string     `json:"camera_version"`
	RecipeVersion    string     `json:"recipe_version"`
	AlgorithmVersion string     `json:"algorithm_version"`
	Rays             []TruthRay `json:"rays"`
}

type RaySegment struct {
	Order             int      `json:"order"`
	T0Ticks           int64    `json:"t0_ticks"`
	T1Ticks           int64    `json:"t1_ticks"`
	T0ResidualTicks   float64  `json:"t0_residual_ticks"`
	T1ResidualTicks   float64  `json:"t1_residual_ticks"`
	MediaIDs          []string `json:"media_ids"`
	Steps             int      `json:"steps"`
	ShadowSamples     int      `json:"shadow_samples"`
	Transmittance     float64  `json:"transmittance"`
	ScatteredRadiance float64  `json:"scattered_radiance"`
}

type PixelResult struct {
	X                 int          `json:"x"`
	Y                 int          `json:"y"`
	RayID             string       `json:"ray_id"`
	Transmittance     float64      `json:"transmittance"`
	ScatteredRadiance float64      `json:"scattered_radiance"`
	Steps             int          `json:"steps"`
	ShadowSamples     int          `json:"shadow_samples"`
	Segments          []RaySegment `json:"segments"`
}

type BlockResult struct {
	X                int           `json:"x"`
	Y                int           `json:"y"`
	Index            int           `json:"index"`
	Pixels           []PixelResult `json:"pixels"`
	TransmittanceAvg float64       `json:"transmittance_avg"`
	ScatterAvg       float64       `json:"scatter_avg"`
	Steps            int           `json:"steps"`
	ShadowSamples    int           `json:"shadow_samples"`
	CacheKey         string        `json:"cache_key"`
	ContentHash      string        `json:"content_hash"`
	RayDigest        string        `json:"ray_digest"`
}

type QueueEvent struct {
	BlockIndex int     `json:"block_index"`
	Reason     string  `json:"reason"`
	WaitMS     float64 `json:"wait_ms"`
	At         string  `json:"at"`
}

type Frame struct {
	Mode             string        `json:"mode"`
	SnapshotVersion  string        `json:"snapshot_version"`
	CameraVersion    string        `json:"camera_version"`
	RecipeVersion    string        `json:"recipe_version"`
	AlgorithmVersion string        `json:"algorithm_version"`
	Viewport         Viewport      `json:"viewport"`
	Pixels           []PixelResult `json:"pixels"`
	Blocks           []BlockResult `json:"blocks"`
	ElapsedMS        float64       `json:"elapsed_ms"`
	StepBudget       int           `json:"step_budget"`
	ShadowBudget     int           `json:"shadow_budget"`
	ActualSteps      int           `json:"actual_steps"`
	ActualShadows    int           `json:"actual_shadows"`
	Queue            []QueueEvent  `json:"queue"`
}

func (f Frame) Pixel(x, y int) (PixelResult, bool) {
	if x < 0 || y < 0 || x >= f.Viewport.Width || y >= f.Viewport.Height {
		return PixelResult{}, false
	}
	idx := y*f.Viewport.Width + x
	if idx >= len(f.Pixels) {
		return PixelResult{}, false
	}
	return f.Pixels[idx], true
}

type SceneSnapshot struct {
	Scene         Scene
	Version       string
	CameraVersion string
	RecipeVersion string
	SceneHash     string
}

func NewSnapshot(scene Scene) (SceneSnapshot, error) {
	h, err := HashScene(scene)
	if err != nil {
		return SceneSnapshot{}, err
	}
	ch, err := HashValue(scene.CameraTrack)
	if err != nil {
		return SceneSnapshot{}, err
	}
	rh, err := HashValue(scene.Recipe)
	if err != nil {
		return SceneSnapshot{}, err
	}
	return SceneSnapshot{
		Scene:         scene,
		Version:       "snap-" + h[:16],
		CameraVersion: "cam-" + ch[:16],
		RecipeVersion: scene.Recipe.Version + "@" + rh[:12],
		SceneHash:     h,
	}, nil
}

func HashScene(s Scene) (string, error) {
	cp := s
	cp.Media = append([]Medium(nil), s.Media...)
	sort.Slice(cp.Media, func(i, j int) bool { return cp.Media[i].ID < cp.Media[j].ID })
	return HashValue(cp)
}

func HashValue(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func isFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func JoinIDs(ids []string) string {
	cp := append([]string(nil), ids...)
	sort.Strings(cp)
	return strings.Join(cp, ",")
}

func ClipUnit(v float64) int {
	if v <= 0 {
		return 0
	}
	if v >= 1 {
		return DensityMax
	}
	return int(math.Round(v * DensityMax))
}

func FormatFloat(v float64) string { return fmt.Sprintf("%.9g", v) }
