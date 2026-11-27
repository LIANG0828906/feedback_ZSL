package volumefog

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
)

const (
	SchemaVersion          = "volumefog/v1"
	AlgorithmVersion       = "vf-segment-v1"
	FixedScale       int64 = 1024
	CoeffScale       int64 = 1_000_000
	MaxDensity             = 255
	MinVoxelQ        int64 = 256  // 0.25 world units
	MaxVoxelQ        int64 = 4096 // 4.0 world units
)

type Vec3i [3]int64

func (v Vec3i) Add(o Vec3i) Vec3i { return Vec3i{v[0] + o[0], v[1] + o[1], v[2] + o[2]} }
func (v Vec3i) Sub(o Vec3i) Vec3i { return Vec3i{v[0] - o[0], v[1] - o[1], v[2] - o[2]} }

// Medium is an axis-aligned participating medium with an integer voxel field.
type Medium struct {
	ID              string `json:"id"`
	MinQ            Vec3i  `json:"min_q"`
	MaxQ            Vec3i  `json:"max_q"`
	DensityRes      [3]int `json:"density_res"`
	Density         []int  `json:"-"`
	DensityChecksum string `json:"density_checksum"`
	AbsorptionQ     int64  `json:"absorption_q"`
	ScatteringQ     int64  `json:"scattering_q"`
	AnisotropyQ     int64  `json:"anisotropy_q"`
	densityRaw      []byte
}

func (m *Medium) ExtentQ() Vec3i { return m.MaxQ.Sub(m.MinQ) }

func (m *Medium) VoxelCount() int {
	if len(m.DensityRes) != 3 {
		return 0
	}
	return m.DensityRes[0] * m.DensityRes[1] * m.DensityRes[2]
}

func (m *Medium) DensityIndex(x, y, z int) int {
	rx, ry := m.DensityRes[0], m.DensityRes[1]
	return (z*ry+y)*rx + x
}

func (m *Medium) DensityXYZ(index int) (int, int, int, bool) {
	n := m.VoxelCount()
	if index < 0 || index >= n || len(m.DensityRes) != 3 || m.DensityRes[0] <= 0 || m.DensityRes[1] <= 0 {
		return 0, 0, 0, false
	}
	rx, ry := m.DensityRes[0], m.DensityRes[1]
	z := index / (rx * ry)
	rem := index % (rx * ry)
	y := rem / rx
	x := rem % rx
	return x, y, z, true
}

func (m *Medium) Contains(p Vec3i) bool {
	return p[0] >= m.MinQ[0] && p[0] < m.MaxQ[0] &&
		p[1] >= m.MinQ[1] && p[1] < m.MaxQ[1] &&
		p[2] >= m.MinQ[2] && p[2] < m.MaxQ[2]
}

func (m *Medium) LocalDensityIndex(p Vec3i) (int, bool) {
	if !m.Contains(p) {
		return 0, false
	}
	ext := m.ExtentQ()
	var idx [3]int
	for a := 0; a < 3; a++ {
		if m.DensityRes[a] <= 0 || ext[a] <= 0 {
			return 0, false
		}
		v := (p[a] - m.MinQ[a]) * int64(m.DensityRes[a]) / ext[a]
		if v < 0 {
			v = 0
		}
		if v >= int64(m.DensityRes[a]) {
			v = int64(m.DensityRes[a] - 1)
		}
		idx[a] = int(v)
	}
	return m.DensityIndex(idx[0], idx[1], idx[2]), true
}

func (m *Medium) DensityAt(p Vec3i) (float64, int, bool) {
	idx, ok := m.LocalDensityIndex(p)
	if !ok || idx < 0 || idx >= len(m.Density) {
		return 0, idx, false
	}
	return float64(m.Density[idx]) / float64(MaxDensity), idx, true
}

func (m *Medium) Absorption() float64 { return float64(m.AbsorptionQ) / float64(CoeffScale) }
func (m *Medium) Scattering() float64 { return float64(m.ScatteringQ) / float64(CoeffScale) }
func (m *Medium) Anisotropy() float64 { return float64(m.AnisotropyQ) / float64(CoeffScale) }

func EncodeDensity(density []int) []byte {
	raw := make([]byte, 4*len(density))
	for i, v := range density {
		binary.LittleEndian.PutUint32(raw[i*4:], uint32(int32(v)))
	}
	return raw
}

func DecodeDensity(raw []byte) ([]int, error) {
	if len(raw)%4 != 0 {
		return nil, fmt.Errorf("density byte length %d is not a multiple of 4", len(raw))
	}
	out := make([]int, len(raw)/4)
	for i := range out {
		out[i] = int(int32(binary.LittleEndian.Uint32(raw[i*4:])))
	}
	return out, nil
}

func DensityChecksum(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (m *Medium) RefreshDensityEncoding() {
	m.densityRaw = EncodeDensity(m.Density)
	m.DensityChecksum = DensityChecksum(m.densityRaw)
}

func (m *Medium) DensityRawCopy() []byte {
	if m.densityRaw == nil {
		m.densityRaw = EncodeDensity(m.Density)
	}
	return append([]byte(nil), m.densityRaw...)
}

func (m *Medium) setDensityRaw(raw []byte) { m.densityRaw = append([]byte(nil), raw...) }

// CameraSample is an orthographic frustum sample. Orientation is expressed in
// millidegrees and the viewport is a world-space rectangle; this makes the
// generated center rays analytically integrable while still using a complete
// position/orientation/frustum trajectory.
type CameraSample struct {
	TimeUS          int64   `json:"time_us"`
	PositionQ       Vec3i   `json:"position_q"`
	OrientationMdeg Vec3i   `json:"orientation_mdeg"`
	ViewWidthQ      int64   `json:"view_width_q"`
	ViewHeightQ     int64   `json:"view_height_q"`
	FovDeg          float64 `json:"fov_deg"`
	NearQ           int64   `json:"near_q"`
	FarQ            int64   `json:"far_q"`
	Width           int     `json:"width"`
	Height          int     `json:"height"`
}

type Recipe struct {
	Steps                int        `json:"steps"`
	PreviewSteps         int        `json:"preview_steps"`
	PhaseType            string     `json:"phase_type"`
	ShadowSamples        int        `json:"shadow_samples"`
	PreviewShadowSamples int        `json:"preview_shadow_samples"`
	TransmittanceCutoff  float64    `json:"transmittance_cutoff"`
	MaxParallel          int        `json:"max_parallel"`
	RayBuffer            int        `json:"ray_buffer"`
	ShadowBudget         int        `json:"shadow_budget"`
	PreviewLatencyMS     int        `json:"preview_latency_ms"`
	LightDir             [3]float64 `json:"light_dir"`
	AmbientColor         [3]float64 `json:"ambient_color"`
}

type Scene struct {
	Schema           string         `json:"schema"`
	Seed             int64          `json:"seed"`
	FixedScale       int64          `json:"fixed_scale"`
	CoefficientScale int64          `json:"coefficient_scale"`
	ChunkSize        int            `json:"chunk_size"`
	ActiveCamera     int            `json:"active_camera"`
	Media            []Medium       `json:"media"`
	Cameras          []CameraSample `json:"cameras"`
	Recipe           Recipe         `json:"recipe"`
	Truth            *TruthSet      `json:"truth,omitempty"`
}

func (s *Scene) ActiveCameraSample() *CameraSample {
	if len(s.Cameras) == 0 {
		return nil
	}
	i := s.ActiveCamera
	if i < 0 || i >= len(s.Cameras) {
		i = 0
	}
	return &s.Cameras[i]
}

func (s *Scene) MediumByID(id string) (*Medium, int) {
	for i := range s.Media {
		if s.Media[i].ID == id {
			return &s.Media[i], i
		}
	}
	return nil, -1
}

func hashStrings(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		var n [8]byte
		binary.LittleEndian.PutUint64(n[:], uint64(len(p)))
		h.Write(n[:])
		h.Write([]byte(p))
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func hashBytes(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		var n [8]byte
		binary.LittleEndian.PutUint64(n[:], uint64(len(p)))
		h.Write(n[:])
		h.Write(p)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func (m *Medium) ContentHash() string {
	var b bytes.Buffer
	writeInt64 := func(v int64) {
		var x [8]byte
		binary.LittleEndian.PutUint64(x[:], uint64(v))
		b.Write(x[:])
	}
	b.WriteString(m.ID)
	for a := 0; a < 3; a++ {
		writeInt64(m.MinQ[a])
		writeInt64(m.MaxQ[a])
		writeInt64(int64(m.DensityRes[a]))
	}
	writeInt64(m.AbsorptionQ)
	writeInt64(m.ScatteringQ)
	writeInt64(m.AnisotropyQ)
	for _, d := range m.Density {
		writeInt64(int64(d))
	}
	return hashBytes(b.Bytes())
}

func (s *Scene) MediaVersion() string {
	parts := make([]string, 0, len(s.Media))
	for i := range s.Media {
		parts = append(parts, s.Media[i].ID+":"+s.Media[i].ContentHash())
	}
	sort.Strings(parts)
	return hashStrings(parts...)
}

func (s *Scene) GeometryVersion() string {
	parts := make([]string, 0, len(s.Media))
	for i := range s.Media {
		m := &s.Media[i]
		parts = append(parts, fmt.Sprintf("%s|%v|%v|%v", m.ID, m.MinQ, m.MaxQ, m.DensityRes))
	}
	sort.Strings(parts)
	return hashStrings(parts...)
}

func (s *Scene) CameraVersion() string {
	parts := []string{fmt.Sprintf("active=%d", s.ActiveCamera)}
	for _, c := range s.Cameras {
		parts = append(parts, fmt.Sprintf("%d|%v|%v|%d|%d|%.12g|%d|%d|%d|%d",
			c.TimeUS, c.PositionQ, c.OrientationMdeg, c.ViewWidthQ, c.ViewHeightQ,
			c.FovDeg, c.NearQ, c.FarQ, c.Width, c.Height))
	}
	return hashStrings(parts...)
}

func (s *Scene) RecipeVersion() string {
	r := s.Recipe
	return hashStrings(fmt.Sprintf("%d|%d|%s|%d|%d|%.12g|%d|%d|%d|%d|%v|%v",
		r.Steps, r.PreviewSteps, r.PhaseType, r.ShadowSamples, r.PreviewShadowSamples,
		r.TransmittanceCutoff, r.MaxParallel, r.RayBuffer, r.ShadowBudget,
		r.PreviewLatencyMS, r.LightDir, r.AmbientColor))
}

func (s *Scene) SnapshotVersion() string {
	return hashStrings(s.MediaVersion(), s.CameraVersion(), s.RecipeVersion(),
		fmt.Sprintf("chunk=%d schema=%s", s.ChunkSize, s.Schema))
}

func (s *Scene) Clone() *Scene {
	out := *s
	out.Media = make([]Medium, len(s.Media))
	for i := range s.Media {
		out.Media[i] = s.Media[i]
		out.Media[i].Density = append([]int(nil), s.Media[i].Density...)
		if s.Media[i].densityRaw != nil {
			out.Media[i].densityRaw = append([]byte(nil), s.Media[i].densityRaw...)
		}
	}
	out.Cameras = append([]CameraSample(nil), s.Cameras...)
	if s.Truth != nil {
		tr := *s.Truth
		tr.Rays = append([]RayTruth(nil), s.Truth.Rays...)
		for i := range tr.Rays {
			tr.Rays[i].Segments = append([]SegmentTruth(nil), s.Truth.Rays[i].Segments...)
		}
		tr.Chunks = append([]ScreenTruth(nil), s.Truth.Chunks...)
		out.Truth = &tr
	}
	return &out
}

type ChunkCoord struct {
	X int `json:"x"`
	Y int `json:"y"`
}

func (c ChunkCoord) String() string { return fmt.Sprintf("(%d,%d)", c.X, c.Y) }

func fixedToFloat(v int64) float64 { return float64(v) / float64(FixedScale) }
func floatToFixed(v float64) int64 { return int64(math.Round(v * float64(FixedScale))) }
