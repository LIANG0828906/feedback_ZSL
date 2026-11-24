package cull

import (
	"math"

	"terrain-occlusion/internal/estimate"
	"terrain-occlusion/internal/geo"
	"terrain-occlusion/internal/model"
)

type OccluderDepth struct {
	ID   int
	MinZ float64
	MaxZ float64
	MinX float64
	MaxX float64
	MinY float64
	MaxY float64
}

func ChunkBounds(p *model.Project, c model.Chunk) model.Box3 {
	b := model.Box3{
		Min: model.Vec3{X: c.Rect.MinX, Y: math.Inf(1), Z: c.Rect.MinZ},
		Max: model.Vec3{X: c.Rect.MaxX, Y: math.Inf(-1), Z: c.Rect.MaxZ},
	}
	cell := p.Terrain.CellSize
	ix0 := int(math.Round(c.Rect.MinX / cell))
	ix1 := int(math.Round(c.Rect.MaxX / cell))
	iz0 := int(math.Round(c.Rect.MinZ / cell))
	iz1 := int(math.Round(c.Rect.MaxZ / cell))
	for iz := iz0; iz <= iz1; iz++ {
		for ix := ix0; ix <= ix1; ix++ {
			if ix < 0 || iz < 0 || ix > p.Terrain.WidthCells || iz > p.Terrain.DepthCells {
				continue
			}
			v := geo.TerrainVertex(p.Terrain, ix, iz)
			b.Min.Y = math.Min(b.Min.Y, v.Y)
			b.Max.Y = math.Max(b.Max.Y, v.Y)
		}
	}
	for _, pt := range c.SamplePoints {
		b.Min.Y = math.Min(b.Min.Y, pt.Y)
		b.Max.Y = math.Max(b.Max.Y, pt.Y)
	}
	if math.IsInf(b.Min.Y, 1) {
		b.Min.Y, b.Max.Y = 0, 0
	}
	return b
}

func EvaluateChunk(p *model.Project, camSample model.CameraSample, shadow *geo.DepthMap, chunk model.Chunk) model.ChunkVerdict {
	q := p.Parameters
	bound := estimate.BaseBound(q)
	cam := geo.NewCamera(camSample, q.ViewportWidth, q.ViewportHeight)
	box := ChunkBounds(p, chunk)
	out := model.ChunkVerdict{ChunkID: chunk.ID, InFrustum: true}

	// Coarse light-space shadow test. A chunk is only fully culled when the
	// conservative interval remains strictly on the occluded side of zero.
	coarseMargin, coarseRef, coarseOK := coarseShadowMargin(chunk, shadow)
	if coarseOK {
		qm := geo.QuantizeFixed(coarseMargin, q.FixedPointStep)
		lower := qm - bound
		upper := qm + q.FixedPointStep + bound
		if upper < 0 {
			out.Coarse = "fully_occluded"
			out.Fine = model.VerdictOccluded
			out.Final = model.VerdictOccluded
			out.ReferenceOccluderID = coarseRef
			out.DecisionMargin = qm
			out.LowerBound = lower
			out.UpperBound = upper
			_, _, screen := cam.FrustumMargin(box)
			out.ScreenBounds = screen
			return out
		}
	}
	out.Coarse = "preliminary_visible"

	frustumWorld, allInside, screen := cam.FrustumMargin(box)
	out.ScreenBounds = screen
	frustumMargin := frustumWorld / camSample.Far
	fq := geo.QuantizeFixed(frustumMargin, q.FixedPointStep)
	fLower := fq - bound
	fUpper := fq + q.FixedPointStep + bound
	if fUpper < 0 {
		out.InFrustum = false
		out.Fine = model.VerdictOccluded
		out.Final = model.VerdictOccluded
		out.DecisionMargin = fq
		out.LowerBound = fLower
		out.UpperBound = fUpper
		return out
	}
	if fLower <= 0 && fUpper >= 0 {
		out.InFrustum = allInside
		out.Fine = model.VerdictUncertain
		out.Final = model.VerdictUncertain
		out.DecisionMargin = fq
		out.LowerBound = fLower
		out.UpperBound = fUpper
		return out
	}

	margin, exactMargin, ref, hasPoint := fineDepthMargin(p, cam, chunk)
	if !hasPoint {
		out.Fine = model.VerdictUncertain
		out.Final = model.VerdictUncertain
		out.LowerBound = -bound
		out.UpperBound = bound
		return out
	}
	qm := geo.QuantizeFixed(margin, q.FixedPointStep)
	lower := qm - bound
	upper := qm + q.FixedPointStep + bound
	out.InFrustum = true
	out.ReferenceOccluderID = ref
	out.DecisionMargin = exactMargin
	out.LowerBound = lower
	out.UpperBound = upper
	switch {
	case lower > 0:
		out.Fine = model.VerdictVisible
		out.Final = model.VerdictVisible
	case upper < 0:
		out.Fine = model.VerdictOccluded
		out.Final = model.VerdictOccluded
	default:
		out.Fine = model.VerdictUncertain
		out.Final = model.VerdictUncertain
	}
	return out
}

func coarseShadowMargin(chunk model.Chunk, shadow *geo.DepthMap) (float64, int, bool) {
	if shadow == nil {
		return 0, 0, false
	}
	found := false
	decision := math.Inf(-1)
	for _, pt := range chunk.SamplePoints {
		targetMargin, mapDepth, _, ok := shadow.ShadowOcclusionMargin(pt)
		if !ok {
			continue
		}
		found = true
		// Signed visibility margin: mapDepth-targetDepth. Negative means shadowed.
		signed := mapDepth - (mapDepth + targetMargin)
		decision = math.Max(decision, signed)
	}
	if !found {
		return 0, firstRef(chunk), false
	}
	return decision, firstRef(chunk), true
}

func firstRef(c model.Chunk) int {
	if len(c.OccluderRefs) > 0 {
		return c.OccluderRefs[0]
	}
	return 0
}

func fineDepthMargin(p *model.Project, cam geo.Camera, chunk model.Chunk) (quantizedInput, exactDecision float64, ref int, hasPoint bool) {
	occluders := map[int]model.Occluder{}
	for _, o := range p.Occluders {
		occluders[o.ID] = o
	}
	camBoxes := map[int]OccluderDepth{}
	for _, id := range chunk.OccluderRefs {
		if o, ok := occluders[id]; ok {
			camBoxes[id] = cameraOccluderDepth(cam, o.Bounds)
		}
	}

	decision := math.Inf(-1)
	decisionExact := math.Inf(-1)
	decisionRef := 0
	for _, pt := range chunk.SamplePoints {
		q := cam.ToCamera(pt)
		if q.Z <= cam.Near {
			continue
		}
		hasPoint = true
		pointMargin := 1.0
		pointExact := 1.0
		pointRef := 0
		for id, ob := range camBoxes {
			if q.X < ob.MinX || q.X > ob.MaxX || q.Y < ob.MinY || q.Y > ob.MaxY {
				continue
			}
			if q.Z < ob.MinZ-cam.Near {
				// Point is confidently in front of this occluder.
				margin := (ob.MinZ - q.Z) / cam.Far
				exact := margin
				if margin < pointMargin {
					pointMargin = margin
					pointExact = exact
					pointRef = id
				}
				continue
			}
			margin := (ob.MinZ - q.Z) / cam.Far
			if margin < pointMargin {
				pointMargin = margin
				pointExact = margin
				pointRef = id
			}
		}
		// Chunk verdict uses the most visible probe: if one probe is clear,
		// the chunk contains visible surface.
		if pointMargin > decision {
			decision = pointMargin
			decisionExact = pointExact
			decisionRef = pointRef
		}
	}
	if math.IsInf(decision, -1) {
		return 0, 0, 0, false
	}
	return decision, decisionExact, decisionRef, true
}

func cameraOccluderDepth(cam geo.Camera, box model.Box3) OccluderDepth {
	corners := geo.BoxCorners(box)
	out := OccluderDepth{MinZ: math.Inf(1), MaxZ: math.Inf(-1), MinX: math.Inf(1), MaxX: math.Inf(-1), MinY: math.Inf(1), MaxY: math.Inf(-1)}
	for _, c := range corners {
		q := cam.ToCamera(c)
		out.MinX = math.Min(out.MinX, q.X)
		out.MaxX = math.Max(out.MaxX, q.X)
		out.MinY = math.Min(out.MinY, q.Y)
		out.MaxY = math.Max(out.MaxY, q.Y)
		out.MinZ = math.Min(out.MinZ, q.Z)
		out.MaxZ = math.Max(out.MaxZ, q.Z)
	}
	return out
}

func IDsByVerdict(vs []model.ChunkVerdict) (visible, occluded, uncertain []int) {
	visible = []int{}
	occluded = []int{}
	uncertain = []int{}
	for _, v := range vs {
		switch v.Final {
		case model.VerdictVisible:
			visible = append(visible, v.ChunkID)
		case model.VerdictOccluded:
			occluded = append(occluded, v.ChunkID)
		default:
			uncertain = append(uncertain, v.ChunkID)
		}
	}
	return visible, occluded, uncertain
}

// ExactMargins contains unquantized geometric margins used by the deterministic
// truth oracle. It performs no verdict classification itself.
type ExactMargins struct {
	CoarseMargin  float64
	CoarseRef     int
	CoarseOK      bool
	FrustumMargin float64
	FrustumInside bool
	ScreenBounds  [4]float64
	DepthMargin   float64
	DepthRef      int
	HasDepth      bool
}

func AnalyzeExact(p *model.Project, camSample model.CameraSample, shadow *geo.DepthMap, chunk model.Chunk) ExactMargins {
	cam := geo.NewCamera(camSample, p.Parameters.ViewportWidth, p.Parameters.ViewportHeight)
	box := ChunkBounds(p, chunk)
	coarseMargin, coarseRef, coarseOK := coarseShadowMargin(chunk, shadow)
	frustumWorld, inside, screen := cam.FrustumMargin(box)
	depthMargin, _, depthRef, hasDepth := fineDepthMargin(p, cam, chunk)
	return ExactMargins{
		CoarseMargin: coarseMargin, CoarseRef: coarseRef, CoarseOK: coarseOK,
		FrustumMargin: frustumWorld / camSample.Far, FrustumInside: inside, ScreenBounds: screen,
		DepthMargin: depthMargin, DepthRef: depthRef, HasDepth: hasDepth,
	}
}
