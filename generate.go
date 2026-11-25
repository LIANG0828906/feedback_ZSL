package fog

import (
	"fmt"
	"math"
	"math/rand"
)

const GeneratorSeed int64 = 0x464f4731 // deterministic ASCII "FOG1"

func GenerateSample() (*GeneratedSample, error) {
	rng := rand.New(rand.NewSource(GeneratorSeed))
	scene := Scene{
		SchemaVersion: "fog-kernel/v1",
		Seed:          GeneratorSeed,
		FixedScale:    FixedScale,
		Viewport:      Viewport{Width: 32, Height: 18},
		ActiveCamera:  0,
		CameraTrack: CameraTrack{
			ID:      "camera-track-main",
			Version: 3,
			Samples: generateCameraTrack(),
		},
		Light: Light{
			Direction: Vec3{X: -0.38, Y: 0.86, Z: 0.34}.Normalize(),
			Color:     [3]float64{1.0, 0.95, 0.86},
			Intensity: 11.0,
			Ambient:   [3]float64{0.30, 0.38, 0.52},
		},
		Recipe: Recipe{
			ID:                   "recipe-volumetric-v1",
			Version:              2,
			TileWidth:            8,
			TileHeight:           6,
			MaxStepsPerRay:       64,
			ShadowSamplesPerStep: 3,
			ShadowBudgetPerRay:   192,
			PreviewStepsPerRay:   18,
			PreviewShadowBudget:  42,
			PreviewDelayMS:       250,
			PhaseFunction:        PhaseHenyeyGreenstein,
		},
		Tolerances: Tolerances{
			SegmentTransmittance: 0.012,
			SegmentScattering:    0.025,
			PreviewPixel:         0.09,
			CheckpointPixel:      1e-10,
			Convergence:          0.02,
		},
	}

	scene.Media = []*Medium{
		generateMedium(rng, "fog-a", "Left stratocumulus slab", AABB{
			Min: fixedVecFromFloat(Vec3{X: -2.6, Y: 0.10, Z: -2.2}),
			Max: fixedVecFromFloat(Vec3{X: 0.4, Y: 2.60, Z: 1.8}),
		}, [3]int{6, 5, 8}, FixedFromFloat(0.5), 0.052, 0.165, 0.28, 1),
		generateMedium(rng, "fog-b", "Right cumulus tower", AABB{
			Min: fixedVecFromFloat(Vec3{X: -0.4, Y: 0.30, Z: -1.2}),
			Max: fixedVecFromFloat(Vec3{X: 2.6, Y: 2.80, Z: 2.3}),
		}, [3]int{6, 5, 7}, FixedFromFloat(0.5), 0.043, 0.145, -0.42, 2),
	}

	if err := ValidateScene(&scene); err != nil {
		return nil, fmt.Errorf("generated scene failed validation: %w", err)
	}
	truth, err := buildTruth(&scene)
	if err != nil {
		return nil, err
	}
	return &GeneratedSample{Scene: scene, Truth: *truth}, nil
}

func generateCameraTrack() []CameraSample {
	target := Vec3{X: 0, Y: 1.25, Z: 0}
	positions := []Vec3{
		{X: 0.0, Y: 1.45, Z: 6.4},
		{X: 0.18, Y: 1.50, Z: 6.25},
		{X: -0.12, Y: 1.42, Z: 6.55},
	}
	samples := make([]CameraSample, 0, len(positions))
	for i, position := range positions {
		forward := target.Sub(position).Normalize()
		samples = append(samples, CameraSample{
			LogicalTime: int64(i + 1),
			Position:    position,
			Forward:     forward,
			Up:          Vec3{X: 0, Y: 1, Z: 0},
			FovYDegrees: 46,
			Near:        FixedFromFloat(0.1),
			Far:         FixedFromFloat(40),
		})
	}
	return samples
}

func generateMedium(rng *rand.Rand, id, label string, bounds AABB, resolution [3]int, voxelSize Fixed, absorption, scattering, g float64, priority int) *Medium {
	count := resolution[0] * resolution[1] * resolution[2]
	values := make([]int, count)
	for z := 0; z < resolution[2]; z++ {
		for y := 0; y < resolution[1]; y++ {
			for x := 0; x < resolution[0]; x++ {
				nx := float64(x+1) / float64(resolution[0]+1)
				ny := float64(y+1) / float64(resolution[1]+1)
				nz := float64(z+1) / float64(resolution[2]+1)
				shape := 0.5 + 0.28*sinFloat(nx*6.0+nz*2.2) + 0.22*cosFloat(ny*7.0-nx*2.0)
				centerBias := 1.0 - 0.35*(nx-0.5)*(nx-0.5) - 0.25*(ny-0.5)*(ny-0.5)
				value := int(1.5 + 5.5*shape*centerBias + float64(rng.Intn(3)))
				if value < 0 {
					value = 0
				}
				if value > MaxDensityVoxel {
					value = MaxDensityVoxel
				}
				values[x+resolution[0]*(y+resolution[1]*z)] = value
			}
		}
	}
	return &Medium{
		ID:           id,
		Label:        label,
		Bounds:       bounds,
		Absorption:   FixedFromFloat(absorption),
		Scattering:   FixedFromFloat(scattering),
		Anisotropy:   g,
		DensityScale: 0.075,
		Density: &DensityField{
			MediumID:   id,
			Resolution: resolution,
			VoxelSize:  FixedVec3{X: voxelSize, Y: voxelSize, Z: voxelSize},
			Values:     values,
		},
		Priority: priority,
		Version:  1,
	}
}

func sinFloat(v float64) float64 {
	return math.Sin(v)
}

func cosFloat(v float64) float64 {
	return math.Cos(v)
}

func buildTruth(scene *Scene) (*TruthBundle, error) {
	bundle := &TruthBundle{}
	for y := 0; y < scene.Viewport.Height; y++ {
		for x := 0; x < scene.Viewport.Width; x++ {
			ray := RayForPixel(scene.ActiveSample(), scene.Viewport, x, y)
			truth, err := BuildAnalyticRayTruth(scene, ray)
			if err != nil {
				return nil, err
			}
			bundle.Rays = append(bundle.Rays, *truth)
		}
	}

	for _, tile := range TileGrid(scene.Viewport, scene.Recipe) {
		tileTruth := TileTruth{Tile: tile}
		for _, ray := range RaysForTile(*scene, tile) {
			for _, rt := range bundle.Rays {
				if rt.RayID == ray.ID {
					tileTruth.Pixels = append(tileTruth.Pixels, ExpectedPixel{
						X: ray.Pixel.X, Y: ray.Pixel.Y, Radiance: rt.Radiance, Color: rt.Color,
					})
					break
				}
			}
		}
		bundle.Tiles = append(bundle.Tiles, tileTruth)
	}
	return bundle, nil
}

func BuildAnalyticRayTruth(scene *Scene, ray Ray) (*RayTruth, error) {
	hits, err := IntersectRayMedia(ray, scene.Media)
	if err != nil {
		return nil, err
	}
	segments, err := BuildRaySegments(ray, hits)
	if err != nil {
		return nil, err
	}
	truth := &RayTruth{RayID: ray.ID, Pixel: ray.Pixel, Segments: make([]SegmentTruth, 0, len(segments))}
	cumulativeT := 1.0
	cumulativeScatter := 0.0

	for _, segment := range segments {
		near := segment.Near.Fixed.Float64()
		far := segment.Far.Fixed.Float64()
		boundaries := []float64{near, far}
		for _, id := range segment.MediumIDs {
			medium := scene.Medium(id)
			if medium != nil {
				boundaries = append(boundaries, voxelBoundaries(ray, medium, near, far)...)
			}
		}
		boundaries = uniqueSortedFloat(boundaries)
		segmentT := 1.0
		segmentScatter := 0.0

		for i := 0; i+1 < len(boundaries); i++ {
			a, b := boundaries[i], boundaries[i+1]
			if b <= a {
				continue
			}
			t := (a + b) / 2
			material := SampleMaterials(scene, ray, t, segment.MediumIDs)
			point := ray.Origin.Add(ray.Dir.Scale(t))
			shadowT := ShadowTransmittanceExact(scene, point)
			source := material.Ambient + material.PhaseScatter*scene.Light.Intensity*shadowT
			dt := (b - a) * ray.Dir.Len()
			tau := material.SigmaT * dt
			pieceT := expFloat(-tau)
			var scatter float64
			if material.SigmaT > 1e-12 {
				scatter = cumulativeT * source / material.SigmaT * (1 - pieceT)
			} else {
				scatter = cumulativeT * source * dt
			}
			cumulativeScatter += scatter
			segmentScatter += scatter
			cumulativeT *= pieceT
			segmentT *= pieceT
		}

		truth.Segments = append(truth.Segments, SegmentTruth{
			SegmentIndex:         segment.Index,
			MediumIDs:            append([]string(nil), segment.MediumIDs...),
			Near:                 segment.Near.Fixed,
			Far:                  segment.Far.Fixed,
			Transmittance:        segmentT,
			CumulativeT:          cumulativeT,
			Scattering:           segmentScatter,
			CumulativeScattering: cumulativeScatter,
		})
	}

	truth.FinalTransmittance = cumulativeT
	truth.Scattering = cumulativeScatter
	truth.Radiance = RadianceFor(scene, cumulativeT, cumulativeScatter)
	truth.Color = RadianceToColor(truth.Radiance)
	return truth, nil
}

func expFloat(v float64) float64 {
	return math.Exp(v)
}
