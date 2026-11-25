package fogkernel

import (
	"math"
	"sort"
)

type Interval struct {
	T0 float64
	T1 float64
}

// ClipAABB returns the normalized-ray parameter interval inside an AABB.
func ClipAABB(ray Ray, mn, mx Vec3) (float64, float64, bool) {
	tmin := -math.MaxFloat64
	tmax := math.MaxFloat64
	o := []float64{ray.Origin.X, ray.Origin.Y, ray.Origin.Z}
	d := []float64{ray.Direction.X, ray.Direction.Y, ray.Direction.Z}
	lo := []float64{mn.X, mn.Y, mn.Z}
	hi := []float64{mx.X, mx.Y, mx.Z}
	for axis := 0; axis < 3; axis++ {
		if math.Abs(d[axis]) < 1e-15 {
			if o[axis] < lo[axis]-1e-12 || o[axis] > hi[axis]+1e-12 {
				return 0, 0, false
			}
			continue
		}
		t1 := (lo[axis] - o[axis]) / d[axis]
		t2 := (hi[axis] - o[axis]) / d[axis]
		if t1 > t2 {
			t1, t2 = t2, t1
		}
		if t1 > tmin {
			tmin = t1
		}
		if t2 < tmax {
			tmax = t2
		}
		if tmin > tmax+1e-10 {
			return 0, 0, false
		}
	}
	if tmax < -1e-10 {
		return 0, 0, false
	}
	if tmin < 0 {
		tmin = 0
	}
	if tmax-tmin < 1e-10 {
		return 0, 0, false
	}
	return tmin, tmax, true
}

type VoxelRef struct {
	VolumeID string `json:"volume_id"`
	X        int    `json:"x"`
	Y        int    `json:"y"`
	Z        int    `json:"z"`
	Density  int    `json:"density"`
}

type VoxelSegment struct {
	Ref    VoxelRef `json:"ref"`
	T0     float64  `json:"t0"`
	T1     float64  `json:"t1"`
	Length float64  `json:"length"`
}

func (m *Medium) boundsMeters() (Vec3, Vec3) {
	return m.MinMicro.ToMeters(), m.MaxMicro.ToMeters()
}

// VoxelSegments exact-clips a ray parameter interval to every intersected
// axis-aligned voxel. The generated grids are deliberately small enough that a
// direct voxel scan is clearer than a DDA implementation.
func VoxelSegments(medium *Medium, ray Ray, tLo, tHi float64) []VoxelSegment {
	mn, _ := medium.boundsMeters()
	voxel := float64(medium.VoxelMicro) / float64(DistanceScale)
	n := medium.Density.Size
	out := make([]VoxelSegment, 0, 8)
	for iz := 0; iz < int(n.Z); iz++ {
		for iy := 0; iy < int(n.Y); iy++ {
			for ix := 0; ix < int(n.X); ix++ {
				vmin := Vec3{X: mn.X + float64(ix)*voxel, Y: mn.Y + float64(iy)*voxel, Z: mn.Z + float64(iz)*voxel}
				vmax := Vec3{X: vmin.X + voxel, Y: vmin.Y + voxel, Z: vmin.Z + voxel}
				a, b, ok := ClipAABB(ray, vmin, vmax)
				if !ok {
					continue
				}
				a = math.Max(a, tLo)
				b = math.Min(b, tHi)
				if b-a <= 1e-10 {
					continue
				}
				density := medium.Density.Get(ix, iy, iz)
				out = append(out, VoxelSegment{
					Ref: VoxelRef{VolumeID: medium.ID, X: ix, Y: iy, Z: iz, Density: density},
					T0:  a, T1: b, Length: (b - a) * ray.Direction.Norm(),
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if math.Abs(out[i].T0-out[j].T0) > 1e-12 {
			return out[i].T0 < out[j].T0
		}
		return out[i].Ref.VolumeID < out[j].Ref.VolumeID
	})
	return out
}

type BlockPlan struct {
	BlockIndex int            `json:"block_index"`
	Bin        int64          `json:"chunk_bin"`
	T0         float64        `json:"t0"`
	T1         float64        `json:"t1"`
	Segments   []VoxelSegment `json:"-"`
	VolumeIDs  []string       `json:"volume_ids"`
}

type RayPlan struct {
	RayIndex int         `json:"ray_index"`
	Hit      bool        `json:"hit"`
	T0       float64     `json:"t0"`
	T1       float64     `json:"t1"`
	Blocks   []BlockPlan `json:"blocks"`
}

func PlanRay(scene *Scene, ray Ray) RayPlan {
	chunk := scene.Recipe.ChunkMeters()
	type rawChunk struct {
		bin  int64
		t0   float64
		t1   float64
		segs []VoxelSegment
		vols map[string]struct{}
	}
	chunks := map[int64]*rawChunk{}
	var first, last float64
	hit := false

	for _, medium := range scene.Media {
		mn, mx := medium.boundsMeters()
		t0, t1, ok := ClipAABB(ray, mn, mx)
		if !ok {
			continue
		}
		if !hit || t0 < first {
			first = t0
		}
		if !hit || t1 > last {
			last = t1
		}
		hit = true
		startBin := int64(math.Floor((t0 - 1e-9) / chunk))
		endBin := int64(math.Floor((t1 + 1e-9) / chunk))
		for bin := startBin; bin <= endBin; bin++ {
			c0 := math.Max(t0, float64(bin)*chunk)
			c1 := math.Min(t1, float64(bin+1)*chunk)
			if c1-c0 <= 1e-9 {
				continue
			}
			segments := VoxelSegments(medium, ray, c0, c1)
			if len(segments) == 0 {
				continue
			}
			c, ok := chunks[bin]
			if !ok {
				c = &rawChunk{bin: bin, t0: c0, t1: c1, vols: map[string]struct{}{}}
				chunks[bin] = c
			}
			c.t0 = math.Min(c.t0, c0)
			c.t1 = math.Max(c.t1, c1)
			c.segs = append(c.segs, segments...)
			c.vols[medium.ID] = struct{}{}
		}
	}
	if !hit || len(chunks) == 0 {
		return RayPlan{RayIndex: ray.Index, Hit: false}
	}
	bins := make([]int64, 0, len(chunks))
	for bin := range chunks {
		bins = append(bins, bin)
	}
	sort.Slice(bins, func(i, j int) bool { return bins[i] < bins[j] })
	plan := RayPlan{RayIndex: ray.Index, Hit: true, T0: first, T1: last}
	for idx, bin := range bins {
		c := chunks[bin]
		vols := make([]string, 0, len(c.vols))
		for v := range c.vols {
			vols = append(vols, v)
		}
		sort.Strings(vols)
		sort.Slice(c.segs, func(i, j int) bool {
			if math.Abs(c.segs[i].T0-c.segs[j].T0) > 1e-12 {
				return c.segs[i].T0 < c.segs[j].T0
			}
			return c.segs[i].Ref.VolumeID < c.segs[j].Ref.VolumeID
		})
		plan.Blocks = append(plan.Blocks, BlockPlan{
			BlockIndex: idx, Bin: bin, T0: c.t0, T1: c.t1,
			Segments: c.segs, VolumeIDs: vols,
		})
	}
	return plan
}

func (v Vec3) Norm() float64 { return math.Sqrt(v.Dot(v)) }
