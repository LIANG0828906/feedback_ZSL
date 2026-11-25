// Package generator creates deterministic terrain samples and an independent
// truth inventory. Truth is evaluated directly from generated geometry and
// parameters, never from bake output.
package generator

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"terrainbake/model"
)

// Config controls the deterministic sample layout.
type Config struct {
	Seed       int64
	ChunksX    int
	ChunksZ    int
	ResX       int
	ResZ       int
	Width      float64
	Depth      float64
	TimeCount  int
	SkyCount   int
	FixedScale int64
}

// DefaultConfig is the fixed, reproducible acceptance sample.
func DefaultConfig(seed int64) Config {
	return Config{
		Seed:       seed,
		ChunksX:    3,
		ChunksZ:    3,
		ResX:       5,
		ResZ:       5,
		Width:      36,
		Depth:      36,
		TimeCount:  4,
		SkyCount:   8,
		FixedScale: 10000,
	}
}

// Generate uses the default layout.
func Generate(seed int64) (model.SampleBundle, error) {
	return GenerateWithConfig(DefaultConfig(seed))
}

// GenerateWithConfig creates a deterministic project and truth inventory.
func GenerateWithConfig(cfg Config) (model.SampleBundle, error) {
	if cfg.ChunksX <= 0 || cfg.ChunksZ <= 0 || cfg.ResX < 2 || cfg.ResZ < 2 ||
		cfg.Width <= 0 || cfg.Depth <= 0 || cfg.TimeCount <= 0 || cfg.SkyCount <= 0 || cfg.FixedScale <= 0 {
		return model.SampleBundle{}, model.NewError(model.CodeValue, "invalid generator configuration")
	}
	if cfg.FixedScale < 100 {
		return model.SampleBundle{}, model.NewError(model.CodeValue, "fixed scale is too small")
	}
	p := model.Project{
		Seed: cfg.Seed,
		Bounds: model.Bounds{
			MinX: 0,
			MinZ: 0,
			MaxX: cfg.Width,
			MaxZ: cfg.Depth,
		},
		Params: model.BakeParams{
			FixedScale:          cfg.FixedScale,
			MaxChannelFixed:     cfg.FixedScale * 64,
			ShadowStepBudget:    256,
			RayStep:             0.55,
			RayEpsilon:          0.005,
			IrradianceTolerance: 0.0025,
			MaxParallelism:      4,
			VertexBufferCap:     16,
			QueueBudget:         3,
			ParameterVersion:    1,
			AlgorithmVersion:    "terrain-bake-v1",
		},
	}
	spacingX := cfg.Width / float64(cfg.ChunksX*(cfg.ResX-1))
	spacingZ := cfg.Depth / float64(cfg.ChunksZ*(cfg.ResZ-1))
	chunkWidth := spacingX * float64(cfg.ResX-1)
	chunkDepth := spacingZ * float64(cfg.ResZ-1)

	phase := float64(cfg.Seed%10007) * 0.0173
	ridgeX := cfg.Width * 0.56
	ridgeZ := cfg.Depth * 0.47
	ridgeRadius := math.Min(cfg.Width, cfg.Depth) * 0.22
	ridgeAmp := 8.2
	hillX := cfg.Width * 0.20
	hillZ := cfg.Depth * 0.68
	hillRadius := math.Min(cfg.Width, cfg.Depth) * 0.14
	hillAmp := 4.3
	heightAt := func(x, z float64) float64 {
		base := 2.4 +
			0.62*math.Sin(x*0.20+phase) +
			0.48*math.Cos(z*0.17-phase*0.7) +
			0.20*math.Sin((x+z)*0.31+phase*0.3)
		base += gaussian(x, z, ridgeX, ridgeZ, ridgeRadius) * ridgeAmp
		base += gaussian(x, z, hillX, hillZ, hillRadius) * hillAmp
		return math.Round(base*1e9) / 1e9
	}

	for gz := 0; gz < cfg.ChunksZ; gz++ {
		for gx := 0; gx < cfg.ChunksX; gx++ {
			id := fmt.Sprintf("chunk-%02d-%02d", gz, gx)
			chunk := model.Chunk{
				ID:    id,
				GridX: gx,
				GridZ: gz,
				Bounds: model.Bounds{
					MinX: float64(gx*(cfg.ResX-1)) * spacingX,
					MinZ: float64(gz*(cfg.ResZ-1)) * spacingZ,
					MaxX: float64((gx+1)*(cfg.ResX-1)) * spacingX,
					MaxZ: float64((gz+1)*(cfg.ResZ-1)) * spacingZ,
				},
				ResX:      cfg.ResX,
				ResZ:      cfg.ResZ,
				Occluders: make([]string, 0),
			}
			for iz := 0; iz < cfg.ResZ; iz++ {
				for ix := 0; ix < cfg.ResX; ix++ {
					globalX := gx*(cfg.ResX-1) + ix
					globalZ := gz*(cfg.ResZ-1) + iz
					chunk.Heights = append(chunk.Heights, heightAt(
						float64(globalX)*spacingX,
						float64(globalZ)*spacingZ,
					))
				}
			}
			chunk.Albedo = [3]float64{
				0.30 + 0.12*float64((gx+int(cfg.Seed)+1)%4)/3 + 0.025*float64(gz),
				0.34 + 0.10*float64((gz+int(cfg.Seed)+2)%4)/3 + 0.018*float64(gx),
				0.40 + 0.08*float64((gx+gz+3)%4)/3,
			}
			p.Chunks = append(p.Chunks, chunk)
		}
	}

	occluderSpecs := []struct {
		fx, fz, radius, height float64
	}{
		{0.33, 0.38, 1.25, 5.8},
		{0.67, 0.30, 1.05, 4.6},
		{0.74, 0.71, 1.35, 6.4},
		{0.24, 0.78, 0.95, 4.1},
	}
	for i, spec := range occluderSpecs {
		center := model.Vec2{X: cfg.Width * spec.fx, Z: cfg.Depth * spec.fz}
		base := heightAt(center.X, center.Z)
		occ := model.Occluder{
			ID:     fmt.Sprintf("pillar-%d", i),
			Center: center,
			Radius: spec.radius,
			Base:   base - 0.25,
			Height: spec.height,
		}
		p.Occluders = append(p.Occluders, occ)
		gx := int(center.X / chunkWidth)
		gz := int(center.Z / chunkDepth)
		if gx < 0 {
			gx = 0
		}
		if gx >= cfg.ChunksX {
			gx = cfg.ChunksX - 1
		}
		if gz < 0 {
			gz = 0
		}
		if gz >= cfg.ChunksZ {
			gz = cfg.ChunksZ - 1
		}
		index := gz*cfg.ChunksX + gx
		p.Chunks[index].Occluders = append(p.Chunks[index].Occluders, occ.ID)
	}

	skyBase := [3]float64{0.58, 0.66, 0.82}
	for i := 0; i < cfg.SkyCount; i++ {
		az := 360 * float64(i) / float64(cfg.SkyCount)
		el := 27 + 7*float64(i%4)
		weight := 0.7 + 0.11*float64((i+int(cfg.Seed%5))%5)
		scale := 0.86 + 0.06*float64(i%3)
		p.Sky = append(p.Sky, model.SkySample{
			ID:           fmt.Sprintf("sky-%02d", i),
			AzimuthDeg:   az,
			ElevationDeg: el,
			Radiance: [3]float64{
				skyBase[0] * scale,
				skyBase[1] * (scale + 0.015),
				skyBase[2] * (scale + 0.03),
			},
			Weight: weight,
		})
	}

	for i := 0; i < cfg.TimeCount; i++ {
		p.Sun = append(p.Sun, model.SunSample{
			Time:         i,
			AzimuthDeg:   34 + 17*float64(i),
			ElevationDeg: 23 + 7*float64(i),
			Radiance: [3]float64{
				3.1 + 0.22*float64(i),
				2.85 + 0.20*float64(i),
				2.55 + 0.17*float64(i),
			},
		})
	}

	truth := evaluateTruth(&p)
	return model.SampleBundle{Project: p, Truth: truth}, nil
}

// MarshalBundle writes deterministic indented JSON.
func MarshalBundle(bundle model.SampleBundle) ([]byte, error) {
	data, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return nil, model.NewError(model.CodeValue, "marshal generated bundle: %v", err)
	}
	return append(data, '\n'), nil
}

func gaussian(x, z, cx, cz, radius float64) float64 {
	dx := x - cx
	dz := z - cz
	return math.Exp(-(dx*dx + dz*dz) / (2 * radius * radius))
}

type truthSurface struct {
	project   *model.Project
	chunks    []model.Chunk
	maxHeight float64
}

func newTruthSurface(p *model.Project) *truthSurface {
	s := &truthSurface{project: p, chunks: append([]model.Chunk(nil), p.Chunks...)}
	for _, c := range p.Chunks {
		for _, h := range c.Heights {
			if h > s.maxHeight {
				s.maxHeight = h
			}
		}
	}
	for _, o := range p.Occluders {
		if o.Top() > s.maxHeight {
			s.maxHeight = o.Top()
		}
	}
	return s
}

func (s *truthSurface) heightAt(x, z float64) float64 {
	for i := range s.chunks {
		c := &s.chunks[i]
		b := c.Bounds
		if x < b.MinX-1e-9 || x > b.MaxX+1e-9 || z < b.MinZ-1e-9 || z > b.MaxZ+1e-9 {
			continue
		}
		u := clamp((x-b.MinX)/b.Width()*float64(c.ResX-1), 0, float64(c.ResX-1))
		v := clamp((z-b.MinZ)/b.Depth()*float64(c.ResZ-1), 0, float64(c.ResZ-1))
		x0 := int(math.Floor(u))
		z0 := int(math.Floor(v))
		x1 := minInt(x0+1, c.ResX-1)
		z1 := minInt(z0+1, c.ResZ-1)
		tx := u - float64(x0)
		tz := v - float64(z0)
		h00 := c.Heights[z0*c.ResX+x0]
		h10 := c.Heights[z0*c.ResX+x1]
		h01 := c.Heights[z1*c.ResX+x0]
		h11 := c.Heights[z1*c.ResX+x1]
		return h00 + (h10-h00)*tx + (h01-h00)*tz + (h00-h10-h01+h11)*tx*tz
	}
	return 0
}

func (s *truthSurface) trace(start model.Vec3, dir model.Vec3) (bool, int, *model.Hit) {
	params := s.project.Params
	nd := normalize(dir)
	origin := model.Vec3{X: start.X, Y: start.Y + params.RayEpsilon, Z: start.Z}
	for step := 1; step <= params.ShadowStepBudget; step++ {
		distance := params.RayStep * float64(step)
		p := model.Vec3{
			X: origin.X + nd.X*distance,
			Y: origin.Y + nd.Y*distance,
			Z: origin.Z + nd.Z*distance,
		}
		if p.Y > s.maxHeight+params.RayEpsilon+1e-9 {
			return true, step, nil
		}
		if p.X < s.project.Bounds.MinX || p.X > s.project.Bounds.MaxX ||
			p.Z < s.project.Bounds.MinZ || p.Z > s.project.Bounds.MaxZ {
			return true, step, nil
		}
		h := s.heightAt(p.X, p.Z)
		if p.Y <= h-params.RayEpsilon {
			return false, step, &model.Hit{Step: step, Position: p, Height: h, Kind: "terrain"}
		}
		for _, o := range s.project.Occluders {
			dx := p.X - o.Center.X
			dz := p.Z - o.Center.Z
			if dx*dx+dz*dz <= o.Radius*o.Radius && p.Y >= o.Base-params.RayEpsilon && p.Y <= o.Top() {
				return false, step, &model.Hit{
					Step: step, Position: p, Height: o.Top(), Kind: "occluder", Occluder: o.ID,
				}
			}
		}
	}
	return true, params.ShadowStepBudget, nil
}

func evaluateTruth(p *model.Project) model.Truth {
	surface := newTruthSurface(p)
	skyWeight := 0.0
	for _, s := range p.Sky {
		skyWeight += s.Weight
	}
	truth := model.Truth{Seed: p.Seed}
	for _, sun := range p.Sun {
		sunDir := direction(sun.AzimuthDeg, sun.ElevationDeg)
		for _, chunk := range p.Chunks {
			for iz := 0; iz < chunk.ResZ; iz++ {
				for ix := 0; ix < chunk.ResX; ix++ {
					pos := chunk.VertexAt(ix, iz)
					visible, steps, hit := surface.trace(pos, sunDir)
					skyVisible := 0.0
					for _, sky := range p.Sky {
						dir := direction(sky.AzimuthDeg, sky.ElevationDeg)
						vis, _, _ := surface.trace(pos, dir)
						if vis {
							skyVisible += sky.Weight
						}
					}
					skyVisibility := skyVisible / skyWeight
					irradiance := [3]float64{}
					for ch := 0; ch < 3; ch++ {
						direct := 0.0
						if visible {
							direct = sun.Radiance[ch] * math.Sin(sun.ElevationDeg*math.Pi/180)
						}
						sky := 0.0
						for _, skySample := range p.Sky {
							dir := direction(skySample.AzimuthDeg, skySample.ElevationDeg)
							vis, _, _ := surface.trace(pos, dir)
							if vis {
								sky += skySample.Weight * skySample.Radiance[ch] / skyWeight
							}
						}
						irradiance[ch] = chunk.Albedo[ch] * (direct + sky)
					}
					truth.Points = append(truth.Points, model.TruthPoint{
						ChunkID:       chunk.ID,
						Time:          sun.Time,
						Vertex:        model.VertexPoint{X: ix, Z: iz},
						World:         pos,
						DirectShadow:  !visible,
						SkyVisibility: skyVisibility,
						Irradiance:    irradiance,
						ShadowSteps:   steps,
						ShadowBudget:  p.Params.ShadowStepBudget,
						FirstHit:      hit,
					})
				}
			}
		}
	}
	sort.Slice(truth.Points, func(i, j int) bool {
		a, b := truth.Points[i], truth.Points[j]
		if a.Time != b.Time {
			return a.Time < b.Time
		}
		if a.ChunkID != b.ChunkID {
			return a.ChunkID < b.ChunkID
		}
		if a.Vertex.Z != b.Vertex.Z {
			return a.Vertex.Z < b.Vertex.Z
		}
		return a.Vertex.X < b.Vertex.X
	})
	return truth
}

func direction(azimuthDeg, elevationDeg float64) model.Vec3 {
	az := azimuthDeg * math.Pi / 180
	el := elevationDeg * math.Pi / 180
	cosEl := math.Cos(el)
	return model.Vec3{X: math.Sin(az) * cosEl, Y: math.Sin(el), Z: math.Cos(az) * cosEl}
}

func normalize(v model.Vec3) model.Vec3 {
	n := math.Sqrt(v.X*v.X + v.Y*v.Y + v.Z*v.Z)
	if n == 0 {
		return v
	}
	return model.Vec3{X: v.X / n, Y: v.Y / n, Z: v.Z / n}
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
