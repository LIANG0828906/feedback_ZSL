package engine

import (
	"context"
	"fmt"
	"math"

	"volumediag/internal/diag"
	"volumediag/internal/volume"
)

func (e *Engine) VerifyTruth(ctx context.Context) (*TruthReport, error) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	e.mu.RLock()
	cache := make(map[volume.BlockKey]*volume.BlockResult, len(e.cache))
	for k, v := range e.cache {
		cache[k] = v
	}
	scene := e.scene
	params := e.params
	cameraIndex := e.cameraIndex
	truth := e.bundle.Truth
	e.mu.RUnlock()
	report := &TruthReport{
		PixelTolerance:   truth.Tolerance.PixelAbs,
		ScatterTolerance: truth.Tolerance.ScatterAbs,
	}
	if len(cache) != scene.BlockRows()*scene.BlockCols() {
		return report, diag.New(diag.EDep, "cache is incomplete; render before truth verification")
	}
	for _, pixelTruth := range truth.Pixels {
		key := volume.BlockKey{
			CameraIndex: cameraIndex,
			CameraID:    scene.CameraPath[cameraIndex].ID,
			X:           pixelTruth.X / scene.BlockSize.Width,
			Y:           pixelTruth.Y / scene.BlockSize.Height,
		}
		block := cache[key]
		if block == nil {
			return report, diag.Dependency(scene.CameraPath[cameraIndex].ID, key.X, key.Y, "truth pixel block is not cached")
		}
		var pixel *volume.PixelResult
		for i := range block.Pixels {
			if block.Pixels[i].X == pixelTruth.X && block.Pixels[i].Y == pixelTruth.Y {
				pixel = &block.Pixels[i]
				break
			}
		}
		if pixel == nil {
			return report, diag.Dependency(scene.CameraPath[cameraIndex].ID, key.X, key.Y, "truth pixel is absent from cached block")
		}
		report.PixelsCompared++
		for c := 0; c < 3; c++ {
			diff := math.Abs(pixel.Volume[c] - pixelTruth.Volume[c])
			if diff > report.MaxPixelError {
				report.MaxPixelError = diff
			}
			tol := truth.Tolerance.PixelAbs + truth.Tolerance.Relative*math.Max(math.Abs(pixel.Volume[c]), math.Abs(pixelTruth.Volume[c]))
			if diff > tol {
				return report, &diag.Error{
					Code:     diag.EDiff,
					CameraID: scene.CameraPath[cameraIndex].ID,
					BlockX:   key.X,
					BlockY:   key.Y,
					PixelX:   pixelTruth.X,
					PixelY:   pixelTruth.Y,
					Item:     "truth_pixel_volume",
					Left:     fmt.Sprintf("%.17g", pixel.Volume[c]),
					Right:    fmt.Sprintf("%.17g", pixelTruth.Volume[c]),
					Message:  fmt.Sprintf("truth mismatch exceeds %.6g (actual %.6g)", tol, diff),
				}
			}
		}
	}
	for _, rayTruth := range truth.Rays {
		if err := ctx.Err(); err != nil {
			return report, &diag.Error{Code: diag.ECanceled, Message: "truth verification canceled", Cause: err}
		}
		rendered, renderErr := volume.RenderRayPublic(ctx, scene, cameraIndex, rayTruth.Origin, rayTruth.Direction,
			rayTruth.TNear, rayTruth.TFar, rayTruth.PixelX, rayTruth.PixelY, params)
		if renderErr != nil {
			return report, renderErr
		}
		report.RaysCompared++
		for c := 0; c < 3; c++ {
			incoming := math.Abs(rendered.Transmittance[c] - rayTruth.Transmittance[c])
			if incoming > report.MaxTransmittanceError {
				report.MaxTransmittanceError = incoming
			}
			tolT := truth.Tolerance.TransmittanceAbs + truth.Tolerance.Relative*math.Max(math.Abs(rendered.Transmittance[c]), math.Abs(rayTruth.Transmittance[c]))
			if incoming > tolT {
				return report, &diag.Error{
					Code:     diag.EDiff,
					CameraID: rayTruth.CameraID,
					PixelX:   rayTruth.PixelX,
					PixelY:   rayTruth.PixelY,
					Item:     "truth_ray_transmittance",
					Left:     fmt.Sprintf("%.17g", rendered.Transmittance[c]),
					Right:    fmt.Sprintf("%.17g", rayTruth.Transmittance[c]),
					Message:  fmt.Sprintf("ray transmittance mismatch for %s exceeds %.6g", rayTruth.ID, tolT),
				}
			}
			scatter := math.Abs(rendered.Scatter[c] - rayTruth.Scatter[c])
			if scatter > report.MaxScatterError {
				report.MaxScatterError = scatter
			}
			tolS := truth.Tolerance.ScatterAbs + truth.Tolerance.Relative*math.Max(math.Abs(rendered.Scatter[c]), math.Abs(rayTruth.Scatter[c]))
			if scatter > tolS {
				return report, &diag.Error{
					Code:     diag.EDiff,
					CameraID: rayTruth.CameraID,
					PixelX:   rayTruth.PixelX,
					PixelY:   rayTruth.PixelY,
					Item:     "truth_ray_scatter",
					Left:     fmt.Sprintf("%.17g", rendered.Scatter[c]),
					Right:    fmt.Sprintf("%.17g", rayTruth.Scatter[c]),
					Message:  fmt.Sprintf("ray scatter mismatch for %s exceeds %.6g (actual %.6g)", rayTruth.ID, tolS, scatter),
				}
			}
		}
	}
	report.Equal = true
	e.lastTruth = report
	return report, nil
}

func (e *Engine) LastTruthReport() *TruthReport {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lastTruth
}
