package pipeline

import (
	"fmt"

	"cadpipe/internal/cad"
	"cadpipe/internal/render"
)

func (p *Pipeline) patchOrError(id string) (*cad.Patch, error) {
	patch := p.Scene.Patch(id)
	if patch == nil {
		return nil, cad.NewError(cad.CodeVersion, "patch %q not found", id).
			WithPatch(id).WithPath("$.patches")
	}
	return patch, nil
}

func cloneTileSet(in map[int]bool) map[int]bool {
	out := make(map[int]bool, len(in))
	for id := range in {
		out[id] = true
	}
	return out
}

func (p *Pipeline) planGeometryChange(patchID, reason string, oldTiles, newTiles map[int]bool) *InvalidationPlan {
	plan := &InvalidationPlan{}
	union := make(map[int]bool)
	for id := range oldTiles {
		union[id] = true
	}
	for id := range newTiles {
		union[id] = true
	}
	for tileID := range union {
		plan.add(p, tileID, patchID, reason)
	}
	plan.finalize(p, patchID)
	p.deletePlanCache(plan)
	return plan
}

func (p *Pipeline) deletePlanCache(plan *InvalidationPlan) {
	for _, id := range plan.TileIDs {
		delete(p.Cache, id)
	}
}

func (p *Pipeline) bump(id string, mutate func(*PatchVersions)) {
	v := p.Versions.Patches[id]
	mutate(&v)
	p.Versions.Patches[id] = v
}

func (p *Pipeline) UpdateControlPoint(id string, u, v int, position cad.Point3) (*InvalidationPlan, error) {
	patch, err := p.patchOrError(id)
	if err != nil {
		return nil, err
	}
	if u < 0 || u >= len(patch.ControlPoints) || v < 0 || v >= len(patch.ControlPoints[0]) {
		return nil, cad.NewError(cad.CodeParam, "control point index (%d,%d) out of range", u, v).
			WithPatch(id).WithPath(fmt.Sprintf("$.patches.%s.control_points[%d][%d]", id, u, v))
	}
	oldTiles := cloneTileSet(p.PatchTiles[id])
	patch.ControlPoints[u][v] = position
	p.bump(id, func(v *PatchVersions) { v.Control++ })
	newTiles, err := p.RefreshPatchCoverage(id)
	if err != nil {
		return nil, err
	}
	return p.planGeometryChange(id, "control_point", oldTiles, newTiles), nil
}

func (p *Pipeline) UpdateKnots(id string, knotsU, knotsV []float64) (*InvalidationPlan, error) {
	patch, err := p.patchOrError(id)
	if err != nil {
		return nil, err
	}
	oldTiles := cloneTileSet(p.PatchTiles[id])
	if knotsU != nil {
		patch.KnotsU = append([]float64(nil), knotsU...)
	}
	if knotsV != nil {
		patch.KnotsV = append([]float64(nil), knotsV...)
	}
	p.bump(id, func(v *PatchVersions) { v.Knot++ })
	newTiles, err := p.RefreshPatchCoverage(id)
	if err != nil {
		return nil, err
	}
	return p.planGeometryChange(id, "knot_vector", oldTiles, newTiles), nil
}

func (p *Pipeline) UpdateWeights(id string, weights [][]float64) (*InvalidationPlan, error) {
	patch, err := p.patchOrError(id)
	if err != nil {
		return nil, err
	}
	oldTiles := cloneTileSet(p.PatchTiles[id])
	patch.Weights = cloneWeights(weights)
	p.bump(id, func(v *PatchVersions) { v.Weight++ })
	newTiles, err := p.RefreshPatchCoverage(id)
	if err != nil {
		return nil, err
	}
	return p.planGeometryChange(id, "weight", oldTiles, newTiles), nil
}

func cloneWeights(in [][]float64) [][]float64 {
	out := make([][]float64, len(in))
	for i := range in {
		out[i] = append([]float64(nil), in[i]...)
	}
	return out
}

func (p *Pipeline) UpdateMaterial(id string, material cad.Material) (*InvalidationPlan, error) {
	patch, err := p.patchOrError(id)
	if err != nil {
		return nil, err
	}
	patch.Material = &material
	p.bump(id, func(v *PatchVersions) { v.Material++ })
	plan := &InvalidationPlan{}
	for tileID := range p.PatchTiles[id] {
		plan.add(p, tileID, id, "material")
	}
	plan.finalize(p, id)
	p.deletePlanCache(plan)
	return plan, nil
}

func (p *Pipeline) UpdateSubdivision(id string, params cad.Subdivision) (*InvalidationPlan, error) {
	patch, err := p.patchOrError(id)
	if err != nil {
		return nil, err
	}
	oldTiles := cloneTileSet(p.PatchTiles[id])
	patch.Subdivision = &params
	p.bump(id, func(v *PatchVersions) { v.Subdivision++ })
	newTiles, err := p.RefreshPatchCoverage(id)
	if err != nil {
		return nil, err
	}
	return p.planGeometryChange(id, "subdivision", oldTiles, newTiles), nil
}

func (p *Pipeline) UpdateClip(id string, clip cad.ClipBoundary) (*InvalidationPlan, error) {
	patch, err := p.patchOrError(id)
	if err != nil {
		return nil, err
	}
	if err := cad.ValidateClip(id, clip); err != nil {
		return nil, err
	}
	oldValues := make(map[string]bool)
	for _, probe := range p.Probes {
		if probe.PatchID == id {
			oldValues[probe.ID] = cad.PointInsideClip(patch.Clip, cad.Vec2{U: probe.U, V: probe.V})
		}
	}
	patch.Clip = clip
	p.bump(id, func(v *PatchVersions) { v.Clip++ })
	if _, err := p.RefreshPatchCoverage(id); err != nil {
		return nil, err
	}

	plan := &InvalidationPlan{}
	for _, probe := range p.Probes {
		if probe.PatchID != id {
			continue
		}
		now := cad.PointInsideClip(clip, cad.Vec2{U: probe.U, V: probe.V})
		if oldValues[probe.ID] == now {
			continue
		}
		tileID, ok := probeTileWithPipelineCamera(p, probe)
		if !ok {
			continue
		}
		plan.add(p, tileID, id, "clip_boundary", probe.ID)
	}
	plan.finalize(p, id)
	p.deletePlanCache(plan)
	return plan, nil
}

func (p *Pipeline) UpdateCamera(camera *render.Camera) (*InvalidationPlan, error) {
	if camera == nil {
		return nil, cad.NewError(cad.CodeParam, "camera must not be nil").WithPath("camera")
	}
	p.Camera = camera
	p.Versions.Camera++
	camera.Version = p.Versions.Camera
	if err := p.RebuildCoverage(); err != nil {
		return nil, err
	}
	plan := &InvalidationPlan{}
	for tileID := 0; tileID < p.Camera.TileCount(); tileID++ {
		patchIDs := sortedKeys(p.TilePatches[tileID])
		if len(patchIDs) == 0 {
			continue
		}
		for _, id := range patchIDs {
			plan.add(p, tileID, id, "camera_version")
		}
	}
	plan.finalize(p)
	for id := 0; id < p.Camera.TileCount(); id++ {
		delete(p.Cache, id)
	}
	return plan, nil
}

func probeTileWithPipelineCamera(p *Pipeline, probe cad.ClipProbe) (int, bool) {
	patch := p.Scene.Patch(probe.PatchID)
	if patch == nil {
		return 0, false
	}
	return render.ProbeTile(p.Camera, patch, probe.U, probe.V)
}
