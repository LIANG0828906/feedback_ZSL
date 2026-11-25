package bake

import (
	"math"

	"terrainpipeline/internal/model"
)

func BakeBlock(project *model.Project, block *model.Block) *model.BlockResult {
	result := &model.BlockResult{
		BlockID:     block.ID,
		NX:          block.NX,
		NZ:          block.NZ,
		Pixels:      make([]model.PixelBake, 0, block.NX*block.NZ),
		NormalBytes: int64(block.NX*block.NZ) * 32,
		PixelBuffer: block.NX * block.NZ,
	}
	for j := 0; j < block.NZ; j++ {
		for i := 0; i < block.NX; i++ {
			result.Pixels = append(result.Pixels, bakePixel(project, block, i, j))
		}
	}
	Finalize(result)
	return result
}

func BakeBlockTiled(project *model.Project, block *model.Block, scratch []model.PixelBake) *model.BlockResult {
	if len(scratch) == 0 {
		return BakeBlock(project, block)
	}
	result := &model.BlockResult{
		BlockID:     block.ID,
		NX:          block.NX,
		NZ:          block.NZ,
		Pixels:      make([]model.PixelBake, 0, block.NX*block.NZ),
		NormalBytes: int64(block.NX*block.NZ) * 32,
		PixelBuffer: min(len(scratch), block.NX*block.NZ),
	}
	tile := len(scratch)
	for start := 0; start < block.NX*block.NZ; start += tile {
		end := min(start+tile, block.NX*block.NZ)
		for p := start; p < end; p++ {
			i := p % block.NX
			j := p / block.NX
			scratch[p-start] = bakePixel(project, block, i, j)
		}
		result.Pixels = append(result.Pixels, scratch[:end-start]...)
	}
	Finalize(result)
	return result
}

func Finalize(result *model.BlockResult) {
	normals := make([]model.Vec3, len(result.Pixels))
	slopes := make([]float64, len(result.Pixels))
	weights := make([][]uint8, len(result.Pixels))
	for i, p := range result.Pixels {
		normals[i] = p.Normal
		slopes[i] = p.SlopeDeg
		weights[i] = p.Weights
	}
	result.NormalDigest = model.HashJSON("normal-v1", normals)
	result.SlopeDigest = model.HashJSON("slope-v1", slopes)
	result.WeightDigest = model.HashJSON("weights-v1", weights)
	result.ResultHash = model.HashJSON("block-result-v1", struct {
		Normal string `json:"normal"`
		Slope  string `json:"slope"`
		Weight string `json:"weight"`
	}{result.NormalDigest, result.SlopeDigest, result.WeightDigest})
}

func bakePixel(project *model.Project, block *model.Block, i, j int) model.PixelBake {
	dhx, dhz := HeightDerivatives(project.Params.Differential, block, i, j)
	scale := project.Params.HeightScale
	geometryNormal := NormalFromDerivatives(dhx, dhz, scale)
	normal := blendMaterialNormals(project, block, i, j, geometryNormal, dhx, dhz)
	slope := math.Atan(math.Hypot(scale*dhx, scale*dhz)) * 180 / math.Pi
	weights := append([]uint8(nil), block.Weights[block.Index(i, j)]...)
	return model.PixelBake{
		I: i, J: j,
		Normal:   normal,
		RGB:      NormalRGB(normal),
		SlopeDeg: round9(slope),
		Weights:  weights,
	}
}

func HeightDerivatives(operator string, block *model.Block, i, j int) (float64, float64) {
	switch operator {
	case "forward":
		return forwardDerivatives(block, i, j)
	case "sobel":
		return sobelDerivatives(block, i, j)
	default:
		return centralDerivatives(block, i, j)
	}
}

func centralDerivatives(b *model.Block, i, j int) (float64, float64) {
	var dhx float64
	if i == 0 {
		dhx = (b.Height(1, j) - b.Height(0, j)) / b.DX
	} else if i == b.NX-1 {
		dhx = (b.Height(b.NX-1, j) - b.Height(b.NX-2, j)) / b.DX
	} else {
		dhx = (b.Height(i+1, j) - b.Height(i-1, j)) / (2 * b.DX)
	}
	var dhz float64
	if j == 0 {
		dhz = (b.Height(i, 1) - b.Height(i, 0)) / b.DZ
	} else if j == b.NZ-1 {
		dhz = (b.Height(i, b.NZ-1) - b.Height(i, b.NZ-2)) / b.DZ
	} else {
		dhz = (b.Height(i, j+1) - b.Height(i, j-1)) / (2 * b.DZ)
	}
	return dhx, dhz
}

func forwardDerivatives(b *model.Block, i, j int) (float64, float64) {
	var dhx float64
	if i < b.NX-1 {
		dhx = (b.Height(i+1, j) - b.Height(i, j)) / b.DX
	} else {
		dhx = (b.Height(i, j) - b.Height(i-1, j)) / b.DX
	}
	var dhz float64
	if j < b.NZ-1 {
		dhz = (b.Height(i, j+1) - b.Height(i, j)) / b.DZ
	} else {
		dhz = (b.Height(i, j) - b.Height(i, j-1)) / b.DZ
	}
	return dhx, dhz
}

func sobelDerivatives(b *model.Block, i, j int) (float64, float64) {
	if b.NX < 3 || b.NZ < 3 || i == 0 || j == 0 || i == b.NX-1 || j == b.NZ-1 {
		return centralDerivatives(b, i, j)
	}
	at := func(di, dj int) float64 { return b.Height(i+di, j+dj) }
	sx := -at(-1, -1) + at(1, -1) - 2*at(-1, 0) + 2*at(1, 0) - at(-1, 1) + at(1, 1)
	sz := -at(-1, -1) - 2*at(0, -1) - at(1, -1) + at(-1, 1) + 2*at(0, 1) + at(1, 1)
	return sx / (8 * b.DX), sz / (8 * b.DZ)
}

func NormalFromDerivatives(dhx, dhz, scale float64) model.Vec3 {
	return Normalize(model.Vec3{-scale * dhx, -scale * dhz, 1})
}

func blendMaterialNormals(project *model.Project, block *model.Block, i, j int, geometry model.Vec3, dhx, dhz float64) model.Vec3 {
	weights := block.Weights[block.Index(i, j)]
	blended := model.Vec3{}
	for layerIndex, weight := range weights {
		if weight == 0 {
			continue
		}
		layer := project.Layers[layerIndex]
		layerNormal := materialNormal(project, block, i, j, layerIndex, layer, geometry)
		w := float64(weight) / 255
		blended[0] += w * layerNormal[0]
		blended[1] += w * layerNormal[1]
		blended[2] += w * layerNormal[2]
	}
	return Normalize(blended)
}

func materialNormal(project *model.Project, block *model.Block, i, j, layerIndex int, layer model.Layer, geometry model.Vec3) model.Vec3 {
	if layerIndex == 0 || layer.Strength == 0 || layer.Frequency == 0 {
		return geometry
	}
	wx := block.OriginX + float64(i)*block.DX
	wz := block.OriginZ + float64(j)*block.DZ
	detailX := layer.Strength * 0.75 * math.Sin(wx*layer.Frequency+float64(layerIndex)*1.37)
	detailZ := layer.Strength * 0.75 * math.Cos(wz*layer.Frequency*1.17+float64(layerIndex)*0.91)
	return Normalize(model.Vec3{geometry[0] - detailX, geometry[1] - detailZ, geometry[2]})
}

func NormalRGB(n model.Vec3) [3]uint8 {
	return [3]uint8{toByte(n[0]), toByte(n[1]), toByte(n[2])}
}

func toByte(v float64) uint8 {
	c := math.Round((v*0.5 + 0.5) * 255)
	if c < 0 {
		c = 0
	}
	if c > 255 {
		c = 255
	}
	return uint8(c)
}

func Normalize(v model.Vec3) model.Vec3 {
	l := math.Sqrt(v[0]*v[0] + v[1]*v[1] + v[2]*v[2])
	if l == 0 || math.IsNaN(l) || math.IsInf(l, 0) {
		return model.Vec3{0, 0, 1}
	}
	return model.Vec3{v[0] / l, v[1] / l, v[2] / l}
}

func AngleDeg(a, b model.Vec3) float64 {
	dot := a[0]*b[0] + a[1]*b[1] + a[2]*b[2]
	if dot > 1 {
		dot = 1
	} else if dot < -1 {
		dot = -1
	}
	return math.Acos(dot) * 180 / math.Pi
}

func round9(v float64) float64 {
	return math.Round(v*1e9) / 1e9
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
