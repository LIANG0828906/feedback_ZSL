package volumefog

import (
	"math"
	"sort"
)

type SegmentTruth struct {
	Index              int      `json:"index"`
	T0Q                int64    `json:"t0_q"`
	T1Q                int64    `json:"t1_q"`
	Active             []string `json:"active"`
	ResidualQ          float64  `json:"residual_q"`
	Transmittance      float64  `json:"transmittance"`
	CumulativeRadiance float64  `json:"cumulative_radiance"`
}

type RayTruth struct {
	RayID              string         `json:"ray_id"`
	Segments           []SegmentTruth `json:"segments"`
	FinalTransmittance float64        `json:"final_transmittance"`
	FinalRadiance      float64        `json:"final_radiance"`
}

type ScreenTruth struct {
	ChunkX        int        `json:"chunk_x"`
	ChunkY        int        `json:"chunk_y"`
	PixelX        int        `json:"pixel_x"`
	PixelY        int        `json:"pixel_y"`
	RayID         string     `json:"ray_id"`
	Color         [3]float64 `json:"color"`
	Transmittance float64    `json:"transmittance"`
	Scattering    float64    `json:"scattering"`
}

type TruthSet struct {
	GeneratedBy string        `json:"generated_by"`
	Rays        []RayTruth    `json:"rays"`
	Chunks      []ScreenTruth `json:"chunks"`
}

type analyticCell struct {
	T0Q int64
	T1Q int64
}

// analyticSegmentsZ is the generator's independent +Z AABB/overlap
// construction. It intentionally does not call IntersectRay/BuildSegments.
func analyticSegmentsZ(scene *Scene, ray Ray) []Segment {
	type zone struct {
		id    string
		start int64
		end   int64
	}
	var zones []zone
	originZ := ray.OriginQ[2]
	x, y := ray.OriginQ[0], ray.OriginQ[1]
	for i := range scene.Media {
		m := &scene.Media[i]
		if x < m.MinQ[0] || x >= m.MaxQ[0] || y < m.MinQ[1] || y >= m.MaxQ[1] {
			continue
		}
		startZ := m.MinQ[2]
		if startZ < originZ+ray.NearQ {
			startZ = originZ + ray.NearQ
		}
		endZ := m.MaxQ[2]
		if endZ > originZ+ray.FarQ {
			endZ = originZ + ray.FarQ
		}
		if endZ <= startZ {
			continue
		}
		zones = append(zones, zone{id: m.ID, start: startZ - originZ, end: endZ - originZ})
	}
	sort.Slice(zones, func(i, j int) bool {
		if zones[i].start != zones[j].start {
			return zones[i].start < zones[j].start
		}
		if zones[i].end != zones[j].end {
			return zones[i].end < zones[j].end
		}
		return zones[i].id < zones[j].id
	})
	set := map[int64]struct{}{}
	for _, z := range zones {
		set[z.start] = struct{}{}
		set[z.end] = struct{}{}
	}
	bounds := make([]int64, 0, len(set))
	for b := range set {
		bounds = append(bounds, b)
	}
	sort.Slice(bounds, func(i, j int) bool { return bounds[i] < bounds[j] })
	var out []Segment
	for i := 0; i+1 < len(bounds); i++ {
		lo, hi := bounds[i], bounds[i+1]
		if hi <= lo {
			continue
		}
		var active []string
		for _, z := range zones {
			if z.start <= lo && z.end >= hi {
				active = append(active, z.id)
			}
		}
		if len(active) == 0 {
			continue
		}
		sort.Strings(active)
		out = append(out, Segment{Index: len(out), T0Q: lo, T1Q: hi, Active: active})
	}
	return out
}

func requireAxisAlignedTruth(ray Ray) error {
	if ray.DirQ[0] != 0 || ray.DirQ[1] != 0 || ray.DirQ[2] != FixedScale {
		return &FogError{Code: E_INT, Message: "analytic truth requires +Z axis-aligned rays",
			RayID: ray.ID}
	}
	return nil
}

func axisAlignedCells(media []Medium, active []string, ray Ray, t0, t1 int64) []analyticCell {
	zStart := ray.OriginQ[2]/FixedScale + t0
	zEnd := ray.OriginQ[2]/FixedScale + t1
	set := map[int64]struct{}{t0: {}, t1: {}}
	x, y := ray.OriginQ[0], ray.OriginQ[1]
	for _, id := range active {
		m, _ := findByID(media, id)
		if m == nil || x < m.MinQ[0] || x >= m.MaxQ[0] || y < m.MinQ[1] || y >= m.MaxQ[1] {
			continue
		}
		ext := m.ExtentQ()[2]
		res := int64(m.DensityRes[2])
		if ext <= 0 || res <= 0 {
			continue
		}
		voxel := ext / res
		loZ := m.MinQ[2]
		hiZ := m.MaxQ[2]
		first := ceilDiv(loZ-zStart, voxel)
		last := floorDiv(hiZ-zStart, voxel)
		for k := first; k <= last; k++ {
			z := loZ + k*voxel
			if z <= zStart || z >= zEnd {
				continue
			}
			set[z-zStart] = struct{}{}
		}
	}
	bounds := make([]int64, 0, len(set))
	for b := range set {
		bounds = append(bounds, b)
	}
	sort.Slice(bounds, func(i, j int) bool { return bounds[i] < bounds[j] })
	cells := make([]analyticCell, 0, len(bounds)-1)
	for i := 0; i+1 < len(bounds); i++ {
		if bounds[i+1] > bounds[i] {
			cells = append(cells, analyticCell{T0Q: bounds[i], T1Q: bounds[i+1]})
		}
	}
	return cells
}

func findByID(media []Medium, id string) (*Medium, int) {
	for i := range media {
		if media[i].ID == id {
			return &media[i], i
		}
	}
	return nil, -1
}

func compositeAt(scene *Scene, active []string, p Vec3i, rayDir [3]float64) (sigmaT, sigmaS, phase float64) {
	var gWeighted float64
	var gSum float64
	for _, id := range active {
		m, _ := scene.MediumByID(id)
		if m == nil {
			continue
		}
		d, _, ok := m.DensityAt(p)
		if !ok {
			continue
		}
		a := m.Absorption() * d
		s := m.Scattering() * d
		sigmaT += a + s
		sigmaS += s
		gWeighted += s * m.Anisotropy()
		gSum += s
	}
	g := 0.0
	if gSum > 0 {
		g = gWeighted / gSum
	}
	phase = phaseValue(scene.Recipe, g, rayDir)
	return sigmaT, sigmaS, phase
}

func normalize3(v [3]float64) [3]float64 {
	n := math.Sqrt(v[0]*v[0] + v[1]*v[1] + v[2]*v[2])
	if n < 1e-15 {
		return [3]float64{0, 0, 1}
	}
	return [3]float64{v[0] / n, v[1] / n, v[2] / n}
}

func phaseValue(recipe Recipe, g float64, rayDir [3]float64) float64 {
	ld := normalize3(recipe.LightDir)
	rd := normalize3(rayDir)
	mu := rd[0]*ld[0] + rd[1]*ld[1] + rd[2]*ld[2]
	switch recipe.PhaseType {
	case "isotropic":
		return 1.0 / (4.0 * math.Pi)
	case "hg":
		g = math.Max(-0.999999, math.Min(0.999999, g))
		den := 1 + g*g - 2*g*mu
		den = math.Max(1e-9, den)
		return (1 - g*g) / (4 * math.Pi * math.Pow(den, 1.5))
	default:
		return 0
	}
}

func shadowTransmittanceExact(scene *Scene, p Vec3i, farQ int64) float64 {
	cam := scene.ActiveCameraSample()
	if cam == nil {
		return 1
	}
	farZ := farQ
	if farZ <= p[2] {
		return 1
	}
	start := p[2]
	set := map[int64]struct{}{start: {}, farZ: {}}
	x, y := p[0], p[1]
	for i := range scene.Media {
		m := &scene.Media[i]
		if x < m.MinQ[0] || x >= m.MaxQ[0] || y < m.MinQ[1] || y >= m.MaxQ[1] {
			continue
		}
		ext := m.ExtentQ()[2]
		res := int64(m.DensityRes[2])
		if ext <= 0 || res <= 0 {
			continue
		}
		voxel := ext / res
		lo := m.MinQ[2]
		hi := m.MaxQ[2]
		first := ceilDiv(lo-start, voxel)
		last := floorDiv(hi-start, voxel)
		for k := first; k <= last; k++ {
			z := lo + k*voxel
			if z > start && z < farZ {
				set[z] = struct{}{}
			}
		}
	}
	bounds := make([]int64, 0, len(set))
	for b := range set {
		bounds = append(bounds, b)
	}
	sort.Slice(bounds, func(i, j int) bool { return bounds[i] < bounds[j] })
	var tau float64
	for i := 0; i+1 < len(bounds); i++ {
		t0, t1 := bounds[i], bounds[i+1]
		midZ := (t0 + t1) / 2
		mid := Vec3i{x, y, midZ}
		var active []string
		for j := range scene.Media {
			if scene.Media[j].Contains(mid) {
				active = append(active, scene.Media[j].ID)
			}
		}
		sort.Strings(active)
		sigmaT, _, _ := compositeAt(scene, active, mid, [3]float64{0, 0, 1})
		dt := float64(t1-t0) / float64(FixedScale)
		tau += sigmaT * dt
	}
	return math.Exp(-tau)
}

// BuildRayTruth computes the analytic per-segment truth for a +Z-only ray.
func BuildRayTruth(scene *Scene, ray Ray) (RayTruth, error) {
	if err := requireAxisAlignedTruth(ray); err != nil {
		return RayTruth{}, err
	}
	segments := analyticSegmentsZ(scene, ray)
	out := RayTruth{RayID: ray.ID, FinalTransmittance: 1}
	T := 1.0
	L := 0.0
	rayDir := ray.DirectionFloat()
	for _, seg := range segments {
		cells := axisAlignedCells(scene.Media, seg.Active, ray, seg.T0Q, seg.T1Q)
		for _, cell := range cells {
			midT := (cell.T0Q + cell.T1Q) / 2
			mid := ray.PointAtQ(midT)
			sigmaT, sigmaS, phase := compositeAt(scene, seg.Active, mid, rayDir)
			shadow := shadowTransmittanceExact(scene, mid, ray.FarQ)
			source := sigmaS * phase * shadow
			dt := float64(cell.T1Q-cell.T0Q) / float64(FixedScale)
			if sigmaT > 1e-15 {
				L += T * source * (1 - math.Exp(-sigmaT*dt)) / sigmaT
			} else {
				L += T * source * dt
			}
			T *= math.Exp(-sigmaT * dt)
			if T <= scene.Recipe.TransmittanceCutoff {
				T = 0
				break
			}
		}
		out.Segments = append(out.Segments, SegmentTruth{
			Index: seg.Index, T0Q: seg.T0Q, T1Q: seg.T1Q,
			Active: append([]string(nil), seg.Active...), ResidualQ: seg.ResidualQ,
			Transmittance: T, CumulativeRadiance: L,
		})
		if T == 0 {
			break
		}
	}
	out.FinalTransmittance = T
	out.FinalRadiance = L
	return out, nil
}

// GenerateTruth constructs the full truth list for sample rays and chunk
// center pixels. It deliberately uses closed-form cell integration.
func GenerateTruth(scene *Scene, sampleRays []Ray) (*TruthSet, error) {
	truth := &TruthSet{GeneratedBy: "analytic-cell-v1"}
	for _, ray := range sampleRays {
		rt, err := BuildRayTruth(scene, ray)
		if err != nil {
			return nil, err
		}
		truth.Rays = append(truth.Rays, rt)
	}
	cam := scene.ActiveCameraSample()
	if cam == nil {
		return truth, nil
	}
	for cy := 0; cy*scene.ChunkSize < cam.Height; cy++ {
		for cx := 0; cx*scene.ChunkSize < cam.Width; cx++ {
			ray, px, py, err := CenterChunkRay(scene, cx, cy)
			if err != nil {
				return nil, err
			}
			rt, err := BuildRayTruth(scene, ray)
			if err != nil {
				return nil, err
			}
			truth.Chunks = append(truth.Chunks, ScreenTruth{
				ChunkX: cx, ChunkY: cy, PixelX: px, PixelY: py, RayID: ray.ID,
				Color: [3]float64{
					rt.FinalRadiance * scene.Recipe.AmbientColor[0],
					rt.FinalRadiance * scene.Recipe.AmbientColor[1],
					rt.FinalRadiance * scene.Recipe.AmbientColor[2],
				},
				Transmittance: rt.FinalTransmittance, Scattering: rt.FinalRadiance,
			})
		}
	}
	return truth, nil
}
