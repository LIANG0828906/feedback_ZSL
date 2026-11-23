package bake

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"terrain-bake/project"
)

// AlgorithmVersion is folded into the snapshot parameter hash. Bump it when
// bake math changes in a way that must invalidate previously persisted bytes.
const AlgorithmVersion = "seam-projection-v1"

// Params are the effective parameters for one bake run. MaxWorkers and
// MaxVerticesPerThread bound resources but do not affect artifact bytes.
type Params struct {
	Subdivisions          int     `json:"subdivisions"`
	QuantMax              int     `json:"quantMax"`
	SeamMaxIterations     int     `json:"seamMaxIterations"`
	SeamAngleThresholdDeg float64 `json:"seamAngleThresholdDeg"`
	SeamMaterialBandCells float64 `json:"seamMaterialBandCells"`
	ParamsVersion         string  `json:"paramsVersion"`
	MaxWorkers            int     `json:"maxWorkers"`
	MaxVerticesPerThread  int     `json:"maxVerticesPerThread"`
}

func ParamsFromManifest(m project.Manifest) Params {
	return Params{
		Subdivisions:          m.Bake.Subdivisions,
		QuantMax:              m.Bake.QuantMax,
		SeamMaxIterations:     m.Bake.SeamMaxIterations,
		SeamAngleThresholdDeg: m.Bake.SeamAngleThresholdDeg,
		SeamMaterialBandCells: m.Bake.SeamMaterialBandCells,
		ParamsVersion:         m.Bake.ParamsVersion,
		MaxWorkers:            m.Bake.MaxWorkers,
		MaxVerticesPerThread:  m.Bake.MaxVerticesPerThread,
	}
}

func (p Params) Validate() *project.Error {
	switch {
	case p.Subdivisions <= 0:
		return project.NewError(project.CodeSchema, "", "bake.subdivisions", "subdivisions must be positive")
	case p.QuantMax <= 0 || p.QuantMax > 65535:
		return project.NewError(project.CodeSchema, "", "bake.quantMax", "quantMax must be in [1,65535]")
	case p.SeamMaxIterations < 0:
		return project.NewError(project.CodeSchema, "", "bake.seamMaxIterations", "seamMaxIterations must be non-negative")
	case p.SeamAngleThresholdDeg < 0:
		return project.NewError(project.CodeSchema, "", "bake.seamAngleThresholdDeg", "seam angle threshold must be non-negative")
	case p.SeamMaterialBandCells < 0:
		return project.NewError(project.CodeSchema, "", "bake.seamMaterialBandCells", "material band must be non-negative")
	case p.ParamsVersion == "":
		return project.NewError(project.CodeSchema, "", "bake.paramsVersion", "paramsVersion must not be empty")
	case p.MaxWorkers <= 0:
		return project.NewError(project.CodeSchema, "", "bake.maxWorkers", "maxWorkers must be positive")
	case p.MaxVerticesPerThread <= 0:
		return project.NewError(project.CodeSchema, "", "bake.maxVerticesPerThread", "maxVerticesPerThread must be positive")
	}
	return nil
}

// ArtifactHash covers the artifact-affecting fields only. Changing worker
// count or vertex admission limits does not invalidate persisted artifacts.
func (p Params) ArtifactHash() string {
	type artifactParams struct {
		Algorithm             string  `json:"algorithm"`
		Subdivisions          int     `json:"subdivisions"`
		QuantMax              int     `json:"quantMax"`
		SeamMaxIterations     int     `json:"seamMaxIterations"`
		SeamAngleThresholdDeg float64 `json:"seamAngleThresholdDeg"`
		SeamMaterialBandCells float64 `json:"seamMaterialBandCells"`
		ParamsVersion         string  `json:"paramsVersion"`
	}
	a := artifactParams{
		Algorithm:             AlgorithmVersion,
		Subdivisions:          p.Subdivisions,
		QuantMax:              p.QuantMax,
		SeamMaxIterations:     p.SeamMaxIterations,
		SeamAngleThresholdDeg: p.SeamAngleThresholdDeg,
		SeamMaterialBandCells: p.SeamMaterialBandCells,
		ParamsVersion:         p.ParamsVersion,
	}
	b, _ := json.Marshal(a)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (p Params) VertexCount(blockCols, blockRows int) int {
	return ((blockCols-1)*p.Subdivisions + 1) * ((blockRows-1)*p.Subdivisions + 1)
}

func (p Params) String() string {
	return fmt.Sprintf("subdiv=%d quant=%d seamIter=%d angle=%.6g band=%.6g version=%s workers=%d vertexLimit=%d",
		p.Subdivisions, p.QuantMax, p.SeamMaxIterations, p.SeamAngleThresholdDeg,
		p.SeamMaterialBandCells, p.ParamsVersion, p.MaxWorkers, p.MaxVerticesPerThread)
}
