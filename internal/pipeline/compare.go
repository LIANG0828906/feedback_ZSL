package pipeline

import (
	"math"

	"volumekernel/internal/model"
)

type CompareReport struct {
	MaxAbsError       float64 `json:"max_abs_error"`
	MeanAbsError      float64 `json:"mean_abs_error"`
	OverToleranceRate float64 `json:"over_tolerance_rate"`
	Tolerance         float64 `json:"tolerance"`
	PixelsCompared    int     `json:"pixels_compared"`
	FirstPixelX       int     `json:"first_pixel_x"`
	FirstPixelY       int     `json:"first_pixel_y"`
}

func CompareFrames(preview, exact model.Frame, tolerance float64) (CompareReport, error) {
	report := CompareReport{Tolerance: tolerance, FirstPixelX: -1, FirstPixelY: -1}
	if preview.SnapshotVersion != exact.SnapshotVersion {
		return report, diverge("snapshot version mismatch", preview, nil)
	}
	if preview.CameraVersion != exact.CameraVersion {
		return report, diverge("camera version mismatch", preview, nil)
	}
	if preview.RecipeVersion != exact.RecipeVersion || preview.AlgorithmVersion != exact.AlgorithmVersion {
		return report, diverge("recipe or algorithm version mismatch", preview, nil)
	}
	if preview.Viewport != exact.Viewport {
		return report, diverge("viewport mismatch", preview, nil)
	}
	if len(preview.Pixels) != len(exact.Pixels) || len(preview.Pixels) == 0 {
		return report, diverge("pixel count mismatch", preview, nil)
	}
	over := 0
	sum := 0.0
	for i := range preview.Pixels {
		a, b := preview.Pixels[i], exact.Pixels[i]
		if a.X != b.X || a.Y != b.Y {
			return report, diverge("pixel ordering mismatch", preview, &a)
		}
		d := math.Abs(a.ScatteredRadiance - b.ScatteredRadiance)
		dt := math.Abs(a.Transmittance - b.Transmittance)
		if dt > d {
			d = dt
		}
		sum += d
		if d > report.MaxAbsError {
			report.MaxAbsError = d
		}
		if d > tolerance {
			if over == 0 {
				report.FirstPixelX, report.FirstPixelY = a.X, a.Y
			}
			over++
		}
	}
	report.PixelsCompared = len(preview.Pixels)
	report.MeanAbsError = sum / float64(report.PixelsCompared)
	report.OverToleranceRate = float64(over) / float64(report.PixelsCompared)
	if over > 0 {
		measures := map[string]float64{
			"max_abs_error":       report.MaxAbsError,
			"mean_abs_error":      report.MeanAbsError,
			"over_tolerance_rate": report.OverToleranceRate,
		}
		e := model.NewError(model.EDiverge, "preview and exact renders diverge")
		e.FirstPixelX, e.FirstPixelY = report.FirstPixelX, report.FirstPixelY
		e.Measurements = measures
		return report, e
	}
	return report, nil
}

func diverge(msg string, frame model.Frame, p *model.PixelResult) error {
	e := model.NewError(model.EDiverge, "%s", msg)
	e.BlockX, e.BlockY = -1, -1
	if p != nil {
		e.FirstPixelX, e.FirstPixelY = p.X, p.Y
	}
	e.Measurements = map[string]float64{
		"max_abs_error":       0,
		"mean_abs_error":      0,
		"over_tolerance_rate": 0,
	}
	return e
}

func FramesEqualPixels(a, b model.Frame) bool {
	if len(a.Pixels) != len(b.Pixels) {
		return false
	}
	for i := range a.Pixels {
		if a.Pixels[i].X != b.Pixels[i].X || a.Pixels[i].Y != b.Pixels[i].Y ||
			a.Pixels[i].Transmittance != b.Pixels[i].Transmittance ||
			a.Pixels[i].ScatteredRadiance != b.Pixels[i].ScatteredRadiance {
			return false
		}
	}
	return true
}
