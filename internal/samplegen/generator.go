package samplegen

import (
	"fmt"
	"math"

	"terrain-occlusion/internal/cull"
	"terrain-occlusion/internal/estimate"
	"terrain-occlusion/internal/geo"
	"terrain-occlusion/internal/model"
	"terrain-occlusion/internal/stream"
)

const Seed int64 = 420261125

func Generate() (*model.Project, error) {
	widthCells, depthCells := 16, 12
	cell := 2.0
	t := model.Terrain{WidthCells: widthCells, DepthCells: depthCells, CellSize: cell, HeightScale: 1, Heights: make([]float64, (widthCells+1)*(depthCells+1))}
	for iz := 0; iz <= depthCells; iz++ {
		for ix := 0; ix <= widthCells; ix++ {
			x, z := float64(ix)*cell, float64(iz)*cell
			t.Heights[iz*(widthCells+1)+ix] = heightAt(Seed, x, z)
		}
	}

	occluders := []model.Occluder{
		{ID: 1, Label: "west-shadow-ridge", Bounds: model.Box3{Min: model.Vec3{X: -0.5, Y: 0, Z: 5.8}, Max: model.Vec3{X: 18.5, Y: 6.4, Z: 8.2}}},
		{ID: 2, Label: "east-back-ridge", Bounds: model.Box3{Min: model.Vec3{X: 13.5, Y: 0, Z: 13.8}, Max: model.Vec3{X: 32.5, Y: 7.1, Z: 16.2}}},
		{ID: 3, Label: "central-knife-spur", Bounds: model.Box3{Min: model.Vec3{X: 14.2, Y: 0, Z: 9.4}, Max: model.Vec3{X: 17.8, Y: 3.4, Z: 12.8}}},
	}

	chunks := []model.Chunk{}
	cols, rows := 4, 3
	chunkW := float64(widthCells) * cell / float64(cols)
	chunkD := float64(depthCells) * cell / float64(rows)
	id := 1
	for row := 0; row < rows; row++ {
		for col := 0; col < cols; col++ {
			minX := float64(col) * chunkW
			minZ := float64(row) * chunkD
			rect := model.Rect2{MinX: minX, MinZ: minZ, MaxX: minX + chunkW, MaxZ: minZ + chunkD}
			complexity := 72 + id*11
			chunk := model.Chunk{
				ID: id, Label: fmt.Sprintf("chunk-r%d-c%d", row+1, col+1), Rect: rect,
				Complexity: complexity, LoadCost: 2 + (id % 4), MemoryBytes: 1<<20 + int64(complexity)*8192,
				OccluderRefs: refsForRect(rect, occluders),
				SamplePoints: samplePoints(t, rect),
			}
			chunks = append(chunks, chunk)
			id++
		}
	}

	track := make([]model.CameraSample, 0, 9)
	zPositions := []float64{-10, -6, -2, 2, 6, 10, 6, 2, -2}
	for i, z := range zPositions {
		track = append(track, model.CameraSample{
			Index: i, TimeMs: int64(i) * 400,
			Position: model.Vec3{X: 16, Y: 10.6, Z: z},
			Target:   model.Vec3{X: 16, Y: 4.0, Z: 18},
			FovDeg:   78, Near: 0.1, Far: 180,
			PoseKey: fmt.Sprintf("advance-z%.0f", z),
		})
	}

	p := &model.Project{
		SchemaVersion: "terrain-occlusion/v1", Seed: Seed,
		GeneratedAt: "2026-11-25T00:00:00Z",
		Parameters: model.QuantParameters{
			ShadowResolution: 128, LightAzimuthDeg: 38, LightElevationDeg: 44,
			CameraDepthRes: 512, SampleSpacing: 0.125, FixedPointStep: 1.0 / 2048.0,
			ViewportWidth: 960, ViewportHeight: 540, MaxWorkers: 2, DepthBufferLimit: 64,
		},
		Budgets: model.Budgets{
			LoadSlots: 2, MemoryBytes: 12 << 20, MaxResidencyDelay: 2,
			MaxParallelTasks: 2, DepthBuffersPerTask: 64,
		},
		Terrain: t, Occluders: occluders, Chunks: chunks, CameraTrack: track,
		Truth: model.Truth{GeneratorSeed: Seed},
	}

	truth, projections, err := buildTruth(p)
	if err != nil {
		return nil, err
	}
	p.Truth = truth
	p.Truth.Projection = projections
	return p, nil
}

func heightAt(seed int64, x, z float64) float64 {
	h := 1.35 + 0.55*math.Sin(x*0.33+float64(seed%5)) + 0.35*math.Cos(z*0.41)
	ridge1 := 5.7 * math.Exp(-((z-7)*(z-7))/2.7) * math.Exp(-((x-8)*(x-8))/95)
	ridge2 := 6.3 * math.Exp(-((z-15)*(z-15))/3.3) * math.Exp(-((x-25)*(x-25))/82)
	spur := 2.7 * math.Exp(-((x-16)*(x-16))/10.5) * math.Exp(-((z-11)*(z-11))/15)
	return math.Max(0.25, h+ridge1+ridge2+spur)
}

func samplePoints(t model.Terrain, r model.Rect2) []model.Vec3 {
	out := make([]model.Vec3, 0, 9)
	for _, fz := range []float64{0.25, 0.5, 0.75} {
		for _, fx := range []float64{0.25, 0.5, 0.75} {
			x := r.MinX + (r.MaxX-r.MinX)*fx
			z := r.MinZ + (r.MaxZ-r.MinZ)*fz
			out = append(out, model.Vec3{X: x, Y: heightWorld(t, x, z) + 0.08, Z: z})
		}
	}
	return out
}

func heightWorld(t model.Terrain, x, z float64) float64 {
	gx := x / t.CellSize
	gz := z / t.CellSize
	x0 := int(math.Floor(gx))
	z0 := int(math.Floor(gz))
	tx := gx - float64(x0)
	tz := gz - float64(z0)
	h00 := vertexHeight(t, x0, z0)
	h10 := vertexHeight(t, x0+1, z0)
	h01 := vertexHeight(t, x0, z0+1)
	h11 := vertexHeight(t, x0+1, z0+1)
	h0 := h00*(1-tx) + h10*tx
	h1 := h01*(1-tx) + h11*tx
	return h0*(1-tz) + h1*tz
}

func vertexHeight(t model.Terrain, ix, iz int) float64 {
	ix = clampInt(ix, 0, t.WidthCells)
	iz = clampInt(iz, 0, t.DepthCells)
	return t.Heights[iz*(t.WidthCells+1)+ix]
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func refsForRect(r model.Rect2, occ []model.Occluder) []int {
	refs := []int{}
	for _, o := range occ {
		expanded := model.Rect2{MinX: o.Bounds.Min.X - 2, MaxX: o.Bounds.Max.X + 2, MinZ: o.Bounds.Min.Z - 2, MaxZ: o.Bounds.Max.Z + 2}
		if r.MaxX >= expanded.MinX && r.MinX <= expanded.MaxX && r.MaxZ >= expanded.MinZ && r.MinZ <= expanded.MaxZ {
			refs = append(refs, o.ID)
		}
	}
	if len(refs) == 0 {
		// Every chunk still gets a concrete terrain shadow body reference when
		// spatially adjacent bodies exist; front chunks use the west ridge.
		refs = append(refs, 1)
	}
	return refs
}

func buildTruth(p *model.Project) (model.Truth, []model.ProjectionTruth, error) {
	truth := model.Truth{GeneratorSeed: p.Seed, Samples: make([]model.SampleTruth, 0, len(p.CameraTrack))}
	planner := stream.NewPlanner(p.Budgets)
	projections := []model.ProjectionTruth{}

	for i, camSample := range p.CameraTrack {
		shadow := geo.RenderShadowMap(*p)
		chunkTruth := make([]model.ChunkTruth, 0, len(p.Chunks))
		for _, chunk := range p.Chunks {
			truth := oracleChunkTruth(p, camSample, shadow, chunk)
			chunkTruth = append(chunkTruth, truth)
			cam := geo.NewCamera(camSample, p.Parameters.ViewportWidth, p.Parameters.ViewportHeight)
			for pi, pt := range chunk.SamplePoints {
				if pi != 0 && pi != 4 && pi != 8 {
					continue
				}
				sx, sy, depth, _, _, ok := cam.Project(pt)
				if depth <= camSample.Near || !ok {
					continue
				}
				projections = append(projections, model.ProjectionTruth{
					SampleIndex: i, ChunkID: chunk.ID, PointIndex: pi,
					ScreenX: sx, ScreenY: sy, ViewDepth: depth,
				})
			}
		}
		visible, occluded, uncertain := truthIDs(chunkTruth)
		desired := append(append([]int{}, visible...), uncertain...)
		distances := stream.Distances(p, camSample.Position)
		residency, err := planner.Reconcile(i, p, desired, distances)
		if err != nil {
			return model.Truth{}, nil, err
		}
		truth.Samples = append(truth.Samples, model.SampleTruth{
			SampleIndex: i, TimeMs: camSample.TimeMs, Chunks: chunkTruth,
			VisibleIDs: visible, OccludedIDs: occluded, UncertainIDs: uncertain,
			ResidentIDs: residency.ResidentIDs, LoadEvents: residency.Loads, UnloadEvents: residency.Unloads,
		})
	}
	return truth, projections, nil
}

// oracleChunkTruth is the generator's analytic truth rule. It uses unquantized
// geometric margins and the declared conservative threshold, not the runtime
// ChunkVerdict result.
func oracleChunkTruth(p *model.Project, camSample model.CameraSample, shadow *geo.DepthMap, chunk model.Chunk) model.ChunkTruth {
	a := cull.AnalyzeExact(p, camSample, shadow, chunk)
	threshold := estimate.BaseBound(p.Parameters) + p.Parameters.FixedPointStep

	if a.CoarseOK && a.CoarseMargin < -threshold {
		return model.ChunkTruth{ChunkID: chunk.ID, Verdict: model.VerdictOccluded, ReferenceOccluderID: a.CoarseRef, ExactMargin: a.CoarseMargin}
	}
	if a.FrustumMargin < -threshold {
		return model.ChunkTruth{ChunkID: chunk.ID, Verdict: model.VerdictOccluded, ExactMargin: a.FrustumMargin}
	}
	if a.FrustumMargin <= threshold {
		return model.ChunkTruth{ChunkID: chunk.ID, Verdict: model.VerdictUncertain, ExactMargin: a.FrustumMargin}
	}
	if !a.HasDepth {
		return model.ChunkTruth{ChunkID: chunk.ID, Verdict: model.VerdictUncertain, ExactMargin: 0}
	}
	switch {
	case a.DepthMargin > threshold:
		return model.ChunkTruth{ChunkID: chunk.ID, Verdict: model.VerdictVisible, ReferenceOccluderID: a.DepthRef, ExactMargin: a.DepthMargin}
	case a.DepthMargin < -threshold:
		return model.ChunkTruth{ChunkID: chunk.ID, Verdict: model.VerdictOccluded, ReferenceOccluderID: a.DepthRef, ExactMargin: a.DepthMargin}
	default:
		return model.ChunkTruth{ChunkID: chunk.ID, Verdict: model.VerdictUncertain, ReferenceOccluderID: a.DepthRef, ExactMargin: a.DepthMargin}
	}
}

func truthIDs(chunks []model.ChunkTruth) (visible, occluded, uncertain []int) {
	visible, occluded, uncertain = []int{}, []int{}, []int{}
	for _, c := range chunks {
		switch c.Verdict {
		case model.VerdictVisible:
			visible = append(visible, c.ChunkID)
		case model.VerdictOccluded:
			occluded = append(occluded, c.ChunkID)
		default:
			uncertain = append(uncertain, c.ChunkID)
		}
	}
	return visible, occluded, uncertain
}
