package bake

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"terrainbake/geometry"
	"terrainbake/model"
)

// BakeUnit computes one chunk at one solar time. It has no shared mutable
// state, so output is independent of worker order and concurrency.
func BakeUnit(t *geometry.Terrain, chunk *model.Chunk, sun model.SunSample, previousKey string, vertexBufferCap int) (*BakedUnit, error) {
	if t == nil || chunk == nil {
		return nil, model.NewError(model.CodeValue, "bake unit requires terrain and chunk")
	}
	if vertexBufferCap <= 0 {
		return nil, model.NewError(model.CodeValue, "vertex buffer cap must be positive")
	}
	p := t.Project()
	totalSkyWeight := 0.0
	scale := p.Params.FixedScale
	totalSkyWeightFixed := int64(0)
	for _, sky := range p.Sky {
		totalSkyWeight += sky.Weight
		weightFixed, _ := model.Quantize(sky.Weight, scale)
		totalSkyWeightFixed += weightFixed
	}
	if !(totalSkyWeight > 0) {
		return nil, model.NewError(model.CodeValue, "sky weight sum must be positive").WithField("sky")
	}
	if totalSkyWeightFixed <= 0 {
		return nil, model.NewError(model.CodeValue, "fixed sky weight sum must be positive").WithField("sky")
	}
	sunDir := geometry.Direction(sun.AzimuthDeg, sun.ElevationDeg)
	occluderSetHash := model.OccluderSetHash(p.Occluders)
	sunSin := math.Sin(sun.ElevationDeg * math.Pi / 180)
	sunSinFixed, _ := model.Quantize(sunSin, scale)
	var sunRadianceFixed [3]int64
	for ch := 0; ch < 3; ch++ {
		sunRadianceFixed[ch], _ = model.Quantize(sun.Radiance[ch], scale)
	}
	traceOpts := geometry.TraceOptions{
		Budget:     p.Params.ShadowStepBudget,
		Step:       p.Params.RayStep,
		Epsilon:    p.Params.RayEpsilon,
		RecordPath: true,
	}
	unit := &BakedUnit{
		ChunkID:          chunk.ID,
		Time:             sun.Time,
		HeightHash:       model.ChunkHeightHash(*chunk),
		ContentHash:      model.ChunkContentHash(*chunk),
		SunHash:          model.SunHash(sun),
		SkyHash:          model.SkySetHash(p.Sky),
		ParameterVersion: p.Params.ParameterVersion,
		AlgorithmVersion: p.Params.AlgorithmVersion,
	}
	sourceSet := make(map[string]struct{})
	edgeSet := make(map[string]struct{})
	occluderSet := make(map[string]struct{})
	for _, ref := range chunk.Occluders {
		occluderSet[ref] = struct{}{}
	}

	sampleCount := chunk.ResX * chunk.ResZ
	for start := 0; start < sampleCount; start += vertexBufferCap {
		end := minInt(start+vertexBufferCap, sampleCount)
		unit.BufferBatches++
		for index := start; index < end; index++ {
			ix := index % chunk.ResX
			iz := index / chunk.ResX
			world := chunk.VertexAt(ix, iz)
			direct, err := t.TraceVisibility(world, sunDir, traceOpts)
			if err != nil {
				return nil, err
			}
			sourceSet[chunk.ID] = struct{}{}
			for _, id := range direct.SourceChunks {
				sourceSet[id] = struct{}{}
			}
			for _, id := range direct.EdgeIDs {
				edgeSet[id] = struct{}{}
			}
			if direct.FirstHit != nil && direct.FirstHit.Occluder != "" {
				occluderSet[direct.FirstHit.Occluder] = struct{}{}
			}

			skyVisibleWeight := 0.0
			skyVisibleWeightFixed := int64(0)
			skyByChannel := [3]float64{}
			skyWeightedFixed := [3]int64{}
			truncations := make([]model.Truncation, 0, 8)
			for _, sky := range p.Sky {
				skyDir := geometry.Direction(sky.AzimuthDeg, sky.ElevationDeg)
				skyResult, err := t.TraceVisibility(world, skyDir, traceOpts)
				if err != nil {
					return nil, err
				}
				for _, id := range skyResult.SourceChunks {
					sourceSet[id] = struct{}{}
				}
				for _, id := range skyResult.EdgeIDs {
					edgeSet[id] = struct{}{}
				}
				if skyResult.FirstHit != nil && skyResult.FirstHit.Occluder != "" {
					occluderSet[skyResult.FirstHit.Occluder] = struct{}{}
				}
				weightFixed, weightResidual := model.Quantize(sky.Weight, scale)
				if weightResidual != 0 {
					truncations = append(truncations, model.Truncation{
						Vertex: model.VertexPoint{X: ix, Z: iz}, World: model.Vec2{X: world.X, Z: world.Z},
						Stage: "sky_weight", Channel: sky.ID, Exact: sky.Weight,
						Fixed: weightFixed, Scale: scale, Residual: weightResidual,
					})
				}
				if skyResult.Visible {
					skyVisibleWeight += sky.Weight
					skyVisibleWeightFixed += weightFixed
					for ch := 0; ch < 3; ch++ {
						skyByChannel[ch] += sky.Weight * sky.Radiance[ch]
						radianceFixed, radianceResidual := model.Quantize(sky.Radiance[ch], scale)
						if radianceResidual != 0 {
							truncations = append(truncations, model.Truncation{
								Vertex: model.VertexPoint{X: ix, Z: iz}, World: model.Vec2{X: world.X, Z: world.Z},
								Stage: "sky_radiance", Channel: sky.ID + "." + channelName(ch), Exact: sky.Radiance[ch],
								Fixed: radianceFixed, Scale: scale, Residual: radianceResidual,
							})
						}
						product := weightFixed * radianceFixed
						skyWeightedFixed[ch] += product
					}
				}
			}
			rawVisibility := skyVisibleWeight / totalSkyWeight
			if err := ValidateSkyVisibility(rawVisibility, world, model.VertexPoint{X: ix, Z: iz}, p.Params); err != nil {
				return nil, err
			}
			visibilityProduct := skyVisibleWeightFixed * scale
			visibilityFixed := visibilityProduct / totalSkyWeightFixed
			visibilityResidual := visibilityProduct % totalSkyWeightFixed
			if visibilityResidual != 0 {
				truncations = append(truncations, model.Truncation{
					Vertex: model.VertexPoint{X: ix, Z: iz}, World: model.Vec2{X: world.X, Z: world.Z},
					Stage: "sky_visibility", Channel: "visibility", Exact: rawVisibility,
					Fixed: visibilityFixed, Scale: p.Params.FixedScale,
					Residual: float64(visibilityResidual) / float64(totalSkyWeightFixed),
				})
			}
			if visibilityFixed > p.Params.FixedScale || visibilityFixed > p.Params.MaxChannelFixed {
				return nil, model.NewError(model.CodeValue, "sky visibility exceeds declared channel limit").
					WithChunk(chunk.ID).WithPoint(model.VertexPoint{X: ix, Z: iz}).
					WithWorld(model.Vec2{X: world.X, Z: world.Z}).
					WithChannel("visibility")
			}

			point := model.PointResult{
				Vertex:             model.VertexPoint{X: ix, Z: iz},
				World:              world,
				DirectShadow:       direct.Shadow,
				Shadow:             direct,
				SkyVisibility:      rawVisibility,
				SkyVisibilityFixed: visibilityFixed,
			}
			for ch := 0; ch < 3; ch++ {
				channel := channelName(ch)
				directExact := 0.0
				directFixed := int64(0)
				if direct.Visible {
					directExact = sun.Radiance[ch] * sunSin
					product := sunRadianceFixed[ch] * sunSinFixed
					directFixed = product / scale
					if product%scale != 0 {
						truncations = append(truncations, model.Truncation{
							Vertex: model.VertexPoint{X: ix, Z: iz}, World: model.Vec2{X: world.X, Z: world.Z},
							Stage: "direct_accumulate", Channel: channel, Exact: directExact,
							Fixed: directFixed, Scale: scale,
							Residual: float64(product%scale) / float64(scale),
						})
					}
				}
				skyExact := skyByChannel[ch] / totalSkyWeight
				skyFixed := skyWeightedFixed[ch] / totalSkyWeightFixed
				skyResidual := skyWeightedFixed[ch] % totalSkyWeightFixed
				if skyResidual != 0 {
					truncations = append(truncations, model.Truncation{
						Vertex: model.VertexPoint{X: ix, Z: iz}, World: model.Vec2{X: world.X, Z: world.Z},
						Stage: "sky", Channel: channel, Exact: skyExact,
						Fixed: skyFixed, Scale: p.Params.FixedScale,
						Residual: float64(skyResidual) / float64(totalSkyWeightFixed),
					})
				}
				if directFixed+skyFixed > p.Params.MaxChannelFixed {
					return nil, model.NewError(model.CodeValue, "combined radiance exceeds declared channel limit").
						WithChunk(chunk.ID).WithPoint(model.VertexPoint{X: ix, Z: iz}).
						WithWorld(model.Vec2{X: world.X, Z: world.Z}).WithChannel(channel)
				}
				albedoFixed, albedoResidual := model.Quantize(chunk.Albedo[ch], scale)
				if albedoResidual != 0 {
					truncations = append(truncations, model.Truncation{
						Vertex: model.VertexPoint{X: ix, Z: iz}, World: model.Vec2{X: world.X, Z: world.Z},
						Stage: "albedo", Channel: channel, Exact: chunk.Albedo[ch],
						Fixed: albedoFixed, Scale: scale, Residual: albedoResidual,
					})
				}
				mixedProduct := albedoFixed * (directFixed + skyFixed)
				mixedExact := float64(mixedProduct) / float64(scale) / float64(scale)
				mixedFixed := mixedProduct / scale
				mixedResidual := mixedProduct % scale
				if mixedResidual != 0 {
					truncations = append(truncations, model.Truncation{
						Vertex: model.VertexPoint{X: ix, Z: iz}, World: model.Vec2{X: world.X, Z: world.Z},
						Stage: "mixed", Channel: channel, Exact: mixedExact,
						Fixed: mixedFixed, Scale: p.Params.FixedScale,
						Residual: float64(mixedResidual) / float64(scale),
					})
				}
				if mixedFixed > p.Params.MaxChannelFixed {
					return nil, model.NewError(model.CodeValue, "mixed irradiance exceeds declared channel limit").
						WithChunk(chunk.ID).WithPoint(model.VertexPoint{X: ix, Z: iz}).
						WithWorld(model.Vec2{X: world.X, Z: world.Z}).WithChannel(channel)
				}
				point.IrradianceFixed[ch] = mixedFixed
				point.Irradiance[ch] = float64(mixedFixed) / float64(p.Params.FixedScale)
			}
			point.Truncations = truncations
			unit.Points = append(unit.Points, point)
		}
	}

	sources := sortedSet(sourceSet)
	edges := sortedSet(edgeSet)
	occluders := sortedSet(occluderSet)
	upstream := make([]string, 0, len(sources)+1)
	if previousKey != "" {
		upstream = append(upstream, previousKey)
	}
	upstream = append(upstream, "occluders:"+occluderSetHash)
	heightHashes := make([]NamedHash, 0, len(sources))
	neighborHashes := make([]string, 0, len(sources))
	for _, id := range sources {
		c, ok := t.Chunk(id)
		if !ok {
			return nil, model.NewError(model.CodeValue, "ray referenced unknown chunk %q", id).WithChunk(chunk.ID)
		}
		h := model.ChunkHeightHash(*c)
		heightHashes = append(heightHashes, NamedHash{ID: id, Hash: h})
		if id != chunk.ID {
			neighborHashes = append(neighborHashes, h)
			upstream = append(upstream, TerrainCacheKey(id, h, p.Params))
		}
	}
	sort.Slice(heightHashes, func(i, j int) bool { return heightHashes[i].ID < heightHashes[j].ID })
	sort.Strings(upstream)
	unit.CacheKey = model.CombineCacheKey(
		chunk.ID,
		unit.HeightHash,
		neighborHashes,
		sun.Time,
		unit.SunHash,
		p.Params.ParameterVersion,
		p.Params.AlgorithmVersion,
		upstream,
	)
	albedoHash := model.HashStrings(
		fmt.Sprintf("%.17g", chunk.Albedo[0]),
		fmt.Sprintf("%.17g", chunk.Albedo[1]),
		fmt.Sprintf("%.17g", chunk.Albedo[2]),
	)
	dependencies := []string{
		"height:" + chunk.ID,
		"albedo:" + chunk.ID,
		"sun:" + fmt.Sprintf("%d", sun.Time),
		"sky:set",
	}
	for _, id := range sources {
		if id != chunk.ID {
			dependencies = append(dependencies, "height:"+id)
		}
	}
	for _, id := range occluders {
		dependencies = append(dependencies, "occluder:"+id)
	}
	for _, id := range edges {
		dependencies = append(dependencies, "edge:"+id)
	}
	if previousKey != "" {
		dependencies = append(dependencies, "cache:"+previousKey)
	}
	sort.Strings(dependencies)
	unit.Dependencies = Dependencies{
		HeightHashes:  heightHashes,
		AlbedoHash:    albedoHash,
		OccluderIDs:   occluders,
		OccluderHash:  occluderSetHash,
		SunTime:       sun.Time,
		SunHash:       unit.SunHash,
		SkyHash:       unit.SkyHash,
		SourceChunks:  sources,
		EdgeIDs:       edges,
		UpstreamKeys:  upstream,
		TemporalKey:   previousKey,
		ParameterVer:  p.Params.ParameterVersion,
		AlgorithmVer:  p.Params.AlgorithmVersion,
		DependencyIDs: dependencies,
		OutputNodes: []string{
			"shadow:" + chunk.ID + "@" + fmt.Sprintf("%d", sun.Time),
			"sky_visibility:" + chunk.ID + "@" + fmt.Sprintf("%d", sun.Time),
			"irradiance:" + chunk.ID + "@" + fmt.Sprintf("%d", sun.Time),
		},
	}
	unit.ShadowDigest = digestShadow(unit.Points)
	unit.SkyDigest = digestSky(unit.Points)
	unit.IrradianceDigest = digestIrradiance(unit.Points)
	return unit, nil
}

// ValidateSkyVisibility rejects visibility outside the declared unit interval.
func ValidateSkyVisibility(value float64, world model.Vec3, vertex model.VertexPoint, params model.BakeParams) error {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
		return model.NewError(model.CodeValue, "sky visibility %.9f is outside [0,1]", value).
			WithPoint(vertex).
			WithWorld(model.Vec2{X: world.X, Z: world.Z}).
			WithChannel("visibility")
	}
	fixed, _ := model.Quantize(value, params.FixedScale)
	if fixed > params.MaxChannelFixed {
		return model.NewError(model.CodeValue, "sky visibility fixed value %d exceeds channel limit %d", fixed, params.MaxChannelFixed).
			WithPoint(vertex).
			WithWorld(model.Vec2{X: world.X, Z: world.Z}).
			WithChannel("visibility")
	}
	return nil
}

// TerrainCacheKey is the deterministic upstream key for a chunk height field.
func TerrainCacheKey(chunkID, heightHash string, params model.BakeParams) string {
	return model.HashStrings("terrain-cache", chunkID, heightHash,
		fmt.Sprintf("%d", params.ParameterVersion), params.AlgorithmVersion)
}

func digestShadow(points []model.PointResult) string {
	var b strings.Builder
	for _, p := range points {
		fmt.Fprintf(&b, "%d,%d:%t:%d:%d;", p.Vertex.X, p.Vertex.Z,
			p.DirectShadow, p.Shadow.Steps, p.Shadow.StepBudget)
		if p.Shadow.FirstHit != nil {
			fmt.Fprintf(&b, "%s:%d", p.Shadow.FirstHit.Kind, p.Shadow.FirstHit.Step)
		}
		b.WriteByte('|')
	}
	return model.HashStrings("shadow-digest", b.String())
}

func digestSky(points []model.PointResult) string {
	var b strings.Builder
	for _, p := range points {
		fmt.Fprintf(&b, "%d,%d:%d;", p.Vertex.X, p.Vertex.Z, p.SkyVisibilityFixed)
	}
	return model.HashStrings("sky-digest", b.String())
}

func digestIrradiance(points []model.PointResult) string {
	var b strings.Builder
	for _, p := range points {
		fmt.Fprintf(&b, "%d,%d:%d,%d,%d;", p.Vertex.X, p.Vertex.Z,
			p.IrradianceFixed[0], p.IrradianceFixed[1], p.IrradianceFixed[2])
	}
	return model.HashStrings("irradiance-digest", b.String())
}

func sortedSet(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func channelName(i int) string {
	switch i {
	case 0:
		return "r"
	case 1:
		return "g"
	default:
		return "b"
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
