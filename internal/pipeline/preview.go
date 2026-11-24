package pipeline

import (
	"context"
	"image"
	"time"

	"maskpipe/internal/model"
	"maskpipe/internal/sample"
	"maskpipe/internal/vision"
)

type PreviewBasis struct {
	OriginalWidth     int       `json:"original_width"`
	OriginalHeight    int       `json:"original_height"`
	OriginalPixels    int       `json:"original_pixels"`
	TargetPixels      int       `json:"target_pixels"`
	CandidateScales   []float64 `json:"candidate_scales"`
	SelectedScale     float64   `json:"selected_scale"`
	FullIterations    int       `json:"full_iterations"`
	PreviewIterations int       `json:"preview_iterations"`
	Rule              string    `json:"rule"`
}

type PreviewResult struct {
	Scale             float64            `json:"scale"`
	DownsampledWidth  int                `json:"downsampled_width"`
	DownsampledHeight int                `json:"downsampled_height"`
	Basis             PreviewBasis       `json:"basis"`
	ElapsedMS         int64              `json:"elapsed_ms"`
	LatencyBudgetMS   int                `json:"latency_budget_ms"`
	WithinLatency     bool               `json:"within_latency"`
	StatsPlaceholder  bool               `json:"stats_placeholder"`
	Mask              []byte             `json:"-"`
	LowMask           []byte             `json:"-"`
	Regions           []model.Region     `json:"regions"`
	Params            model.ParameterSet `json:"params"`
	Comparison        *DivergenceReport  `json:"comparison,omitempty"`
}

func ChoosePreviewScale(width, height, fullIterations int) PreviewBasis {
	pixels := width * height
	target := 12288
	scales := []float64{1, 0.75, 0.5, 0.25}
	selected := 0.25
	for _, candidate := range scales {
		sw := maxInt(1, int(float64(width)*candidate+0.5))
		sh := maxInt(1, int(float64(height)*candidate+0.5))
		if sw*sh <= target {
			selected = candidate
			break
		}
	}
	previewIterations := 1024
	if fullIterations < previewIterations {
		previewIterations = fullIterations
	}
	return PreviewBasis{
		OriginalWidth: width, OriginalHeight: height, OriginalPixels: pixels,
		TargetPixels: target, CandidateScales: scales, SelectedScale: selected,
		FullIterations: fullIterations, PreviewIterations: previewIterations,
		Rule: "choose largest fixed interactive scale whose sampled pixel count is <= target_pixels; cap FIFO visits",
	}
}

func DownsampleNRGBA(src *image.NRGBA, scale float64) *image.NRGBA {
	b := src.Bounds()
	sw := maxInt(1, int(float64(b.Dx())*scale+0.5))
	sh := maxInt(1, int(float64(b.Dy())*scale+0.5))
	dst := image.NewNRGBA(image.Rect(0, 0, sw, sh))
	for y := 0; y < sh; y++ {
		sy0 := int(float64(y) / scale)
		sy1 := int(float64(y+1) / scale)
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for x := 0; x < sw; x++ {
			sx0 := int(float64(x) / scale)
			sx1 := int(float64(x+1) / scale)
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var sr, sg, sb, n int
			for sy := sy0; sy < sy1 && sy < b.Dy(); sy++ {
				for sx := sx0; sx < sx1 && sx < b.Dx(); sx++ {
					i := src.PixOffset(b.Min.X+sx, b.Min.Y+sy)
					sr += int(src.Pix[i])
					sg += int(src.Pix[i+1])
					sb += int(src.Pix[i+2])
					n++
				}
			}
			if n == 0 {
				n = 1
			}
			di := dst.PixOffset(x, y)
			dst.Pix[di] = uint8(sr / n)
			dst.Pix[di+1] = uint8(sg / n)
			dst.Pix[di+2] = uint8(sb / n)
			dst.Pix[di+3] = 255
		}
	}
	return dst
}

func UpscaleNearest(mask []byte, lowW, lowH, fullW, fullH int) []byte {
	out := make([]byte, fullW*fullH)
	for y := 0; y < fullH; y++ {
		ly := minInt(lowH-1, int(float64(y)*float64(lowH)/float64(fullH)))
		for x := 0; x < fullW; x++ {
			lx := minInt(lowW-1, int(float64(x)*float64(lowW)/float64(fullW)))
			out[y*fullW+x] = mask[ly*lowW+lx]
		}
	}
	return out
}

func RunPreview(_ context.Context, loaded *sample.LoadedScene, params model.ParameterSet) (*PreviewResult, error) {
	start := time.Now()
	scene := loaded.Scene
	basis := ChoosePreviewScale(scene.Width, scene.Height, params.MaxIterations)
	small := DownsampleNRGBA(loaded.Image, basis.SelectedScale)
	previewParams := params
	previewParams.MaxIterations = basis.PreviewIterations
	previewParams.ParamVersion = params.ParamVersion
	smallSeeds := make([]model.IntPoint, 0, len(params.Seeds))
	for _, seed := range params.Seeds {
		smallSeeds = append(smallSeeds, model.IntPoint{
			X: int(float64(seed.X) * basis.SelectedScale),
			Y: int(float64(seed.Y) * basis.SelectedScale),
		})
	}
	previewParams.Seeds = smallSeeds
	rect := model.BBox{MinX: 0, MinY: 0, MaxX: small.Bounds().Dx() - 1, MaxY: small.Bounds().Dy() - 1,
		Width: small.Bounds().Dx(), Height: small.Bounds().Dy()}
	raw := vision.RegionGrowTile(small, smallSeeds, rect, previewParams)
	thresholded := vision.ThresholdTile(small, raw, smallSeeds, rect, previewParams)
	lowMask := vision.MorphClean(thresholded, rect.Width, rect.Height, previewParams.StructureSize)
	fullMask := UpscaleNearest(lowMask, rect.Width, rect.Height, scene.Width, scene.Height)
	regions := vision.AnalyzeMask(fullMask, scene.Width, scene.Height, params.MinRegionArea)
	elapsed := time.Since(start).Milliseconds()
	return &PreviewResult{
		Scale: basis.SelectedScale, DownsampledWidth: rect.Width, DownsampledHeight: rect.Height,
		Basis: basis, ElapsedMS: elapsed, LatencyBudgetMS: scene.Declared.PreviewLatencyMS,
		WithinLatency:    elapsed <= int64(scene.Declared.PreviewLatencyMS),
		StatsPlaceholder: true, Mask: fullMask, LowMask: lowMask, Regions: regions,
		Params: previewParams,
	}, nil
}
