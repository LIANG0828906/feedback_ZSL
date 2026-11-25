package fog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
)

const (
	DensityMax        int32 = 1000
	DefaultAlgorithmV       = "vfog-kernel-v1"
	DefaultTolerance        = 0.035
	DefaultShadowDist       = 4.0
)

type PhaseType string

const (
	PhaseIsotropic PhaseType = "isotropic"
	PhaseHG        PhaseType = "hg"
)

// Medium coefficients are integer fixed-point values. Conversion to physical
// values happens only inside the integrator; the source scene never stores a
// lossy coefficient.
type Medium struct {
	ID           string  `json:"id"`
	Bounds       AABB    `json:"bounds"`
	DensityRes   [3]int  `json:"density_res"`
	Density      []int32 `json:"density"`
	AbsorptionFP int64   `json:"absorption_fp"`
	ScatteringFP int64   `json:"scattering_fp"`
	Anisotropy   float64 `json:"anisotropy"`
	Priority     int     `json:"priority"`
}

func (m Medium) DensityAtLocal(v Vec3) int32 {
	if len(m.Density) == 0 {
		return 0
	}
	size := m.Bounds.Size()
	nx, ny, nz := m.DensityRes[0], m.DensityRes[1], m.DensityRes[2]
	if nx <= 0 || ny <= 0 || nz <= 0 || size.X <= 0 || size.Y <= 0 || size.Z <= 0 {
		return 0
	}
	fx := (v.X - m.Bounds.Min.X) / size.X
	fy := (v.Y - m.Bounds.Min.Y) / size.Y
	fz := (v.Z - m.Bounds.Min.Z) / size.Z
	if fx < 0 || fy < 0 || fz < 0 || fx > 1 || fy > 1 || fz > 1 {
		return 0
	}
	ix := minInt(int(fx*float64(nx)), nx-1)
	iy := minInt(int(fy*float64(ny)), ny-1)
	iz := minInt(int(fz*float64(nz)), nz-1)
	i := ix + nx*(iy+ny*iz)
	if i < 0 || i >= len(m.Density) {
		return 0
	}
	d := m.Density[i]
	if d < 0 {
		return 0
	}
	if d > DensityMax {
		return DensityMax
	}
	return d
}

func (m Medium) VoxelIndex(x, y, z int) int {
	return x + m.DensityRes[0]*(y+m.DensityRes[1]*z)
}

func (m Medium) VoxelBounds(index int) (AABB, bool) {
	nx, ny, nz := m.DensityRes[0], m.DensityRes[1], m.DensityRes[2]
	if nx <= 0 || ny <= 0 || nz <= 0 || index < 0 || index >= nx*ny*nz {
		return AABB{}, false
	}
	iz := index / (nx * ny)
	rem := index % (nx * ny)
	iy := rem / nx
	ix := rem % nx
	s := m.Bounds.Size()
	step := Vec3{s.X / float64(nx), s.Y / float64(ny), s.Z / float64(nz)}
	mn := Vec3{
		m.Bounds.Min.X + float64(ix)*step.X,
		m.Bounds.Min.Y + float64(iy)*step.Y,
		m.Bounds.Min.Z + float64(iz)*step.Z,
	}
	return AABB{Min: mn, Max: mn.Add(step)}, true
}

type Camera struct {
	LogicalTime int64   `json:"logical_time"`
	Position    Vec3    `json:"position"`
	Yaw         float64 `json:"yaw"`
	Pitch       float64 `json:"pitch"`
	Roll        float64 `json:"roll"`
	FOVY        float64 `json:"fov_y"`
	Aspect      float64 `json:"aspect"`
}

type Recipe struct {
	TileWidth          int       `json:"tile_width"`
	TileHeight         int       `json:"tile_height"`
	Steps              int       `json:"steps"`
	PreviewSteps       int       `json:"preview_steps"`
	ShadowSamples      int       `json:"shadow_samples"`
	PreviewShadow      int       `json:"preview_shadow_samples"`
	MaxShadowSamples   int       `json:"max_shadow_samples"`
	ShadowBudget       int       `json:"shadow_budget"`
	MaxParallelTiles   int       `json:"max_parallel_tiles"`
	RayBufferPerWorker int       `json:"ray_buffer_per_worker"`
	Phase              PhaseType `json:"phase"`
	CoefficientScale   int64     `json:"coefficient_scale"`
	FixedScale         int64     `json:"fixed_scale"`
	MaxRayDistance     float64   `json:"max_ray_distance"`
	ShadowDistance     float64   `json:"shadow_distance"`
	LightDirection     Vec3      `json:"light_direction"`
	LightIntensity     float64   `json:"light_intensity"`
	PreviewLatencyMS   int64     `json:"preview_latency_ms"`
	Tolerance          float64   `json:"tolerance"`
	Version            string    `json:"version"`
	AlgorithmVersion   string    `json:"algorithm_version"`
}

type TruthRay struct {
	ID            string  `json:"id"`
	CameraTime    int64   `json:"camera_time"`
	X             int     `json:"x"`
	Y             int     `json:"y"`
	Origin        Vec3    `json:"origin"`
	Direction     Vec3    `json:"direction"`
	T0            float64 `json:"t0"`
	T1            float64 `json:"t1"`
	Transmittance float64 `json:"transmittance"`
	Scattered     float64 `json:"scattered"`
}

type Scene struct {
	Name          string     `json:"name"`
	Seed          int64      `json:"seed"`
	Width         int        `json:"width"`
	Height        int        `json:"height"`
	Media         []Medium   `json:"media"`
	Cameras       []Camera   `json:"cameras"`
	Recipe        Recipe     `json:"recipe"`
	Truth         []TruthRay `json:"truth"`
	SnapshotLabel string     `json:"snapshot_label"`
}

func (s *Scene) Clone() *Scene {
	if s == nil {
		return nil
	}
	c := *s
	c.Media = make([]Medium, len(s.Media))
	for i, m := range s.Media {
		c.Media[i] = m
		c.Media[i].Density = append([]int32(nil), m.Density...)
	}
	c.Cameras = append([]Camera(nil), s.Cameras...)
	c.Truth = append([]TruthRay(nil), s.Truth...)
	return &c
}

func (s *Scene) MediaByID(id string) (int, *Medium) {
	for i := range s.Media {
		if s.Media[i].ID == id {
			return i, &s.Media[i]
		}
	}
	return -1, nil
}

func (s *Scene) SortedMedia() []Medium {
	out := append([]Medium(nil), s.Media...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (s *Scene) Hash() string {
	return SceneHash(s)
}

func SceneHash(s *Scene) string {
	if s == nil {
		return ""
	}
	cp := s.Clone()
	// Truth may be regenerated from the scene and is not a content dependency.
	cp.Truth = nil
	// Sort media and cameras into a canonical order before hashing.
	sort.Slice(cp.Media, func(i, j int) bool { return cp.Media[i].ID < cp.Media[j].ID })
	sort.Slice(cp.Cameras, func(i, j int) bool {
		if cp.Cameras[i].LogicalTime != cp.Cameras[j].LogicalTime {
			return cp.Cameras[i].LogicalTime < cp.Cameras[j].LogicalTime
		}
		return cp.Cameras[i].Position.X < cp.Cameras[j].Position.X
	})
	b, _ := json.Marshal(cp)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (m *Medium) ContentHash(scale int64) string {
	cp := *m
	cp.Density = append([]int32(nil), m.Density...)
	b, _ := json.Marshal(struct {
		Medium
		Scale int64 `json:"scale"`
	}{cp, scale})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (s *Scene) ContentHash(scale int64) string {
	ids := make([]string, 0, len(s.Media))
	for i := range s.Media {
		ids = append(ids, s.Media[i].ContentHash(scale))
	}
	sort.Strings(ids)
	return hashStrings(ids...)
}

func (s *Scene) CameraHash() string {
	cp := append([]Camera(nil), s.Cameras...)
	sort.Slice(cp, func(i, j int) bool {
		if cp[i].LogicalTime != cp[j].LogicalTime {
			return cp[i].LogicalTime < cp[j].LogicalTime
		}
		return cp[i].Position.X < cp[j].Position.X
	})
	b, _ := json.Marshal(cp)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (s *Scene) RecipeHash() string {
	r := s.Recipe
	b, _ := json.Marshal(r)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// StructureHash covers the parts of media that affect whether a ray can hit
// them at all. Density values and coefficients are intentionally excluded:
// those edits are tracked at per-voxel/per-coefficient dependency granularity.
func (s *Scene) StructureHash() string {
	type mediaStructure struct {
		ID         string `json:"id"`
		Bounds     AABB   `json:"bounds"`
		Resolution [3]int `json:"resolution"`
		Priority   int    `json:"priority"`
	}
	ms := make([]mediaStructure, 0, len(s.Media))
	for i := range s.Media {
		ms = append(ms, mediaStructure{
			ID: s.Media[i].ID, Bounds: s.Media[i].Bounds,
			Resolution: s.Media[i].DensityRes, Priority: s.Media[i].Priority,
		})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].ID < ms[j].ID })
	b, _ := json.Marshal(struct {
		Width  int              `json:"width"`
		Height int              `json:"height"`
		Media  []mediaStructure `json:"media"`
	}{s.Width, s.Height, ms})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (s *Scene) CameraAt(time int64) (Camera, bool) {
	for _, c := range s.Cameras {
		if c.LogicalTime == time {
			return c, true
		}
	}
	return Camera{}, false
}

func (r *Recipe) Validate() *Fault {
	if r.TileWidth <= 0 || r.TileHeight <= 0 {
		return newFault(EPara, "recipe.tile", "tile dimensions must be positive", []int{r.TileWidth, r.TileHeight}, ">0")
	}
	if r.Steps <= 0 {
		return newFault(EPara, "recipe.steps", "primary step budget must be positive", r.Steps, ">0")
	}
	if r.PreviewSteps <= 0 || r.PreviewSteps > r.Steps {
		return newFault(EPara, "recipe.preview_steps", "preview steps must be in (0, steps]", r.PreviewSteps, fmt.Sprintf("(0,%d]", r.Steps))
	}
	if r.ShadowSamples < 0 || r.PreviewShadow < 0 {
		return newFault(EPara, "recipe.shadow_samples", "shadow samples cannot be negative", []int{r.ShadowSamples, r.PreviewShadow}, ">=0")
	}
	if r.MaxShadowSamples <= 0 || r.PreviewShadow > r.MaxShadowSamples || r.ShadowSamples > r.MaxShadowSamples {
		return newFault(EPara, "recipe.max_shadow_samples", "shadow samples exceed declared cap", []int{r.PreviewShadow, r.ShadowSamples, r.MaxShadowSamples}, "0<=preview<=shadow<=cap")
	}
	if r.ShadowBudget <= 0 {
		return newFault(EPara, "recipe.shadow_budget", "global shadow budget must be positive", r.ShadowBudget, ">0")
	}
	if r.MaxParallelTiles <= 0 {
		return newFault(EPara, "recipe.max_parallel_tiles", "parallel tile limit must be positive", r.MaxParallelTiles, ">0")
	}
	if r.RayBufferPerWorker <= 0 {
		return newFault(EPara, "recipe.ray_buffer_per_worker", "ray buffer must be positive", r.RayBufferPerWorker, ">0")
	}
	if r.CoefficientScale <= 0 || r.FixedScale <= 0 {
		return newFault(EPara, "recipe.coefficient_scale", "fixed-point scales must be positive", []int64{r.CoefficientScale, r.FixedScale}, ">0")
	}
	if r.Phase != PhaseIsotropic && r.Phase != PhaseHG {
		return newFault(EPara, "recipe.phase", "unsupported phase function", r.Phase, []PhaseType{PhaseIsotropic, PhaseHG})
	}
	if !r.LightDirection.IsFinite() || r.LightDirection.LengthSquared() < 1e-12 {
		return newFault(EPara, "recipe.light_direction", "light direction must be finite and non-zero", r.LightDirection, "non-zero vector")
	}
	if r.MaxRayDistance <= 0 || r.ShadowDistance <= 0 {
		return newFault(EPara, "recipe.ray_distance", "ray distances must be positive", []float64{r.MaxRayDistance, r.ShadowDistance}, ">0")
	}
	if r.Tolerance < 0 {
		return newFault(EPara, "recipe.tolerance", "tolerance cannot be negative", r.Tolerance, ">=0")
	}
	if r.PreviewLatencyMS <= 0 {
		return newFault(EPara, "recipe.preview_latency_ms", "preview latency must be positive", r.PreviewLatencyMS, ">0")
	}
	if r.Version == "" || r.AlgorithmVersion == "" {
		return newFault(EPara, "recipe.version", "recipe and algorithm versions are required", []string{r.Version, r.AlgorithmVersion}, "non-empty")
	}
	return nil
}

func (s *Scene) Validate() *Fault {
	if s == nil {
		return newFault(EDecode, "$", "scene is nil", nil, "object")
	}
	if s.Width <= 0 || s.Height <= 0 {
		return newFault(EPara, "viewport", "viewport dimensions must be positive", []int{s.Width, s.Height}, ">0")
	}
	if f := s.Recipe.Validate(); f != nil {
		return f
	}
	if len(s.Media) == 0 {
		return newFault(EDecode, "media", "scene has no media", 0, ">0")
	}
	seen := map[string]bool{}
	for i := range s.Media {
		m := &s.Media[i]
		path := fmt.Sprintf("media[%d]", i)
		if m.ID == "" {
			return newFault(EDecode, path+".id", "empty medium id", m.ID, "non-empty")
		}
		if seen[m.ID] {
			return newFault(EDecode, path+".id", "duplicate medium id", m.ID, "unique")
		}
		seen[m.ID] = true
		if !m.Bounds.Min.IsFinite() || !m.Bounds.Max.IsFinite() {
			return newFault(EDecode, path+".bounds", "bounds contain NaN or Inf", m.Bounds, "finite")
		}
		if m.Bounds.Min.X > m.Bounds.Max.X || m.Bounds.Min.Y > m.Bounds.Max.Y || m.Bounds.Min.Z > m.Bounds.Max.Z {
			return &Fault{Code: EPara, Path: path + ".bounds", MediaID: m.ID, Actual: m.Bounds, Expected: "min<=max", Message: "bounding box minimum exceeds maximum"}
		}
		size := m.Bounds.Size()
		if size.X <= 0 || size.Y <= 0 || size.Z <= 0 {
			return &Fault{Code: EPara, Path: path + ".bounds", MediaID: m.ID, Actual: size, Expected: "positive extent", Message: "degenerate bounding box"}
		}
		for axis, n := range m.DensityRes {
			if n <= 0 || n > 64 {
				return &Fault{Code: EDens, Path: fmt.Sprintf("%s.density_res[%d]", path, axis), MediaID: m.ID, Actual: n, Expected: "1..64", Message: "density resolution out of range"}
			}
		}
		want := m.DensityRes[0] * m.DensityRes[1] * m.DensityRes[2]
		if len(m.Density) != want {
			return &Fault{Code: EDens, Path: path + ".density", MediaID: m.ID, Actual: len(m.Density), Expected: want, Message: "density sample count does not match resolution"}
		}
		voxel := Vec3{
			size.X / float64(m.DensityRes[0]),
			size.Y / float64(m.DensityRes[1]),
			size.Z / float64(m.DensityRes[2]),
		}
		// This is the declared compatibility window. It rejects accidental
		// millimetre/centimetre mixes while allowing moderately anisotropic
		// voxels.
		for axis, v := range []float64{voxel.X, voxel.Y, voxel.Z} {
			if v < 0.002 || v > 2.0 {
				return &Fault{Code: EDens, Path: fmt.Sprintf("%s.voxel_size[%d]", path, axis), MediaID: m.ID, Actual: v, Expected: "0.002..2.0", Message: "density resolution incompatible with bounds"}
			}
		}
		for _, d := range m.Density {
			if d < 0 || d > DensityMax {
				return &Fault{Code: EDecode, Path: path + ".density", MediaID: m.ID, Actual: d, Expected: fmt.Sprintf("0..%d", DensityMax), Message: "corrupt density sample"}
			}
		}
		maxCoef := 10 * s.Recipe.CoefficientScale
		if m.AbsorptionFP < 0 || m.AbsorptionFP > maxCoef {
			return &Fault{Code: EPara, Path: path + ".absorption_fp", MediaID: m.ID, Actual: m.AbsorptionFP, Expected: fmt.Sprintf("0..%d", maxCoef), Message: "absorption outside declared fixed-point interval"}
		}
		if m.ScatteringFP < 0 || m.ScatteringFP > maxCoef {
			return &Fault{Code: EPara, Path: path + ".scattering_fp", MediaID: m.ID, Actual: m.ScatteringFP, Expected: fmt.Sprintf("0..%d", maxCoef), Message: "scattering outside declared fixed-point interval"}
		}
		if !isFinite(m.Anisotropy) || m.Anisotropy < -0.99 || m.Anisotropy > 0.99 {
			return &Fault{Code: EPara, Path: path + ".anisotropy", MediaID: m.ID, Actual: m.Anisotropy, Expected: "[-0.99,0.99]", Message: "anisotropy out of range"}
		}
	}
	if len(s.Cameras) == 0 {
		return newFault(EDecode, "cameras", "scene has no camera samples", 0, ">0")
	}
	var prev int64
	for i, c := range s.Cameras {
		path := fmt.Sprintf("cameras[%d]", i)
		if !c.Position.IsFinite() || !isFinite(c.Yaw) || !isFinite(c.Pitch) || !isFinite(c.Roll) {
			return newFault(EDecode, path, "camera contains NaN or Inf", c, "finite")
		}
		if c.FOVY <= 0 || c.FOVY >= math.Pi || c.Aspect <= 0 {
			return newFault(EPara, path, "invalid camera projection", []float64{c.FOVY, c.Aspect}, "0<fov<pi, aspect>0")
		}
		if i > 0 && c.LogicalTime <= prev {
			return &Fault{Code: EOrder, Path: path + ".logical_time", Actual: c.LogicalTime, Expected: fmt.Sprintf(">%d", prev), Message: "camera sample times are not strictly increasing"}
		}
		prev = c.LogicalTime
	}
	return nil
}

func DecodeScene(r io.Reader) (*Scene, *Fault) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var s Scene
	if err := dec.Decode(&s); err != nil {
		return nil, &Fault{Code: EDecode, Path: "$", Message: "scene decode failed", Cause: err.Error()}
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, &Fault{Code: EDecode, Path: "$", Message: "extra JSON value after scene", Cause: err.Error()}
	}
	if f := s.Validate(); f != nil {
		return nil, f
	}
	return &s, nil
}

func EncodeScene(w io.Writer, s *Scene) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

func hashStrings(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
