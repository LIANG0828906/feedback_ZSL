package workbench

import "math"

// RenderScene executes a complete render using the requested path. Both paths
// call the same deterministic pixel kernel; the tiled path additionally limits
// per-block working sets and records its downsampling policy.
func RenderScene(scene *Scene, cfg ProcessConfig, path string) (*RenderResult, error) {
	if scene == nil {
		return nil, fieldErr(CodeMeta, "scene", nil, "non-nil scene", "scene is missing")
	}
	if err := ValidateScene(scene); err != nil {
		return nil, err
	}
	cfg = cfg.normalized()
	cfg.Path = path
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	scale := DecideScale(scene.Meta, cfg)
	populateCoarseStats(scene, &scale, cfg)
	ctx, err := newRenderContext(scene, cfg, scale)
	if err != nil {
		return nil, err
	}
	width, height := scene.Meta.Width, scene.Meta.Height
	result := &RenderResult{
		SceneID:         scene.ID,
		Width:           width,
		Height:          height,
		Path:            scale.SelectedPath,
		Scale:           scale,
		FusedRadiance:   make([]float64, width*height),
		LinearRGB:       make([]float64, width*height*3),
		DisplayRGB:      make([]uint8, width*height*3),
		GamutMask:       make([]uint8, width*height),
		GamutBoundaryXY: append([]XY(nil), ctx.boundaryXY...),
	}
	start := nowMS()
	if scale.SelectedPath == PathFull {
		block, err := ctx.renderBlock(blockRect{X: 0, Y: 0, W: width, H: height})
		if err != nil {
			return nil, err
		}
		copy(result.FusedRadiance, block.Fused)
		copy(result.LinearRGB, block.LinearRGB)
		copy(result.DisplayRGB, block.Display)
		copy(result.GamutMask, block.GamutMask)
		result.PeakRadiance = block.Peak
		result.MeanRadiance = block.Mean
		result.OutOfGamut = block.OutOfGamut
		result.OutOfGamutSample = block.OOFSamples
	} else {
		tw := maxInt(1, scale.TileWidth)
		th := maxInt(1, scale.TileHeight)
		var total, count float64
		var samples []PixelCoord
		for y := 0; y < height; y += th {
			for x := 0; x < width; x += tw {
				rect := roundRect(x, y, tw, th, width, height)
				block, err := ctx.renderBlock(rect)
				if err != nil {
					return nil, err
				}
				for ly := 0; ly < rect.H; ly++ {
					src := ly * rect.W
					dst := (rect.Y+ly)*width + rect.X
					copy(result.FusedRadiance[dst:dst+rect.W], block.Fused[src:src+rect.W])
					copy(result.LinearRGB[dst*3:(dst+rect.W)*3], block.LinearRGB[src*3:(src+rect.W)*3])
					copy(result.GamutMask[dst:dst+rect.W], block.GamutMask[src:src+rect.W])
					copy(result.DisplayRGB[dst*3:(dst+rect.W)*3], block.Display[src*3:(src+rect.W)*3])
				}
				result.PeakRadiance = math.Max(result.PeakRadiance, block.Peak)
				total += block.Mean * float64(rect.pixels())
				count += float64(rect.pixels())
				result.OutOfGamut += block.OutOfGamut
				if len(samples) < 512 {
					remaining := 512 - len(samples)
					if len(block.OOFSamples) > remaining {
						samples = append(samples, block.OOFSamples[:remaining]...)
					} else {
						samples = append(samples, block.OOFSamples...)
					}
				}
			}
		}
		if count > 0 {
			result.MeanRadiance = total / count
		}
		result.OutOfGamutSample = samples
	}
	result.OutOfGamutHash = hashGamutMask(result.GamutMask, width)
	result.ElapsedMS = nowMS() - start
	return result, nil
}
