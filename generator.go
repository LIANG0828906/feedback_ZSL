package volumefog

import (
	"fmt"
	"math"
)

const DefaultSeed int64 = 0x5EED2026

type GenerateOptions struct {
	Seed      int64
	Width     int
	Height    int
	ChunkSize int
}

func DefaultGenerateOptions() GenerateOptions {
	return GenerateOptions{Seed: DefaultSeed, Width: 32, Height: 16, ChunkSize: 8}
}

// GenerateScene creates the deterministic acceptance scene. The seed controls
// every voxel; no external sample or network input is consulted.
func GenerateScene(opts GenerateOptions) (*Scene, error) {
	if opts.Seed == 0 {
		opts.Seed = DefaultSeed
	}
	if opts.Width <= 0 || opts.Height <= 0 || opts.ChunkSize <= 0 {
		return nil, schemaErr("", "generate", "invalid generation dimensions")
	}
	scene := &Scene{
		Schema:           SchemaVersion,
		Seed:             opts.Seed,
		FixedScale:       FixedScale,
		CoefficientScale: CoeffScale,
		ChunkSize:        opts.ChunkSize,
		ActiveCamera:     0,
		Recipe: Recipe{
			Steps:                128,
			PreviewSteps:         8,
			PhaseType:            "hg",
			ShadowSamples:        64,
			PreviewShadowSamples: 8,
			TransmittanceCutoff:  1e-10,
			MaxParallel:          4,
			RayBuffer:            4,
			ShadowBudget:         64,
			PreviewLatencyMS:     1500,
			LightDir:             [3]float64{0, 0, 1},
			AmbientColor:         [3]float64{1.0, 0.86, 0.70},
		},
	}
	scene.Media = []Medium{
		makeMedium(opts.Seed, "fog_a",
			Vec3i{-2048, -1536, 2048}, Vec3i{2048, 1536, 10240}, [3]int{16, 12, 16},
			120_000, 280_000, 250_000, 0),
		makeMedium(opts.Seed, "fog_b",
			Vec3i{-1024, -1024, 6144}, Vec3i{3072, 2048, 14336}, [3]int{16, 12, 16},
			90_000, 220_000, -350_000, 17),
		makeMedium(opts.Seed, "fog_c",
			Vec3i{-3072, -2048, 9216}, Vec3i{-1024, 0, 12288}, [3]int{8, 8, 8},
			80_000, 120_000, 600_000, 31),
	}
	for i := range scene.Media {
		scene.Media[i].RefreshDensityEncoding()
	}
	scene.Cameras = []CameraSample{
		{
			TimeUS: 0, PositionQ: Vec3i{0, 0, 0}, OrientationMdeg: Vec3i{0, 0, 0},
			ViewWidthQ: 8192, ViewHeightQ: 6144, FovDeg: 67.380, NearQ: 256, FarQ: 18432,
			Width: opts.Width, Height: opts.Height,
		},
		{
			TimeUS: 500_000, PositionQ: Vec3i{256, -128, 512}, OrientationMdeg: Vec3i{0, 0, 0},
			ViewWidthQ: 8192, ViewHeightQ: 6144, FovDeg: 67.380, NearQ: 256, FarQ: 18432,
			Width: opts.Width, Height: opts.Height,
		},
		{
			TimeUS: 1_000_000, PositionQ: Vec3i{-256, 128, 1024}, OrientationMdeg: Vec3i{0, 0, 0},
			ViewWidthQ: 8192, ViewHeightQ: 6144, FovDeg: 67.380, NearQ: 256, FarQ: 18432,
			Width: opts.Width, Height: opts.Height,
		},
	}
	if err := ValidateScene(scene); err != nil {
		return nil, err
	}
	truth, err := GenerateTruth(scene, SampleRays(scene))
	if err != nil {
		return nil, err
	}
	scene.Truth = truth
	return scene, nil
}

func makeMedium(seed int64, id string, min, max Vec3i, res [3]int, absQ, scatQ, anisoQ int64, salt uint64) Medium {
	m := Medium{
		ID: id, MinQ: min, MaxQ: max, DensityRes: res,
		AbsorptionQ: absQ, ScatteringQ: scatQ, AnisotropyQ: anisoQ,
	}
	n := m.VoxelCount()
	m.Density = make([]int, n)
	for z := 0; z < res[2]; z++ {
		for y := 0; y < res[1]; y++ {
			for x := 0; x < res[0]; x++ {
				m.Density[m.DensityIndex(x, y, z)] = voxelDensity(seed, salt, x, y, z)
			}
		}
	}
	return m
}

func voxelDensity(seed int64, salt uint64, x, y, z int) int {
	h := uint64(seed) ^ (salt * 0x9E3779B97F4A7C15)
	h ^= uint64(x+1) * 0xBF58476D1CE4E5B9
	h ^= uint64(y+3) * 0x94D049BB133111EB
	h ^= uint64(z+7) * 0xD6E8FEB86659FD93
	h ^= h >> 30
	h *= 0xBF58476D1CE4E5B9
	h ^= h >> 27
	h *= 0x94D049BB133111EB
	h ^= h >> 31
	noise := int(h % 43)
	wave := (x*17 + y*29 + z*37 + int(seed&0x3f) + int(salt)) % 61
	v := 54 + wave + noise
	return int(math.Max(0, math.Min(MaxDensity, float64(v))))
}

// SampleRays returns the deterministic analytical comparison rays.
func SampleRays(scene *Scene) []Ray {
	cam := scene.ActiveCameraSample()
	if cam == nil {
		return nil
	}
	pixels := [][2]int{
		{2, 2}, {10, 3}, {18, 5}, {27, 9}, {7, 12}, {24, 13}, {15, 8},
	}
	out := make([]Ray, 0, len(pixels))
	for i, p := range pixels {
		rayID := fmt.Sprintf("sample:%d:pixel:%d,%d", i, p[0], p[1])
		out = append(out, CameraRay(*cam, scene.ActiveCamera, p[0], p[1], rayID))
	}
	return out
}
