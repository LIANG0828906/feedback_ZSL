package bake

import (
	"fmt"
	"math"

	"terrainnormalverify/internal/model"
)

type BlockResult struct {
	BlockID    int               `json:"blockId"`
	Width      int               `json:"width"`
	Height     int               `json:"height"`
	OriginX    int               `json:"originX"`
	OriginY    int               `json:"originY"`
	RawNormals []model.Vec3      `json:"rawNormals"`
	Normals    []model.Vec3      `json:"normals"`
	RawSlopes  []float64         `json:"rawSlopes"`
	Slopes     []float64         `json:"slopes"`
	Colors     []uint8           `json:"colors"`
	BBox       model.BoundingBox `json:"bbox"`
}

func (r *BlockResult) PixelCount() int {
	return r.Width * r.Height
}

func (r *BlockResult) Index(x, y int) int {
	return y*r.Width + x
}

func (r *BlockResult) Clone() *BlockResult {
	out := *r
	out.RawNormals = append([]model.Vec3(nil), r.RawNormals...)
	out.Normals = append([]model.Vec3(nil), r.Normals...)
	out.RawSlopes = append([]float64(nil), r.RawSlopes...)
	out.Slopes = append([]float64(nil), r.Slopes...)
	out.Colors = append([]uint8(nil), r.Colors...)
	return &out
}

func ValidateParameters(params model.BakeParameters) error {
	switch params.DifferenceOperator {
	case "central", "forward", "sobel":
	default:
		e := model.NewError(model.ErrPara, -1, "differenceOperator", "must be central, forward, or sobel")
		e.Value = params.DifferenceOperator
		return e
	}
	if params.Scale < 1e-6 || params.Scale > 1e4 || math.IsNaN(params.Scale) || math.IsInf(params.Scale, 0) {
		e := model.NewError(model.ErrPara, -1, "scale", "must be finite and in [1e-6,1e4]")
		e.Value = params.Scale
		return e
	}
	if params.MaterialMix != "weighted" && params.MaterialMix != "dominant" {
		e := model.NewError(model.ErrPara, -1, "materialMix", "must be weighted or dominant")
		e.Value = params.MaterialMix
		return e
	}
	if params.SeamRelaxation <= 0 || params.SeamRelaxation > 0.5 || math.IsNaN(params.SeamRelaxation) {
		e := model.NewError(model.ErrPara, -1, "seamRelaxation", "must be in (0,0.5]")
		e.Value = params.SeamRelaxation
		return e
	}
	return nil
}

func BakePreview(block model.Block, layers []model.MaterialLayer, params model.BakeParameters, bufferPixels int) (*BlockResult, error) {
	params.PreviewOnly = true
	return BakeBlock(block, layers, params, bufferPixels)
}

func BakeExact(block model.Block, layers []model.MaterialLayer, params model.BakeParameters, bufferPixels int) (*BlockResult, error) {
	params.PreviewOnly = false
	return BakeBlock(block, layers, params, bufferPixels)
}

func BakeBlock(block model.Block, layers []model.MaterialLayer, params model.BakeParameters, bufferPixels int) (*BlockResult, error) {
	if err := ValidateParameters(params); err != nil {
		return nil, err
	}
	if block.Width <= 0 || block.Height <= 0 {
		return nil, model.NewError(model.ErrSize, block.ID, "size", "block dimensions must be positive")
	}
	if len(block.Heights) != block.PixelCount() {
		e := model.NewError(model.ErrSize, block.ID, "heights", "sample count does not match block size")
		e.Value = len(block.Heights)
		e.Want = block.PixelCount()
		return nil, e
	}
	if len(block.Weights) != block.PixelCount() {
		e := model.NewError(model.ErrSize, block.ID, "weights", "weight row count does not match block size")
		e.Value = len(block.Weights)
		e.Want = block.PixelCount()
		return nil, e
	}
	if bufferPixels <= 0 {
		e := model.NewError(model.ErrBudget, block.ID, "perThreadPixelBuffer", "pixel buffer budget must be positive")
		e.Value = bufferPixels
		return nil, e
	}
	for i, row := range block.Weights {
		if len(row) != len(layers) {
			e := model.NewError(model.ErrSize, block.ID, "weights", "weight row does not match material layer count")
			e.Value = i
			e.Want = len(layers)
			return nil, e
		}
		total := 0
		for j, weight := range row {
			if weight < 0 || weight > model.WeightScale {
				e := model.NewError(model.ErrWeight, block.ID, fmt.Sprintf("weights[%d][%d]", i, j), "weight is outside [0,1000]")
				e.Value = weight
				return nil, e
			}
			total += weight
		}
		if total == 0 {
			e := model.NewError(model.ErrWeight, block.ID, fmt.Sprintf("weights[%d]", i), "weight row must have a positive sum")
			e.Value = total
			return nil, e
		}
	}

	result := &BlockResult{
		BlockID:    block.ID,
		Width:      block.Width,
		Height:     block.Height,
		OriginX:    block.OriginX,
		OriginY:    block.OriginY,
		RawNormals: make([]model.Vec3, block.PixelCount()),
		Normals:    make([]model.Vec3, block.PixelCount()),
		RawSlopes:  make([]float64, block.PixelCount()),
		Slopes:     make([]float64, block.PixelCount()),
	}
	tileWidth := block.Width
	if tileWidth > bufferPixels {
		tileWidth = bufferPixels
	}
	tileHeight := bufferPixels / tileWidth
	if tileHeight < 1 {
		tileHeight = 1
	}
	scratch := make([]model.Vec3, tileWidth*tileHeight)
	for tileY := 0; tileY < block.Height; tileY += tileHeight {
		rows := tileHeight
		if tileY+rows > block.Height {
			rows = block.Height - tileY
		}
		for tileX := 0; tileX < block.Width; tileX += tileWidth {
			columns := tileWidth
			if tileX+columns > block.Width {
				columns = block.Width - tileX
			}
			for y := 0; y < rows; y++ {
				for x := 0; x < columns; x++ {
					gx, gy := gradient(&block, tileX+x, tileY+y, params.DifferenceOperator)
					base := model.Vec3{
						X: -gx * params.Scale,
						Y: -gy * params.Scale,
						Z: 1,
					}.Normalize()
					normal := MixMaterial(base, block.WeightsAt(tileX+x, tileY+y), layers, params.MaterialMix)
					scratch[y*tileWidth+x] = normal
				}
			}
			for y := 0; y < rows; y++ {
				dst := (tileY+y)*block.Width + tileX
				src := y * tileWidth
				copy(result.RawNormals[dst:dst+columns], scratch[src:src+columns])
			}
		}
	}
	copy(result.Normals, result.RawNormals)
	for i := range result.Normals {
		result.RawSlopes[i] = model.SlopeDeg(result.RawNormals[i])
	}
	RecomputeDerived(result)
	return result, nil
}

func gradient(block *model.Block, x, y int, operator string) (float64, float64) {
	switch operator {
	case "forward":
		sample := func(sx, sy int) float64 {
			return block.HeightAt(clampInt(sx, 0, block.Width-1), clampInt(sy, 0, block.Height-1))
		}
		return sample(x+1, y) - sample(x, y), sample(x, y+1) - sample(x, y)
	case "sobel":
		sample := func(sx, sy int) float64 {
			return block.HeightAt(clampInt(sx, 0, block.Width-1), clampInt(sy, 0, block.Height-1))
		}
		gx := (-sample(x-1, y-1) + sample(x+1, y-1) +
			-2*sample(x-1, y) + 2*sample(x+1, y) +
			-sample(x-1, y+1) + sample(x+1, y+1)) / 8
		gy := (-sample(x-1, y-1) - 2*sample(x, y-1) - sample(x+1, y-1) +
			sample(x-1, y+1) + 2*sample(x, y+1) + sample(x+1, y+1)) / 8
		return gx, gy
	default:
		sample := func(sx, sy int) float64 {
			return block.HeightAt(clampInt(sx, 0, block.Width-1), clampInt(sy, 0, block.Height-1))
		}
		return (sample(x+1, y) - sample(x-1, y)) * 0.5,
			(sample(x, y+1) - sample(x, y-1)) * 0.5
	}
}

func MixMaterial(base model.Vec3, weights []int, layers []model.MaterialLayer, mode string) model.Vec3 {
	if mode == "dominant" {
		best := 0
		for i := 1; i < len(weights); i++ {
			if weights[i] > weights[best] {
				best = i
			}
		}
		return layerNormal(base, layers[best])
	}
	total := 0
	for _, weight := range weights {
		total += weight
	}
	if total <= 0 {
		return base
	}
	acc := model.Vec3{}
	for i, layer := range layers {
		weight := float64(weights[i]) / float64(total)
		if weight == 0 {
			continue
		}
		acc = acc.Add(layerNormal(base, layer).Mul(weight))
	}
	return acc.Normalize()
}

func layerNormal(base model.Vec3, layer model.MaterialLayer) model.Vec3 {
	return model.Vec3{
		X: base.X + layer.NormalBiasX*layer.Strength,
		Y: base.Y + layer.NormalBiasY*layer.Strength,
		Z: base.Z + layer.NormalBiasZ*layer.Strength,
	}.Normalize()
}

func RecomputeDerived(result *BlockResult) {
	if len(result.Slopes) != result.PixelCount() {
		result.Slopes = make([]float64, result.PixelCount())
	}
	if len(result.Colors) != result.PixelCount()*3 {
		result.Colors = make([]uint8, result.PixelCount()*3)
	}
	bounds := model.BoundingBox{
		Min: model.Vec3{X: math.Inf(1), Y: math.Inf(1), Z: math.Inf(1)},
		Max: model.Vec3{X: math.Inf(-1), Y: math.Inf(-1), Z: math.Inf(-1)},
	}
	for i, normal := range result.Normals {
		result.Slopes[i] = model.SlopeDeg(normal)
		color := model.ColorCode(normal)
		copy(result.Colors[i*3:i*3+3], color[:])
		if normal.X < bounds.Min.X {
			bounds.Min.X = normal.X
		}
		if normal.Y < bounds.Min.Y {
			bounds.Min.Y = normal.Y
		}
		if normal.Z < bounds.Min.Z {
			bounds.Min.Z = normal.Z
		}
		if normal.X > bounds.Max.X {
			bounds.Max.X = normal.X
		}
		if normal.Y > bounds.Max.Y {
			bounds.Max.Y = normal.Y
		}
		if normal.Z > bounds.Max.Z {
			bounds.Max.Z = normal.Z
		}
	}
	result.BBox = bounds
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
