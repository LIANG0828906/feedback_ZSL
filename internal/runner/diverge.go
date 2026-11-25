package runner

import (
	"fmt"

	"edgerefine/internal/errs"
	"edgerefine/internal/model"
)

func ComparePreviewExact(preview, exact *model.RunReport, p model.Params) (*model.DivergenceReport, error) {
	rep := &model.DivergenceReport{Pass: true, FirstTile: [2]int{-1, -1}, DiffPixel: [2]int{-1, -1}}
	if preview == nil || exact == nil || preview.Accuracy == nil || exact.Accuracy == nil {
		return nil, errs.New(errs.EDiverge, "preview or exact accuracy report is missing")
	}
	rep.MedianPreview = preview.Accuracy.Median
	rep.MedianExact = exact.Accuracy.Median
	rep.MedianDifference = absFloat(preview.Accuracy.Median - exact.Accuracy.Median)
	rep.ConvergePreview = preview.ConvergedTileRatio
	rep.ConvergeExact = exact.ConvergedTileRatio
	rep.ConvergeDifference = absFloat(preview.ConvergedTileRatio - exact.ConvergedTileRatio)

	totalPixels := 0
	diffPixels := 0
	found := false
	for i := 0; i < len(exact.Tiles) && i < len(preview.Tiles); i++ {
		et := exact.Tiles[i]
		pt := preview.Tiles[i]
		w, h := et.Rect.W, et.Rect.H
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				idx := y*w + x
				totalPixels++
				var pv byte
				if idx < len(pt.RefinedMask) {
					pv = pt.RefinedMask[idx]
				}
				if idx >= len(et.RefinedMask) || pv != et.RefinedMask[idx] {
					diffPixels++
					if !found {
						found = true
						rep.FirstTile = et.Tile.Array()
						rep.DiffPixel = [2]int{et.Rect.X + x, et.Rect.Y + y}
					}
				}
			}
		}
	}
	if len(preview.Tiles) != len(exact.Tiles) {
		rep.FirstTile = [2]int{-1, -1}
	}
	if totalPixels > 0 {
		rep.PixelDifferenceRatio = float64(diffPixels) / float64(totalPixels)
	}

	if rep.MedianDifference > p.PreviewMedianTol ||
		rep.ConvergeDifference > p.ConvergeRatioTol ||
		rep.PixelDifferenceRatio > p.PreviewPixelTol {
		rep.Pass = false
		return rep, &errs.Error{
			Code:    errs.EDiverge,
			Message: fmt.Sprintf("preview/exact divergence: medianΔ=%.4f convergenceΔ=%.4f pixelRatio=%.4f", rep.MedianDifference, rep.ConvergeDifference, rep.PixelDifferenceRatio),
			Violation: errs.Violation{
				Measured:   []float64{rep.MedianDifference, rep.ConvergeDifference, rep.PixelDifferenceRatio},
				Tolerances: []float64{p.PreviewMedianTol, p.ConvergeRatioTol, p.PreviewPixelTol},
				FirstTile:  rep.FirstTile,
				DiffPixel:  rep.DiffPixel,
			},
		}
	}
	return rep, nil
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
