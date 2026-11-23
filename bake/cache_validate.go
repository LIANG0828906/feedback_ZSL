package bake

import (
	"math"

	"terrain-bake/project"
)

func validateArtifactShape(p *project.Project, params Params, a *Artifact) *project.Error {
	bd, ok := p.Blocks[a.BlockID]
	if !ok {
		return project.NewError(project.CodeCache, a.BlockID, "artifact", "artifact references unknown block")
	}
	vertices := params.VertexCount(bd.Def.Grid.Cols, bd.Def.Grid.Rows)
	if len(a.Positions) != vertices || len(a.Normals) != vertices {
		return project.NewError(project.CodeCache, a.BlockID, "artifact.vertices", "artifact vertex buffer shape is invalid")
	}
	if len(a.MaterialWeights) != vertices*bd.Def.Material.Layers {
		return project.NewError(project.CodeCache, a.BlockID, "artifact.materialWeights", "artifact material buffer shape is invalid")
	}
	if len(a.MaterialRatios) != bd.Def.Material.Layers {
		return project.NewError(project.CodeCache, a.BlockID, "artifact.materialRatios", "artifact material ratio count is invalid")
	}
	for i := range a.Positions {
		if !finiteVec(a.Positions[i]) || !finiteVec(a.Normals[i]) {
			return project.NewError(project.CodeCache, a.BlockID, "artifact.vertices", "artifact contains non-finite vertex data")
		}
	}
	if !finiteVec(a.Bounds.Min) || !finiteVec(a.Bounds.Max) {
		return project.NewError(project.CodeCache, a.BlockID, "artifact.bounds", "artifact bounds are not finite")
	}
	return nil
}

func finiteVec(v Vec3) bool {
	return !math.IsNaN(v.X) && !math.IsInf(v.X, 0) &&
		!math.IsNaN(v.Y) && !math.IsInf(v.Y, 0) &&
		!math.IsNaN(v.Z) && !math.IsInf(v.Z, 0)
}
