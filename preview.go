package fog

import (
	"fmt"
	"time"
)

type PreviewResult struct {
	StepBudget      int              `json:"step_budget_per_ray"`
	ShadowBudget    int              `json:"shadow_budget_per_ray"`
	RayCount        int              `json:"ray_count"`
	ActualSteps     int              `json:"actual_steps_total"`
	ActualShadow    int              `json:"actual_shadow_samples_total"`
	AverageSteps    float64          `json:"average_steps_per_ray"`
	AverageShadow   float64          `json:"average_shadow_samples_per_ray"`
	RecipeVersion   int64            `json:"recipe_version"`
	CameraVersion   int64            `json:"camera_version"`
	MediumVersions  map[string]int64 `json:"medium_versions"`
	ElapsedMS       int64            `json:"elapsed_ms"`
	DelayLimitMS    int64            `json:"delay_limit_ms"`
	WithinDelay     bool             `json:"within_delay"`
	MaxPixelDelta   float64          `json:"max_pixel_delta"`
	WithinTolerance bool             `json:"within_tolerance"`
	JudgementBasis  string           `json:"judgement_basis"`
	Frame           *Frame           `json:"frame"`
	Divergence      *Error           `json:"divergence,omitempty"`
}

func RenderDirectFrame(scene *Scene, options IntegrationOptions, mode string) (*Frame, error) {
	coords := TileGrid(scene.Viewport, scene.Recipe)
	tiles := make([]TileResult, 0, len(coords))
	for _, coord := range coords {
		tile, err := RenderTile(scene, coord, TileRenderConfig{Options: options, RayBufferLimit: scene.Recipe.TileWidth * scene.Recipe.TileHeight})
		if err != nil {
			return nil, err
		}
		tiles = append(tiles, *tile)
	}
	ordered := OrderedTiles(tiles)
	frame := FrameFromTiles(scene, ordered, mode)
	return frame, nil
}

func RenderPreview(scene *Scene) (*PreviewResult, error) {
	options := IntegrationOptions{
		StepBudget:   scene.Recipe.PreviewStepsPerRay,
		ShadowBudget: scene.Recipe.PreviewShadowBudget,
	}
	start := time.Now()
	frame, err := RenderDirectFrame(scene, options, "preview")
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		return nil, err
	}
	result := &PreviewResult{
		StepBudget:     options.StepBudget,
		ShadowBudget:   options.ShadowBudget,
		RayCount:       scene.Viewport.Width * scene.Viewport.Height,
		ActualSteps:    frame.Steps,
		ActualShadow:   frame.ShadowSamples,
		AverageSteps:   float64(frame.Steps) / float64(scene.Viewport.Width*scene.Viewport.Height),
		AverageShadow:  float64(frame.ShadowSamples) / float64(scene.Viewport.Width*scene.Viewport.Height),
		RecipeVersion:  scene.Recipe.Version,
		CameraVersion:  scene.CameraTrack.Version,
		MediumVersions: mediumVersionMap(scene),
		ElapsedMS:      elapsed,
		DelayLimitMS:   scene.Recipe.PreviewDelayMS,
		WithinDelay:    elapsed <= scene.Recipe.PreviewDelayMS,
		Frame:          frame,
	}
	result.JudgementBasis = fmt.Sprintf("steps<=%d, shadow_samples<=%d, elapsed_ms<=%d, pixel_radiance_tolerance=%.3f", result.StepBudget, result.ShadowBudget, scene.Recipe.PreviewDelayMS, scene.Tolerances.PreviewPixel)
	return result, nil
}

func mediumVersionMap(scene *Scene) map[string]int64 {
	out := map[string]int64{}
	for _, medium := range scene.Media {
		out[medium.ID] = medium.Version
	}
	return out
}

func RenderExact(scene *Scene) (*Frame, error) {
	return RenderDirectFrame(scene, IntegrationOptions{}, "exact")
}

func EvaluatePreview(scene *Scene, preview *PreviewResult, exact *Frame) *PreviewResult {
	if preview == nil || preview.Frame == nil || exact == nil {
		return preview
	}
	preview.MaxPixelDelta, preview.Divergence = ComparePreviewExact(scene, preview.Frame, exact)
	preview.WithinTolerance = preview.Divergence == nil
	if preview.WithinTolerance && preview.WithinDelay {
		preview.JudgementBasis += "; verdict=consistent_within_tolerance_and_delay"
	} else if preview.Divergence != nil {
		preview.JudgementBasis += "; verdict=E_DIVERGE"
	} else {
		preview.JudgementBasis += "; verdict=preview_delay_exceeded"
	}
	return preview
}

func ComparePreviewExact(scene *Scene, preview, exact *Frame) (float64, *Error) {
	exactPixels := map[string]RenderPixel{}
	for _, pixel := range exact.Pixels {
		exactPixels[fmt.Sprintf("%d,%d", pixel.X, pixel.Y)] = pixel
	}
	maxDelta := 0.0
	for y := 0; y < scene.Viewport.Height; y++ {
		for x := 0; x < scene.Viewport.Width; x++ {
			var pp *RenderPixel
			for i := range preview.Pixels {
				if preview.Pixels[i].X == x && preview.Pixels[i].Y == y {
					pp = &preview.Pixels[i]
					break
				}
			}
			ep, ok := exactPixels[fmt.Sprintf("%d,%d", x, y)]
			if pp == nil || !ok {
				e := newError(CodeDiverge, "preview/exact pixel missing at (%d,%d)", x, y)
				e.Pixel = &PixelCoord{X: x, Y: y}
				return maxDelta, e
			}
			deltas := [3]float64{}
			pixelMax := 0.0
			for c := 0; c < 3; c++ {
				deltas[c] = floatAbs(pp.Radiance[c] - ep.Radiance[c])
				if deltas[c] > pixelMax {
					pixelMax = deltas[c]
				}
			}
			if pixelMax > maxDelta {
				maxDelta = pixelMax
			}
			if pixelMax > scene.Tolerances.PreviewPixel {
				e := newError(CodeDiverge, "last preview diverges from exact frame at first pixel")
				e.Pixel = &PixelCoord{X: x, Y: y}
				e.Differences = deltas
				e.Value = map[string]any{
					"radiance_deltas":     deltas,
					"preview_radiance":    pp.Radiance,
					"exact_radiance":      ep.Radiance,
					"transmittance_delta": tileAggregateDelta(preview.Tiles, exact.Tiles, x, y, "transmittance"),
					"scattering_delta":    tileAggregateDelta(preview.Tiles, exact.Tiles, x, y, "scattering"),
					"pixel_limit":         scene.Tolerances.PreviewPixel,
				}
				return maxDelta, e
			}
		}
	}
	return maxDelta, nil
}

func tileAggregateDelta(previewTiles, exactTiles []TileResult, x, y int, field string) float64 {
	tile := TileCoord{X: x / 8, Y: y / 6}
	for _, candidate := range exactTiles {
		if candidate.Tile != tile {
			continue
		}
		exactValue := candidate.Transmittance
		if field == "scattering" {
			exactValue = candidate.Scattering
		}
		for _, previewTile := range previewTiles {
			if previewTile.Tile == tile {
				previewValue := previewTile.Transmittance
				if field == "scattering" {
					previewValue = previewTile.Scattering
				}
				return floatAbs(previewValue - exactValue)
			}
		}
		_ = exactValue
	}
	return 0
}
