package fog

import (
	"math"
)

type GenerateOptions struct {
	Seed          int64
	Width         int
	Height        int
	TileWidth     int
	TileHeight    int
	Steps         int
	PreviewSteps  int
	ShadowSamples int
	PreviewShadow int
	ShadowBudget  int
	MaxParallel   int
	RayBuffer     int
}

func DefaultGenerateOptions(seed int64) GenerateOptions {
	return GenerateOptions{
		Seed:          seed,
		Width:         16,
		Height:        12,
		TileWidth:     4,
		TileHeight:    3,
		Steps:         48,
		PreviewSteps:  14,
		ShadowSamples: 4,
		PreviewShadow: 1,
		ShadowBudget:  96,
		MaxParallel:   4,
		RayBuffer:     3,
	}
}

// Generate produces a complete, deterministic sample. All randomness is
// derived from a splitmix64 stream seeded only by opts.Seed.
func Generate(opts GenerateOptions) *Scene {
	def := DefaultGenerateOptions(opts.Seed)
	if opts.Width <= 0 {
		opts.Width = def.Width
	}
	if opts.Height <= 0 {
		opts.Height = def.Height
	}
	if opts.TileWidth <= 0 {
		opts.TileWidth = def.TileWidth
	}
	if opts.TileHeight <= 0 {
		opts.TileHeight = def.TileHeight
	}
	if opts.Steps <= 0 {
		opts.Steps = def.Steps
	}
	if opts.PreviewSteps <= 0 || opts.PreviewSteps > opts.Steps {
		opts.PreviewSteps = minInt(def.PreviewSteps, opts.Steps)
	}
	if opts.ShadowSamples < 0 {
		opts.ShadowSamples = def.ShadowSamples
	}
	if opts.PreviewShadow < 0 || opts.PreviewShadow > opts.ShadowSamples {
		opts.PreviewShadow = minInt(def.PreviewShadow, opts.ShadowSamples)
	}
	if opts.ShadowBudget <= 0 {
		opts.ShadowBudget = def.ShadowBudget
	}
	if opts.MaxParallel <= 0 {
		opts.MaxParallel = def.MaxParallel
	}
	if opts.RayBuffer <= 0 {
		opts.RayBuffer = def.RayBuffer
	}
	rng := newRNG(uint64(opts.Seed))
	maxShadow := maxInt(8, opts.ShadowSamples)
	r := Recipe{
		TileWidth:          opts.TileWidth,
		TileHeight:         opts.TileHeight,
		Steps:              opts.Steps,
		PreviewSteps:       opts.PreviewSteps,
		ShadowSamples:      opts.ShadowSamples,
		PreviewShadow:      opts.PreviewShadow,
		MaxShadowSamples:   maxShadow,
		ShadowBudget:       opts.ShadowBudget,
		MaxParallelTiles:   opts.MaxParallel,
		RayBufferPerWorker: opts.RayBuffer,
		Phase:              PhaseHG,
		CoefficientScale:   DefaultFixedScale,
		FixedScale:         DefaultFixedScale,
		MaxRayDistance:     12,
		ShadowDistance:     DefaultShadowDist,
		LightDirection:     V(-0.4, 0.7, -0.6).Normalize(),
		LightIntensity:     1.0,
		PreviewLatencyMS:   120,
		Tolerance:          DefaultTolerance,
		Version:            "recipe-1",
		AlgorithmVersion:   DefaultAlgorithmV,
	}
	// The calibration body is intentionally isolated from every other body on
	// the analytic truth rays (x=y=0, z from 1..7).
	calibration := uniformMedium("calibration", AABB{Min: V(-1, -1, 2), Max: V(1, 1, 4)},
		[3]int{4, 4, 4}, 600, 120_000, 80_000, 0.2, 0)

	haze := makeDensityMedium(rng, "haze-west", AABB{Min: V(-3.8, -2.4, 1.5), Max: V(-1.4, 1.9, 5.8)},
		[3]int{6, 8, 10}, 70_000, 120_000, 0.15, 1)
	puff := makeDensityMedium(rng, "puff-east", AABB{Min: V(1.25, -2.8, 3.0), Max: V(4.2, 2.2, 7.5)},
		[3]int{7, 9, 8}, 45_000, 90_000, 0.55, 2)
	sheet := makeDensityMedium(rng, "sheet-high", AABB{Min: V(-2.0, 2.2, 2.0), Max: V(2.3, 3.4, 6.5)},
		[3]int{8, 3, 9}, 85_000, 70_000, -0.1, 3)

	s := &Scene{
		Name:  "deterministic-real-time-fog",
		Seed:  opts.Seed,
		Width: opts.Width, Height: opts.Height,
		Media:         []Medium{calibration, haze, puff, sheet},
		Recipe:        r,
		SnapshotLabel: "seed-" + itoa(opts.Seed),
	}
	s.Cameras = []Camera{
		{LogicalTime: 0, Position: V(0, 0, 0), Yaw: 0, Pitch: 0, FOVY: 0.9, Aspect: float64(opts.Width) / float64(opts.Height)},
		{LogicalTime: 10, Position: V(0.08, 0.02, -0.05), Yaw: 0.035, Pitch: 0.01, FOVY: 0.92, Aspect: float64(opts.Width) / float64(opts.Height)},
		{LogicalTime: 20, Position: V(0.16, 0.05, -0.1), Yaw: 0.07, Pitch: 0.02, FOVY: 0.94, Aspect: float64(opts.Width) / float64(opts.Height)},
		{LogicalTime: 30, Position: V(0.24, 0.09, -0.15), Yaw: 0.105, Pitch: 0.03, FOVY: 0.96, Aspect: float64(opts.Width) / float64(opts.Height)},
	}
	s.Truth = makeTruthRays(&s.Recipe)
	return s
}

func makeDensityMedium(rng *rng64, id string, bounds AABB, res [3]int, absFP, scatFP int64, g float64, priority int) Medium {
	m := Medium{
		ID: id, Bounds: bounds, DensityRes: res,
		AbsorptionFP: absFP, ScatteringFP: scatFP,
		Anisotropy: g, Priority: priority,
	}
	m.Density = make([]int32, res[0]*res[1]*res[2])
	for z := 0; z < res[2]; z++ {
		for y := 0; y < res[1]; y++ {
			for x := 0; x < res[0]; x++ {
				fx := (float64(x)+0.5)/float64(res[0]) - 0.5
				fy := (float64(y)+0.5)/float64(res[1]) - 0.5
				fz := (float64(z)+0.5)/float64(res[2]) - 0.5
				r2 := fx*fx + fy*fy + fz*fz
				noise := (rng.float64() - 0.5) * 0.22
				d := 360 + 340*(1-r2) + noise*900
				if d < 0 {
					d = 0
				}
				if d > float64(DensityMax) {
					d = float64(DensityMax)
				}
				m.Density[m.VoxelIndex(x, y, z)] = int32(math.Round(d))
			}
		}
	}
	return m
}

func uniformMedium(id string, bounds AABB, res [3]int, density int32, absFP, scatFP int64, g float64, priority int) Medium {
	n := res[0] * res[1] * res[2]
	d := make([]int32, n)
	for i := range d {
		d[i] = density
	}
	return Medium{
		ID: id, Bounds: bounds, DensityRes: res, Density: d,
		AbsorptionFP: absFP, ScatteringFP: scatFP,
		Anisotropy: g, Priority: priority,
	}
}

func makeTruthRays(r *Recipe) []TruthRay {
	cal := uniformMedium("calibration", AABB{Min: V(-1, -1, 2), Max: V(1, 1, 4)},
		[3]int{4, 4, 4}, 600, 120_000, 80_000, 0.2, 0)
	rays := []TruthRay{
		{ID: "truth-0", CameraTime: 0, X: 4, Y: 3, Origin: V(0, 0, 1), Direction: V(0, 0, 1), T0: 1, T1: 3},
		{ID: "truth-1", CameraTime: 10, X: 8, Y: 3, Origin: V(0.1, 0, 1), Direction: V(0, 0, 1), T0: 1, T1: 3},
		{ID: "truth-2", CameraTime: 20, X: 4, Y: 7, Origin: V(0, 0.1, 1), Direction: V(0, 0, 1), T0: 1, T1: 3},
		{ID: "truth-3", CameraTime: 30, X: 12, Y: 3, Origin: V(-0.2, 0.1, 1), Direction: V(0, 0, 1), T0: 1, T1: 3},
	}
	for i := range rays {
		orig := rays[i]
		orig.Direction = orig.Direction.Normalize()
		t0 := 1.0
		t1 := 3.0
		ext := float64(cal.AbsorptionFP+cal.ScatteringFP) / float64(r.CoefficientScale)
		ext *= 0.6 // fixed density 600 / 1000
		scat := float64(cal.ScatteringFP) / float64(r.CoefficientScale) * 0.6
		phase := EvalPhase(r.Phase, orig.Direction.Dot(r.LightDirection.Normalize()), cal.Anisotropy)
		src := scat * phase * r.LightIntensity
		trans := math.Exp(-ext * (t1 - t0))
		scattered := 0.0
		if ext > 1e-15 {
			scattered = src / ext * (1 - trans)
		} else {
			scattered = src * (t1 - t0)
		}
		orig.T0, orig.T1 = t0, t1
		orig.Transmittance, orig.Scattered = trans, scattered
		rays[i] = orig
	}
	return rays
}

type rng64 struct {
	state uint64
}

func newRNG(seed uint64) *rng64 {
	return &rng64{state: seed + 0x9e3779b97f4a7c15}
}

func (r *rng64) next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (r *rng64) float64() float64 {
	return float64(r.next()>>11) / float64(uint64(1)<<53)
}

func itoa(v int64) string {
	// Kept local to avoid pulling formatting into deterministic labels in
	// callers that only have an int64.
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [24]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
