package fogkernel

import (
	"encoding/json"
	"math"
	"math/rand"
)

const DefaultSeed int64 = 420261126

type GeneratedBundle struct {
	Seed             int64                      `json:"seed"`
	Canonical        *Scene                     `json:"canonical"`
	Docs             map[string]json.RawMessage `json:"docs"`
	Truth            *TruthSet                  `json:"truth"`
	ConversionAudits []ConversionAudit          `json:"conversion_audits"`
}

func GenerateBundle(seed int64) (*GeneratedBundle, error) {
	scene := generateCanonical(seed)
	doc1, err := ExportV1(scene)
	if err != nil {
		return nil, err
	}
	doc2, err := ExportV2(scene)
	if err != nil {
		return nil, err
	}
	doc3, err := ExportV3(scene)
	if err != nil {
		return nil, err
	}
	truth := BuildTruth(scene)
	audits := CriticalConversionAudits()
	return &GeneratedBundle{
		Seed: seed, Canonical: scene, Truth: truth, ConversionAudits: audits,
		Docs: map[string]json.RawMessage{
			SchemaV1: doc1, SchemaV2: doc2, SchemaV3: doc3,
		},
	}, nil
}

func GenerateDefaultBundle() (*GeneratedBundle, error) {
	return GenerateBundle(DefaultSeed)
}

func mustMM(v int64) int64 {
	q, err := MMToMicro(v, "$.generator")
	if err != nil {
		panic(err)
	}
	return q
}

func generateCanonical(seed int64) *Scene {
	rng := rand.New(rand.NewSource(seed))
	recipe := Recipe{
		ChunkSizeMicro:          mustMM(500),
		BlockSampleLimit:        64,
		TransmittanceToleranceQ: DefaultTolQ,
		TerminationThresholdQ:   150_000_000,
		PreviewStride:           2,
		PreviewErrorBoundQ:      PreviewBoundQ,
		DefaultWorkers:          4,
		DefaultBuffer:           64,
	}
	lightQ := [3]int64{-400_000, 800_000, -400_000}
	scene := &Scene{
		ProjectID:    "transparent-volume-fog-deterministic",
		Seed:         seed,
		Recipe:       recipe,
		LightVectorQ: lightQ,
		LightDir:     lightDir(lightQ),
		Cameras: []CameraSample{
			{LogicalTime: 1, PositionMicro: Vec3I{X: mustMM(-400), Y: mustMM(0), Z: mustMM(-3000)}, YawMilliDeg: -6000, PitchMilliDeg: 0, FovYMilliDeg: 45_000, AspectQ: CoeffScale},
			{LogicalTime: 2, PositionMicro: Vec3I{X: mustMM(0), Y: mustMM(200), Z: mustMM(-3000)}, YawMilliDeg: 0, PitchMilliDeg: 0, FovYMilliDeg: 45_000, AspectQ: CoeffScale},
			{LogicalTime: 3, PositionMicro: Vec3I{X: mustMM(400), Y: mustMM(900), Z: mustMM(-3000)}, YawMilliDeg: 5000, PitchMilliDeg: -2000, FovYMilliDeg: 45_000, AspectQ: CoeffScale},
			{LogicalTime: 4, PositionMicro: Vec3I{X: mustMM(0), Y: mustMM(2800), Z: mustMM(-3000)}, YawMilliDeg: 0, PitchMilliDeg: 3_000, FovYMilliDeg: 45_000, AspectQ: CoeffScale},
		},
	}

	alphaDims := Vec3I{X: 5, Y: 3, Z: 3}
	alphaValues := make([]int, alphaDims.X*alphaDims.Y*alphaDims.Z)
	for z := 0; z < int(alphaDims.Z); z++ {
		for y := 0; y < int(alphaDims.Y); y++ {
			for x := 0; x < int(alphaDims.X); x++ {
				v := 125 +
					34*math.Sin(float64(x+1)*1.25+float64(z)*.35) +
					18*math.Cos(float64(y+1)*1.7+float64(z)) +
					float64(rng.Intn(9)-4)
				alphaValues[DensityIndex(alphaDims, x, y, z)] = clampDensity(int(math.Round(v)))
			}
		}
	}
	betaDims := Vec3I{X: 3, Y: 3, Z: 3}
	betaValues := make([]int, betaDims.X*betaDims.Y*betaDims.Z)
	for z := 0; z < int(betaDims.Z); z++ {
		for y := 0; y < int(betaDims.Y); y++ {
			for x := 0; x < int(betaDims.X); x++ {
				v := 150 +
					28*math.Cos(float64(x+z)*1.4) +
					22*math.Sin(float64(y+1)*1.1+float64(z)) +
					float64(rng.Intn(9)-4)
				betaValues[DensityIndex(betaDims, x, y, z)] = clampDensity(int(math.Round(v)))
			}
		}
	}

	scene.Media = []*Medium{
		{
			ID: "fog-alpha", MinMicro: Vec3I{X: mustMM(-2500), Y: mustMM(-1500), Z: 0},
			MaxMicro: Vec3I{X: mustMM(2500), Y: mustMM(1500), Z: mustMM(3000)}, VoxelMicro: mustMM(1000),
			Density: DensityField{Size: alphaDims, Values: alphaValues, Max: 255},
			SigmaAQ: 180_000, SigmaSQ: 760_000,
			Phase: PhaseFunction{Type: "henyey-greenstein", GQ: 250_000},
		},
		{
			ID: "fog-beta", MinMicro: Vec3I{X: mustMM(-1500), Y: mustMM(-1500), Z: mustMM(4000)},
			MaxMicro: Vec3I{X: mustMM(1500), Y: mustMM(1500), Z: mustMM(7000)}, VoxelMicro: mustMM(1000),
			Density: DensityField{Size: betaDims, Values: betaValues, Max: 255},
			SigmaAQ: 240_000, SigmaSQ: 900_000,
			Phase: PhaseFunction{Type: "henyey-greenstein", GQ: -150_000},
		},
	}
	scene.Rays = BuildRays(scene)
	return scene
}

func DensityIndex(size Vec3I, x, y, z int) int {
	return x + int(size.X)*(y+int(size.Y)*z)
}

func clampDensity(v int) int {
	if v < 8 {
		return 8
	}
	if v > 248 {
		return 248
	}
	return v
}

func treeBlocks(field DensityField) []treeBlock {
	const bx, by, bz = 2, 2, 1
	dims := field.Size
	var out []treeBlock
	for z0 := 0; z0 < int(dims.Z); z0 += bz {
		for y0 := 0; y0 < int(dims.Y); y0 += by {
			for x0 := 0; x0 < int(dims.X); x0 += bx {
				sx := minInt(bx, int(dims.X)-x0)
				sy := minInt(by, int(dims.Y)-y0)
				sz := minInt(bz, int(dims.Z)-z0)
				values := make([]int, 0, sx*sy*sz)
				for z := z0; z < z0+sz; z++ {
					for y := y0; y < y0+sy; y++ {
						for x := x0; x < x0+sx; x++ {
							values = append(values, field.Get(x, y, z))
						}
					}
				}
				out = append(out, treeBlock{Origin: [3]int{x0, y0, z0}, Size: [3]int{sx, sy, sz}, Values: values})
			}
		}
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func ExportV1(s *Scene) (json.RawMessage, error) {
	cameras := make([]dtoCameraV1, 0, len(s.Cameras))
	for _, c := range s.Cameras {
		pos, err := microVecToMM3(c.PositionMicro)
		if err != nil {
			return nil, err
		}
		cameras = append(cameras, dtoCameraV1{
			LogicalTime: c.LogicalTime, PositionMM: pos, YawMilliDeg: c.YawMilliDeg,
			PitchMilliDeg: c.PitchMilliDeg, FovYMilliDeg: c.FovYMilliDeg, AspectQ: c.AspectQ,
		})
	}
	vols := make([]dtoVolumeV1, 0, len(s.Media))
	for _, m := range s.Media {
		mn, err := microVecToMM3(m.MinMicro)
		if err != nil {
			return nil, err
		}
		mx, err := microVecToMM3(m.MaxMicro)
		if err != nil {
			return nil, err
		}
		voxel, err := MicroToMM(m.VoxelMicro, "$.export.voxel")
		if err != nil {
			return nil, err
		}
		vols = append(vols, dtoVolumeV1{
			ID: m.ID, BoundsMinMM: mn, BoundsMaxMM: mx, VoxelSizeMM: voxel,
			Size:          [3]int{int(m.Density.Size.X), int(m.Density.Size.Y), int(m.Density.Size.Z)},
			DensityValues: append([]int(nil), m.Density.Values...), DensityMax: m.Density.Max,
			SigmaAPermille: QToPermille(m.SigmaAQ), SigmaSPermille: QToPermille(m.SigmaSQ),
			AnisotropyPermille: QToPermille(m.Phase.GQ),
		})
	}
	chunk, err := MicroToMM(s.Recipe.ChunkSizeMicro, "$.export.chunk")
	if err != nil {
		return nil, err
	}
	doc := dtoV1{
		Schema: SchemaV1, ProjectID: s.ProjectID, Seed: s.Seed,
		Recipe: dtoRecipeV1{
			ChunkSizeMM: chunk, BlockSampleLimit: s.Recipe.BlockSampleLimit,
			TransmittanceToleranceQ: s.Recipe.TransmittanceToleranceQ,
			TerminationThresholdQ:   s.Recipe.TerminationThresholdQ,
			PreviewStride:           s.Recipe.PreviewStride, PreviewErrorBoundQ: s.Recipe.PreviewErrorBoundQ,
			DefaultWorkers: s.Recipe.DefaultWorkers, DefaultBuffer: s.Recipe.DefaultBuffer,
		},
		LightVectorQ: s.LightVectorQ, Cameras: cameras, Volumes: vols,
	}
	return json.MarshalIndent(doc, "", "  ")
}

func microVecToMM3(v Vec3I) ([3]int64, error) {
	x, err := MicroToMM(v.X, "$.export.x")
	if err != nil {
		return [3]int64{}, err
	}
	y, err := MicroToMM(v.Y, "$.export.y")
	if err != nil {
		return [3]int64{}, err
	}
	z, err := MicroToMM(v.Z, "$.export.z")
	if err != nil {
		return [3]int64{}, err
	}
	return [3]int64{x, y, z}, nil
}

func meter3(v Vec3I) [3]DecimalMeters {
	return [3]DecimalMeters{
		DecimalMeters(MicroToMetersString(v.X)),
		DecimalMeters(MicroToMetersString(v.Y)),
		DecimalMeters(MicroToMetersString(v.Z)),
	}
}

func ExportV2(s *Scene) (json.RawMessage, error) {
	cameras := make([]dtoCameraV2, 0, len(s.Cameras))
	for _, c := range s.Cameras {
		cameras = append(cameras, dtoCameraV2{
			LogicalTime: c.LogicalTime, PositionM: meter3(c.PositionMicro),
			YawMilliDeg: c.YawMilliDeg, PitchMilliDeg: c.PitchMilliDeg,
			FovYMilliDeg: c.FovYMilliDeg, AspectQ: c.AspectQ,
		})
	}
	media := make([]dtoVolumeV2, 0, len(s.Media))
	for _, m := range s.Media {
		blocks1 := treeBlocks(m.Density)
		blocks := make([]dtoBlockV2, 0, len(blocks1))
		for _, b := range blocks1 {
			blocks = append(blocks, dtoBlockV2{BlockOrigin: b.Origin, Size: b.Size, Values: b.Values})
		}
		media = append(media, dtoVolumeV2{
			ID: m.ID, BoundsMinM: meter3(m.MinMicro), BoundsMaxM: meter3(m.MaxMicro),
			VoxelSizeM: DecimalMeters(MicroToMetersString(m.VoxelMicro)),
			VoxelGrid: dtoGridV2{
				Dimensions: [3]int{int(m.Density.Size.X), int(m.Density.Size.Y), int(m.Density.Size.Z)},
				DensityMax: m.Density.Max, Blocks: blocks,
			},
			SigmaAPermyriad: QToPermyriad(m.SigmaAQ), SigmaSPermyriad: QToPermyriad(m.SigmaSQ),
			AnisotropyPermyriad: QToPermyriad(m.Phase.GQ),
			PhaseFunction:       &dtoPhaseV2{Type: m.Phase.Type},
		})
	}
	doc := dtoV2{
		SchemaVersion: SchemaV2, ProjectID: s.ProjectID, Seed: s.Seed,
		Recipe: dtoRecipeM{
			ChunkSizeM:              DecimalMeters(MicroToMetersString(s.Recipe.ChunkSizeMicro)),
			BlockSampleLimit:        s.Recipe.BlockSampleLimit,
			TransmittanceToleranceQ: s.Recipe.TransmittanceToleranceQ,
			TerminationThresholdQ:   s.Recipe.TerminationThresholdQ,
			PreviewStride:           s.Recipe.PreviewStride, PreviewErrorBoundQ: s.Recipe.PreviewErrorBoundQ,
			DefaultWorkers: s.Recipe.DefaultWorkers, DefaultBuffer: s.Recipe.DefaultBuffer,
		},
		LightVectorQ: s.LightVectorQ, CameraTracks: cameras, Media: media,
	}
	return json.MarshalIndent(doc, "", "  ")
}

func ExportV3(s *Scene) (json.RawMessage, error) {
	cameras := make([]dtoCameraV3, 0, len(s.Cameras))
	for _, c := range s.Cameras {
		cameras = append(cameras, dtoCameraV3{
			LogicalTime: c.LogicalTime, PositionM: meter3(c.PositionMicro),
			YawMilliDeg: c.YawMilliDeg, PitchMilliDeg: c.PitchMilliDeg,
			FovYMilliDeg: c.FovYMilliDeg, AspectQ: c.AspectQ,
		})
	}
	vols := make([]dtoVolumeV3, 0, len(s.Media))
	for _, m := range s.Media {
		blocks1 := treeBlocks(m.Density)
		blocks := make([]dtoBlockV3, 0, len(blocks1))
		for _, b := range blocks1 {
			blocks = append(blocks, dtoBlockV3{BlockOrigin: b.Origin, Size: b.Size, Values: b.Values})
		}
		vols = append(vols, dtoVolumeV3{
			ID: m.ID, BoundsMinM: meter3(m.MinMicro), BoundsMaxM: meter3(m.MaxMicro),
			VoxelSizeM: DecimalMeters(MicroToMetersString(m.VoxelMicro)),
			VoxelTree: dtoGridV3{
				Dimensions: [3]int{int(m.Density.Size.X), int(m.Density.Size.Y), int(m.Density.Size.Z)},
				DensityMax: m.Density.Max, Blocks: blocks,
			},
			SigmaAPermille: QToPermille(m.SigmaAQ), SigmaSPermille: QToPermille(m.SigmaSQ),
			AnisotropyPermille: QToPermille(m.Phase.GQ),
			PhaseFunction:      &dtoPhaseV3{Type: m.Phase.Type},
		})
	}
	doc := dtoV3{
		APIVersion: SchemaV3, ProjectID: s.ProjectID, Seed: s.Seed,
		Recipe: dtoRecipeV3{
			ChunkSizeM:              DecimalMeters(MicroToMetersString(s.Recipe.ChunkSizeMicro)),
			BlockSampleLimit:        s.Recipe.BlockSampleLimit,
			TransmittanceToleranceQ: s.Recipe.TransmittanceToleranceQ,
			TerminationThresholdQ:   s.Recipe.TerminationThresholdQ,
			PreviewStride:           s.Recipe.PreviewStride, PreviewErrorBoundQ: s.Recipe.PreviewErrorBoundQ,
			DefaultWorkers: s.Recipe.DefaultWorkers, DefaultBuffer: s.Recipe.DefaultBuffer,
		},
		LightVectorQ: s.LightVectorQ, Cameras: cameras, Volumes: vols,
	}
	return json.MarshalIndent(doc, "", "  ")
}
