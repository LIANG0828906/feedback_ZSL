package fogkernel

import "fmt"

func signedScaleToQ(raw int64, scale ScaleKind, path string) (int64, error) {
	multiplier := int64(0)
	switch scale {
	case ScalePermille:
		multiplier = CoeffScale / 1000
	case ScalePermyriad:
		multiplier = CoeffScale / 10000
	default:
		return 0, NewBizError(CodeSchemaCombination, path, "unknown anisotropy scale %q", scale)
	}
	q, ok := checkedMul(raw, multiplier)
	if !ok {
		return 0, NewBizError(CodeFixedOverflow, path, "anisotropy conversion overflows int64").
			WithContext("raw", raw, "scale", string(scale))
	}
	if q < -CoeffScale || q > CoeffScale {
		return 0, NewBizError(CodeSchemaField, path, "anisotropy must remain in [-1,1] after conversion").
			WithContext("raw", raw, "q", q)
	}
	return q, nil
}

func buildV1(doc *dtoV1) (*Scene, error) {
	if doc.Schema != SchemaV1 {
		return nil, NewBizError(CodeSchemaVersion, "$.Schema", "expected %s, got %q", SchemaV1, doc.Schema)
	}
	recipe, err := buildRecipeV1(&doc.Recipe)
	if err != nil {
		return nil, err
	}
	cameras, err := buildCamerasV1(doc.Cameras)
	if err != nil {
		return nil, err
	}
	media := make([]*Medium, 0, len(doc.Volumes))
	for i, v := range doc.Volumes {
		p := fmt.Sprintf("$.Volumes[%d]", i)
		mn, err := vecMM(v.BoundsMinMM, p+".BoundsMinMM")
		if err != nil {
			return nil, err
		}
		mx, err := vecMM(v.BoundsMaxMM, p+".BoundsMaxMM")
		if err != nil {
			return nil, err
		}
		voxel, err := MMToMicro(v.VoxelSizeMM, p+".VoxelSizeMM")
		if err != nil {
			return nil, err
		}
		density, err := flatDensity(v.Size, v.DensityValues, v.DensityMax, p+".DensityValues")
		if err != nil {
			return nil, err
		}
		if err := validateAABBDensity(v.ID, mn, mx, voxel, density, p); err != nil {
			return nil, err
		}
		sa, err := CoefficientToQ(v.SigmaAPermille, ScalePermille, p+".SigmaAPermille")
		if err != nil {
			return nil, err
		}
		ss, err := CoefficientToQ(v.SigmaSPermille, ScalePermille, p+".SigmaSPermille")
		if err != nil {
			return nil, err
		}
		gq, err := signedScaleToQ(v.AnisotropyPermille, ScalePermille, p+".AnisotropyPermille")
		if err != nil {
			return nil, err
		}
		g, err := phaseG(gq, p+".AnisotropyPermille")
		if err != nil {
			return nil, err
		}
		media = append(media, &Medium{
			ID: v.ID, MinMicro: mn, MaxMicro: mx, VoxelMicro: voxel,
			Density: density, SigmaAQ: sa, SigmaSQ: ss, Phase: g,
		})
	}
	scene := &Scene{
		ProjectID: doc.ProjectID, Seed: doc.Seed, Recipe: recipe,
		LightVectorQ: doc.LightVectorQ, LightDir: lightDir(doc.LightVectorQ), Media: media, Cameras: cameras,
	}
	scene.Rays = BuildRays(scene)
	return scene, nil
}

func buildV2(doc *dtoV2) (*Scene, error) {
	if doc.SchemaVersion != SchemaV2 {
		return nil, NewBizError(CodeSchemaVersion, "$.schema_version", "expected %s, got %q", SchemaV2, doc.SchemaVersion)
	}
	r := doc.Recipe
	recipe, err := buildRecipeM(r.ChunkSizeM, r.BlockSampleLimit, r.TransmittanceToleranceQ,
		r.TerminationThresholdQ, r.PreviewStride, r.PreviewErrorBoundQ,
		r.DefaultWorkers, r.DefaultBuffer, "$.recipe.chunk_size_m")
	if err != nil {
		return nil, err
	}
	cameras, err := buildCamerasV2(doc.CameraTracks)
	if err != nil {
		return nil, err
	}
	media := make([]*Medium, 0, len(doc.Media))
	for i, v := range doc.Media {
		p := fmt.Sprintf("$.media[%d]", i)
		mn, err := vecMeters(v.BoundsMinM, p+".aabb_min_m")
		if err != nil {
			return nil, err
		}
		mx, err := vecMeters(v.BoundsMaxM, p+".aabb_max_m")
		if err != nil {
			return nil, err
		}
		voxel, err := DecimalMetersToMicro(string(v.VoxelSizeM), p+".voxel_size_m")
		if err != nil {
			return nil, err
		}
		density, err := flattenTree(v.VoxelGrid.Dimensions, v.VoxelGrid.DensityMax, v.VoxelGrid.Blocks, p+".voxel_grid")
		if err != nil {
			return nil, err
		}
		if err := validateAABBDensity(v.ID, mn, mx, voxel, density, p); err != nil {
			return nil, err
		}
		sa, err := CoefficientToQ(v.SigmaAPermyriad, ScalePermyriad, p+".sigma_a_permyriad")
		if err != nil {
			return nil, err
		}
		ss, err := CoefficientToQ(v.SigmaSPermyriad, ScalePermyriad, p+".sigma_s_permyriad")
		if err != nil {
			return nil, err
		}
		gq, err := signedScaleToQ(v.AnisotropyPermyriad, ScalePermyriad, p+".anisotropy_permyriad")
		if err != nil {
			return nil, err
		}
		phase, err := phaseG(gq, p+".anisotropy_permyriad")
		if err != nil {
			return nil, err
		}
		if v.PhaseFunction != nil && v.PhaseFunction.Type != "" {
			phase.Type = v.PhaseFunction.Type
		}
		media = append(media, &Medium{
			ID: v.ID, MinMicro: mn, MaxMicro: mx, VoxelMicro: voxel,
			Density: density, SigmaAQ: sa, SigmaSQ: ss, Phase: phase,
		})
	}
	scene := &Scene{
		ProjectID: doc.ProjectID, Seed: doc.Seed, Recipe: recipe,
		LightVectorQ: doc.LightVectorQ, LightDir: lightDir(doc.LightVectorQ), Media: media, Cameras: cameras,
	}
	scene.Rays = BuildRays(scene)
	return scene, nil
}

func buildV3(doc *dtoV3) (*Scene, error) {
	if doc.APIVersion != SchemaV3 {
		return nil, NewBizError(CodeSchemaVersion, "$.apiVersion", "expected %s, got %q", SchemaV3, doc.APIVersion)
	}
	r := doc.Recipe
	recipe, err := buildRecipeM(r.ChunkSizeM, r.BlockSampleLimit, r.TransmittanceToleranceQ,
		r.TerminationThresholdQ, r.PreviewStride, r.PreviewErrorBoundQ,
		r.DefaultWorkers, r.DefaultBuffer, "$.recipe.chunkSizeM")
	if err != nil {
		return nil, err
	}
	cameras, err := buildCamerasV3(doc.Cameras)
	if err != nil {
		return nil, err
	}
	media := make([]*Medium, 0, len(doc.Volumes))
	for i, v := range doc.Volumes {
		p := fmt.Sprintf("$.volumes[%d]", i)
		mn, err := vecMeters(v.BoundsMinM, p+".boundsMinM")
		if err != nil {
			return nil, err
		}
		mx, err := vecMeters(v.BoundsMaxM, p+".boundsMaxM")
		if err != nil {
			return nil, err
		}
		voxel, err := DecimalMetersToMicro(string(v.VoxelSizeM), p+".voxelSizeM")
		if err != nil {
			return nil, err
		}
		density, err := flattenTreeV3(v.VoxelTree.Dimensions, v.VoxelTree.DensityMax, v.VoxelTree.Blocks, p+".voxelTree")
		if err != nil {
			return nil, err
		}
		if err := validateAABBDensity(v.ID, mn, mx, voxel, density, p); err != nil {
			return nil, err
		}
		sa, err := CoefficientToQ(v.SigmaAPermille, ScalePermille, p+".sigmaAPermille")
		if err != nil {
			return nil, err
		}
		ss, err := CoefficientToQ(v.SigmaSPermille, ScalePermille, p+".sigmaSPermille")
		if err != nil {
			return nil, err
		}
		gq, err := signedScaleToQ(v.AnisotropyPermille, ScalePermille, p+".anisotropyPermille")
		if err != nil {
			return nil, err
		}
		phase, err := phaseG(gq, p+".anisotropyPermille")
		if err != nil {
			return nil, err
		}
		if v.PhaseFunction != nil && v.PhaseFunction.Type != "" {
			phase.Type = v.PhaseFunction.Type
		}
		media = append(media, &Medium{
			ID: v.ID, MinMicro: mn, MaxMicro: mx, VoxelMicro: voxel,
			Density: density, SigmaAQ: sa, SigmaSQ: ss, Phase: phase,
		})
	}
	scene := &Scene{
		ProjectID: doc.ProjectID, Seed: doc.Seed, Recipe: recipe,
		LightVectorQ: doc.LightVectorQ, LightDir: lightDir(doc.LightVectorQ), Media: media, Cameras: cameras,
	}
	scene.Rays = BuildRays(scene)
	return scene, nil
}
